package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

// systemctl runs `systemctl --user`, the Linux counterpart of launchctl.
func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

// unitEnv is the daemon's environment in the unit: this shell's PATH, so it
// runs the git the person runs, and its XDG_STATE_HOME when set, since the
// user's systemd never reads the shell's rc files: without it the daemon
// would keep its state, pause and token file in another dir than the
// commands run from this shell.
func unitEnv() [][2]string {
	env := [][2]string{{"PATH", os.Getenv("PATH")}}
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		env = append(env, [2]string{"XDG_STATE_HOME", x})
	}
	return env
}

// installUnit is install on Linux: it writes the systemd --user unit and the
// bin link, then has systemd load, enable and restart it. Restart rather than
// enable --now: like the bootout and bootstrap on a Mac, a second install
// restarts a running daemon so it takes the new binary, -server and token,
// and restart starts one that is not running.
func installUnit(exe, server, home string, insecure, dry bool, stdout, stderr io.Writer) int {
	state := statedir.DefaultDir(goos, home, os.Getenv)
	log := systemd.LogPath(state)
	unit := systemd.Render(exe, server, log, insecure, unitEnv())
	if dry {
		stdout.Write(unit)
		return 0
	}
	path := systemd.UnitPath(home, os.Getenv)
	err := statedir.Ensure(state)
	if err == nil {
		err = installFiles(path, unit, binLink(home), exe, log)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", systemd.Unit}, {"restart", systemd.Unit}} {
		if err == nil {
			err = systemctl(args...)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "dugout install:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Dugout installed: the daemon starts with your session and talks to %s.\n", server)
	if wire.InsecureServer(server) {
		fmt.Fprintf(stdout, "Warning: %s has no TLS; the token and the bundles travel in the clear.\n", server)
	}
	return 0
}

// systemdUnit is doctor's launchAgent on Linux: it checks the unit and reads
// back the -server and PATH it was installed with.
func (d *doctor) systemdUnit() agent {
	a := agent{kind: "systemd service", server: os.Getenv(envServer)}
	home, err := os.UserHomeDir()
	if err != nil {
		d.bad("could not locate your HOME: %v", err)
		return a
	}
	raw, err := os.ReadFile(systemd.UnitPath(home, os.Getenv))
	if errors.Is(err, fs.ErrNotExist) {
		d.bad("the daemon is not installed: run dugout install -server <url>")
		return a
	}
	s, perr := systemd.Parse(raw)
	if err != nil || perr != nil || len(s.Args) == 0 {
		d.bad("the systemd service is unreadable: run dugout install again")
		return a
	}
	a.installed, a.path = true, s.Env["PATH"]
	if v := argAfter(s.Args, "-server"); v != "" {
		a.server = v
	}
	a.insecure = slices.Contains(s.Args, "-insecure")
	switch {
	case !fileExists(s.Args[0]):
		d.bad("the systemd service points to a binary that no longer exists: run dugout install again")
	case systemctl("is-active", systemd.Unit) != nil:
		d.bad("the systemd service is not active: run dugout install again; if it stays the same, check systemctl --user status dugout")
	default:
		d.ok("the daemon is installed as a systemd --user service")
	}
	return a
}

// uninstallUnit is uninstall on Linux: it stops and disables the unit, which
// needs its file, then removes the file and the bin link and has systemd
// forget the unit.
func uninstallUnit(home string, stdout, stderr io.Writer) int {
	_ = systemctl("disable", "--now", systemd.Unit) // not loaded is fine
	_ = os.Remove(binLink(home))                    // best effort: it may never have existed
	if err := os.Remove(systemd.UnitPath(home, os.Getenv)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(stdout, "Dugout was not installed.")
			return 0
		}
		fmt.Fprintln(stderr, "dugout uninstall:", err)
		return 1
	}
	_ = systemctl("daemon-reload")
	fmt.Fprintln(stdout, "Dugout uninstalled.")
	return 0
}
