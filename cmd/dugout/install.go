package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

// version is overridable with -ldflags "-X main.version=...".
var version = "0.9.1"

// bootstrapTries bounds the bootstraps install tries: right after a bootout
// launchd may refuse one (exit 5) while the old job is still going away.
const bootstrapTries = 3

var bootstrapDelay = 500 * time.Millisecond // between tries; a var so a test can drop it

// goos is a var so a test can take the Linux paths on a Mac.
var goos = runtime.GOOS

const notSupported = "dugout install only works on macOS and on Linux with systemd"

// supported reports whether install knows how to keep the daemon running
// here: a LaunchAgent on macOS, a systemd --user unit on Linux.
func supported() bool { return goos == "darwin" || goos == "linux" }

func cmdVersion(stdout io.Writer) int {
	fmt.Fprintln(stdout, "dugout", version)
	return 0
}

// binLink is where install links the binary so the plugin's dugout-run finds it
// even when Claude Code was opened without ~/go/bin in its PATH: bin/ of the
// default state dir, which dugout-run knows.
func binLink(home string) string {
	return filepath.Join(statedir.DefaultDir(goos, home, os.Getenv), "bin", "dugout")
}

// daemonLog is the file the installed daemon logs to.
func daemonLog(home string) string {
	if goos == "linux" {
		return systemd.LogPath(statedir.DefaultDir(goos, home, os.Getenv))
	}
	return launchd.LogPath(home)
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", args[0], err, out)
	}
	return nil
}

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", "", "URL of dugout serve")
	insecure := flags.Bool("insecure", false, "allow an http:// server outside this machine: the token travels in the clear")
	dryHelp := "print the plist without installing anything"
	if goos == "linux" {
		dryHelp = "print the systemd unit without installing anything"
	}
	dry := flags.Bool("dry-run", false, dryHelp)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *server == "" {
		fmt.Fprintln(stderr, "dugout install: missing -server")
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if wire.InsecureServer(*server) && !*insecure {
		fmt.Fprintf(stderr, "dugout install: %s: %v\n", *server, wire.ErrInsecureServer)
		return 2
	}
	if !supported() {
		fmt.Fprintln(stderr, notSupported)
		return 1
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(stderr, "dugout install:", err)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "dugout install:", err)
		return 1
	}
	if goos == "linux" {
		return installUnit(exe, *server, home, *insecure, *dry, stdout, stderr)
	}
	// The daemon gets this shell's PATH, so it runs the git the person runs.
	plist := launchd.Plist(exe, *server, home, os.Getenv("PATH"), *insecure)
	if *dry {
		stdout.Write(plist)
		return 0
	}
	path := launchd.PlistPath(home)
	link := binLink(home)
	if err := installFiles(path, plist, link, exe, launchd.LogPath(home)); err != nil {
		fmt.Fprintln(stderr, "dugout install:", err)
		return 1
	}
	target := fmt.Sprintf("gui/%d", os.Getuid())
	_ = launchctl("bootout", target+"/"+launchd.Label) // not loaded yet is fine
	if err := bootstrap(target, path); err != nil {
		fmt.Fprintln(stderr, "dugout install:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Dugout installed: the daemon starts with your session and talks to %s.\n", *server)
	if wire.InsecureServer(*server) {
		fmt.Fprintf(stdout, "Warning: %s has no TLS; the token and the bundles travel in the clear.\n", *server)
	}
	return 0
}

// bootstrap loads the plist into target, retrying, and returns the last error.
func bootstrap(target, path string) error {
	var err error
	for i := range bootstrapTries {
		if i > 0 {
			time.Sleep(bootstrapDelay)
		}
		if err = launchctl("bootstrap", target, path); err == nil {
			return nil
		}
	}
	return err
}

// installFiles writes the plist, creates the log directory and points the bin
// link at exe, replacing whatever link was there.
func installFiles(path string, plist []byte, link, exe, logPath string) error {
	for _, dir := range []string{filepath.Dir(path), filepath.Dir(logPath), filepath.Dir(link)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, plist, 0o644); err != nil {
		return err
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Symlink(exe, link)
}

func cmdUninstall(stdout, stderr io.Writer) int {
	if !supported() {
		fmt.Fprintln(stderr, notSupported)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "dugout uninstall:", err)
		return 1
	}
	if goos == "linux" {
		return uninstallUnit(home, stdout, stderr)
	}
	_ = launchctl("bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchd.Label))
	_ = os.Remove(binLink(home)) // best effort: it may never have existed
	if err := os.Remove(launchd.PlistPath(home)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(stdout, "Dugout was not installed.")
			return 0
		}
		fmt.Fprintln(stderr, "dugout uninstall:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Dugout uninstalled.")
	return 0
}
