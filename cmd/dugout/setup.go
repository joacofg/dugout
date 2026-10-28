package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/keychain"
	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/peers"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	defaultMarketplace = "joacofg/dugout"
	marketplaceName    = "dugout"
	pluginID           = "dugout@dugout"
	claudeTimeout      = 2 * time.Minute // a marketplace from GitHub is a clone
	setupRecordVersion = 1
)

// socketWait is how long setup waits for the daemon it just installed to
// answer; a var so a test can shorten it.
var socketWait = 10 * time.Second

// setupOpts are the flags setup and join share.
type setupOpts struct {
	server, mode, marketplace string
	insecure                  bool
}

func (o *setupOpts) register(flags *flag.FlagSet, withServer bool) {
	if withServer {
		flags.StringVar(&o.server, "server", "", "URL of dugout serve, or off for this machine only (default: the installed daemon's, else $"+envServer+", else off)")
	}
	flags.BoolVar(&o.insecure, "insecure", false, "allow an http:// server outside this machine: the token travels in the clear")
	flags.StringVar(&o.mode, "mode", "", "autonomy mode for this repo: auto, scoped or supervised (without it, init asks)")
	flags.StringVar(&o.marketplace, "marketplace", defaultMarketplace, "where Claude Code gets the plugin: a GitHub repo, a URL or a path")
}

// cmdSetup is the one command that leaves Dugout working: it installs the
// daemon service, adds the Claude Code plugin, runs init in this repo and
// ends with doctor. Every file it edits outside Dugout's own state dir is
// backed up first, and --remove undoes it all.
func cmdSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o setupOpts
	o.register(flags, true)
	remove := flags.Bool("remove", false, "undo dugout setup: remove the service and the plugin and restore every file it edited")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if *remove {
		return setupRemove(stdout, stderr)
	}
	return runSetup(o, stdin, stdout, stderr)
}

// setupRecord is what setup leaves in the state dir for --remove: each file
// it was about to edit, as it was before the first setup, and the plugin
// pieces it added that were not there.
type setupRecord struct {
	Version int         `json:"version"`
	Files   []savedFile `json:"files"`
	// Dirs are directories setup created (the plugin's copies), which
	// remove deletes whole.
	Dirs []string `json:"dirs,omitempty"`
	// Parents are the directories that did not exist above a kept file or
	// dir, which remove deletes when they are left empty.
	Parents []string `json:"parents,omitempty"`
	// Marketplace and Plugin say whether setup added them to Claude Code, so
	// remove takes out only what it put in.
	Marketplace bool `json:"marketplace,omitempty"`
	Plugin      bool `json:"plugin,omitempty"`
}

// savedFile is one file as it was before setup touched it.
type savedFile struct {
	Path    string      `json:"path"`
	Existed bool        `json:"existed"`
	Link    string      `json:"link,omitempty"`   // it was a symlink to this
	Mode    fs.FileMode `json:"mode,omitempty"`   // its permissions
	Backup  string      `json:"backup,omitempty"` // a copy of its bytes
	// Left is the fingerprint of what setup left there. Remove restores the
	// file only while it still has it, so an edit made since is never lost.
	Left string `json:"left"`
}

func setupDir(state string) string { return filepath.Join(state, "setup") }

func recordPath(state string) string { return filepath.Join(setupDir(state), "record.json") }

func loadRecord(state string) (*setupRecord, bool, error) {
	raw, err := os.ReadFile(recordPath(state))
	if errors.Is(err, fs.ErrNotExist) {
		return &setupRecord{Version: setupRecordVersion}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var r setupRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, false, fmt.Errorf("unreadable %s: %w", recordPath(state), err)
	}
	if r.Version != setupRecordVersion {
		return nil, false, fmt.Errorf("%s: version %d not supported", recordPath(state), r.Version)
	}
	return &r, true, nil
}

func (r *setupRecord) save(state string) error {
	if err := os.MkdirAll(setupDir(state), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	return statedir.WriteAtomic(recordPath(state), append(b, '\n'))
}

// keep backs up each path before setup edits it. A path already kept, by
// this setup or an earlier one, keeps its first backup: that is the one from
// before Dugout.
func (r *setupRecord) keep(state string, paths ...string) error {
	for _, p := range paths {
		if slices.ContainsFunc(r.Files, func(f savedFile) bool { return f.Path == p }) {
			continue
		}
		f := savedFile{Path: p}
		st, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case st.Mode()&fs.ModeSymlink != 0:
			if f.Link, err = os.Readlink(p); err != nil {
				return err
			}
			f.Existed = true
		case st.Mode().IsRegular():
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(setupDir(state), "backups"), 0o700); err != nil {
				return err
			}
			f.Backup = filepath.Join(setupDir(state), "backups", fmt.Sprintf("%d-%s", len(r.Files), filepath.Base(p)))
			if err := os.WriteFile(f.Backup, raw, 0o600); err != nil {
				return err
			}
			f.Existed, f.Mode = true, st.Mode().Perm()
		default:
			return fmt.Errorf("%s is not a file setup can back up", p)
		}
		f.Left = fingerprint(p)
		r.Files = append(r.Files, f)
		r.keepParents(p)
	}
	return r.save(state)
}

// keepParents notes the directories above path that do not exist yet.
func (r *setupRecord) keepParents(path string) {
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Lstat(dir); err == nil {
			return
		}
		if !slices.Contains(r.Parents, dir) {
			r.Parents = append(r.Parents, dir)
		}
	}
}

// keepDirs notes the directories that do not exist yet, for remove to delete.
func (r *setupRecord) keepDirs(state string, dirs ...string) error {
	for _, d := range dirs {
		if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) && !slices.Contains(r.Dirs, d) {
			r.Dirs = append(r.Dirs, d)
			r.keepParents(d)
		}
	}
	return r.save(state)
}

// settle notes what setup left in every file it kept.
func (r *setupRecord) settle(state string) error {
	for i := range r.Files {
		r.Files[i].Left = fingerprint(r.Files[i].Path)
	}
	return r.save(state)
}

// fingerprint tells a file's content apart: "none" when missing, its link
// target for a symlink, its sha256 otherwise.
func fingerprint(path string) string {
	st, err := os.Lstat(path)
	if err != nil {
		return "none"
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		return "link:" + target
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// restore puts f back as it was before setup.
func (f savedFile) restore() error {
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	switch {
	case !f.Existed:
		return nil
	case f.Link != "":
		return os.Symlink(f.Link, f.Path)
	}
	raw, err := os.ReadFile(f.Backup)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(f.Path, raw, f.Mode); err != nil {
		return err
	}
	return os.Chmod(f.Path, f.Mode) // WriteFile's mode goes through the umask
}

// servicePath is the LaunchAgent, or the systemd unit on Linux.
func servicePath(home string) string {
	if goos == "linux" {
		return systemd.UnitPath(home, os.Getenv)
	}
	return launchd.PlistPath(home)
}

// installedArgs is the command line of the daemon already installed, nil if
// none.
func installedArgs(home string) []string {
	raw, err := os.ReadFile(servicePath(home))
	if err != nil {
		return nil
	}
	if goos == "linux" {
		s, err := systemd.Parse(raw)
		if err != nil {
			return nil
		}
		return s.Args
	}
	p, err := parsePlist(raw)
	if err != nil {
		return nil
	}
	return p.args
}

// installedServer is the -server of the daemon already installed, "" if none.
func installedServer(home string) string {
	return argAfter(installedArgs(home), "-server")
}

// claudeFiles are the files `claude plugin` edits in the Claude Code config
// dir, and the directories where it copies the marketplace and the plugin.
// .claude.json is not among them: Claude Code rewrites it all the time, so
// remove undoes its entries through the claude CLI instead of restoring it.
func claudeFiles(home string) (files, dirs []string) {
	cfg := peers.Dir(os.Getenv, home)
	files = []string{
		filepath.Join(cfg, "settings.json"),
		filepath.Join(cfg, "plugins", "known_marketplaces.json"),
		filepath.Join(cfg, "plugins", "installed_plugins.json"),
	}
	dirs = []string{
		filepath.Join(cfg, "plugins", "marketplaces", marketplaceName),
		filepath.Join(cfg, "plugins", "cache", marketplaceName),
	}
	return files, dirs
}

// claudeCmd runs `claude plugin args...` and returns its combined output.
func claudeCmd(claude string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, claude, append([]string{"plugin"}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("claude plugin %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// claudeHas reports whether Claude Code already has the dugout marketplace
// and the plugin installed (outside a session-only --plugin-dir).
func claudeHas(claude string) (marketplace, plugin bool) {
	if out, err := claudeCmd(claude, "marketplace", "list", "--json"); err == nil {
		var ms []struct{ Name string }
		if json.Unmarshal([]byte(out), &ms) == nil {
			marketplace = slices.ContainsFunc(ms, func(m struct{ Name string }) bool { return m.Name == marketplaceName })
		}
	}
	if out, err := claudeCmd(claude, "list", "--json"); err == nil {
		var ps []struct{ ID, Scope string }
		if json.Unmarshal([]byte(out), &ps) == nil {
			plugin = slices.ContainsFunc(ps, func(p struct{ ID, Scope string }) bool { return p.ID == pluginID && p.Scope != "session" })
		}
	}
	return marketplace, plugin
}

// waitDaemon waits for the daemon on sock to answer, as doctor asks it.
func waitDaemon(sock string) error {
	deadline := time.Now().Add(socketWait)
	for {
		_, err := ask(sock, wire.Request{Type: wire.TypeHook, Event: "SessionEnd"})
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// gitRoot is the top of the worktree holding dir, "" outside one.
func gitRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runSetup(o setupOpts, stdin io.Reader, stdout, stderr io.Writer) int {
	if !supported() {
		fmt.Fprintln(stderr, "dugout setup only works on macOS and on Linux with systemd")
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "dugout setup:", err); return 1 }
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	state, err := statedir.Dir()
	if err == nil {
		err = statedir.Ensure(state)
	}
	if err != nil {
		return fail(err)
	}
	rec, _, err := loadRecord(state)
	if err != nil {
		return fail(err)
	}
	server := o.server
	for _, s := range []string{installedServer(home), os.Getenv(envServer), "off"} {
		if server == "" {
			server = s
		}
	}
	if server != "off" {
		server = keychainAccount(server)
	}
	// An installed daemon that was given -insecure keeps it for its own server.
	insecure := o.insecure || (server != "off" && keychainAccount(installedServer(home)) == server && slices.Contains(installedArgs(home), "-insecure"))
	var serverFlags []string
	if insecure {
		serverFlags = []string{"-insecure"}
	}

	fmt.Fprintln(stdout, "1/4 The daemon")
	if server != "off" && !wire.IsLoopbackURL(server) {
		if _, err := keychain.Get(server); errors.Is(err, keychain.ErrNotFound) && interactive(stdin) {
			if code := cmdLogin(append(serverFlags, "-server", server), stdin, stdout, stderr); code != 0 {
				return code
			}
		}
	}
	if err := rec.keep(state, servicePath(home), binLink(home)); err != nil {
		return fail(err)
	}
	if code := cmdInstall(append(serverFlags, "-server", server), stdout, stderr); code != 0 {
		_ = rec.settle(state)
		return code
	}
	sock, err := statedir.SocketPath()
	if err == nil {
		err = waitDaemon(sock)
	}
	if err != nil {
		_ = rec.settle(state)
		return fail(fmt.Errorf("the daemon did not answer within %s: check %s", socketWait, logHint()))
	}

	failed := false
	fmt.Fprintln(stdout, "2/4 The Claude Code plugin")
	if claude, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(stdout, "Claude Code is not on the PATH: install it, then run dugout setup again.")
	} else {
		files, dirs := claudeFiles(home)
		hadMarketplace, hadPlugin := claudeHas(claude)
		if err := rec.keep(state, files...); err != nil {
			return fail(err)
		}
		if err := rec.keepDirs(state, dirs...); err != nil {
			return fail(err)
		}
		rec.Marketplace = rec.Marketplace || !hadMarketplace
		rec.Plugin = rec.Plugin || !hadPlugin
		for _, args := range [][]string{{"marketplace", "add", o.marketplace}, {"install", pluginID}} {
			if !failed {
				if _, err := claudeCmd(claude, args...); err != nil {
					fmt.Fprintln(stderr, "dugout setup:", err)
					failed = true
				}
			}
		}
		if !failed {
			fmt.Fprintln(stdout, "The Claude Code plugin is installed: restart Claude Code, or run /reload-plugins in it, to load it.")
		}
	}

	fmt.Fprintln(stdout, "3/4 This repo")
	cwd, err := os.Getwd()
	root := ""
	if err == nil {
		root = gitRoot(cwd)
	}
	if root == "" {
		fmt.Fprintln(stdout, "Not in a git repo: run dugout init in each repo you work in.")
	} else {
		initArgs := []string{}
		if o.mode != "" {
			initArgs = append(initArgs, "-mode", o.mode)
		}
		if cmdInit(append(initArgs, root), stdin, stdout, stderr) != 0 {
			failed = true
		}
	}
	if err := rec.settle(state); err != nil {
		return fail(err)
	}

	fmt.Fprintln(stdout, "4/4 Check")
	code := runDoctor(stdout)
	fmt.Fprintln(stdout, "Undo all of this with dugout setup --remove.")
	if failed {
		return 1
	}
	return code
}

// setupRemove undoes setup: it takes out the plugin and the marketplace it
// added through the claude CLI, uninstalls the daemon service, and puts back
// every file it edited itself, byte for byte, unless something changed that file since: then it says so
// and keeps it, along with its backup. Dugout's own state dir stays.
func setupRemove(stdout, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "dugout setup --remove:", err); return 1 }
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	state, err := statedir.Dir()
	if err != nil {
		return fail(err)
	}
	rec, found, err := loadRecord(state)
	if err != nil {
		return fail(err)
	}
	if !found {
		fmt.Fprintln(stdout, "dugout setup has not run on this machine: nothing to undo. dugout uninstall removes a daemon installed by hand.")
		return 0
	}
	// What changed since setup is decided before the plugin and the service
	// go, since taking them out edits the same files.
	untouched := map[string]bool{}
	for _, f := range rec.Files {
		untouched[f.Path] = fingerprint(f.Path) == f.Left
	}
	failed := false
	if claude, err := exec.LookPath("claude"); err == nil {
		if rec.Plugin {
			if _, err := claudeCmd(claude, "uninstall", pluginID); err != nil {
				fmt.Fprintln(stderr, "dugout setup --remove:", err)
			}
		}
		if rec.Marketplace {
			if _, err := claudeCmd(claude, "marketplace", "remove", marketplaceName); err != nil {
				fmt.Fprintln(stderr, "dugout setup --remove:", err)
			}
		}
	}
	if code := cmdUninstall(stdout, stderr); code != 0 {
		failed = true
	}
	for _, d := range rec.Dirs {
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintln(stderr, "dugout setup --remove:", err)
			failed = true
		}
	}
	kept, service := false, false
	for _, f := range rec.Files {
		if !untouched[f.Path] {
			kept = true
			if f.Backup != "" {
				fmt.Fprintf(stdout, "Kept %s: it changed after dugout setup. Its original is in %s.\n", f.Path, f.Backup)
			} else {
				fmt.Fprintf(stdout, "Kept %s: it changed after dugout setup.\n", f.Path)
			}
			continue
		}
		if err := f.restore(); err != nil {
			fmt.Fprintln(stderr, "dugout setup --remove:", err)
			failed, kept = true, true
			continue
		}
		service = service || (f.Path == servicePath(home) && f.Existed)
	}
	// Deepest first, and only those left empty: os.Remove takes no other.
	parents := slices.Clone(rec.Parents)
	slices.SortFunc(parents, func(a, b string) int { return len(b) - len(a) })
	for _, d := range parents {
		_ = os.Remove(d)
	}
	if service {
		// The daemon installed before setup comes back as it was.
		if err := loadService(home); err != nil {
			fmt.Fprintln(stderr, "dugout setup --remove: the daemon you had before is back in place but did not start:", err)
			failed = true
		}
	}
	if !kept {
		if err := os.RemoveAll(setupDir(state)); err != nil {
			return fail(err)
		}
	} else if err := os.Remove(recordPath(state)); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Dugout setup removed. Dugout's own state stays in %s.\n", state)
	if failed {
		return 1
	}
	return 0
}

// loadService has launchd or systemd run the service file that is in place.
func loadService(home string) error {
	if goos == "linux" {
		for _, args := range [][]string{{"daemon-reload"}, {"enable", systemd.Unit}, {"restart", systemd.Unit}} {
			if err := systemctl(args...); err != nil {
				return err
			}
		}
		return nil
	}
	return bootstrap(fmt.Sprintf("gui/%d", os.Getuid()), launchd.PlistPath(home))
}
