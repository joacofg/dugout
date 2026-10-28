package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

// fakeClaude is a claude whose `plugin` commands edit the config dir the way
// the real one does, roughly: the marketplace and the plugin land in
// settings.json and the plugins/ files, and their copies in plugins/; taking
// them out leaves those files edited, not as they were.
const fakeClaude = `#!/bin/sh
cfg=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
echo "$*" >>"$LOG"
case "$*" in
"plugin marketplace list --json")
  if grep -q dugout "$cfg/plugins/known_marketplaces.json" 2>/dev/null; then echo '[{"name":"dugout"}]'; else echo '[]'; fi ;;
"plugin list --json")
  if grep -q dugout@dugout "$cfg/plugins/installed_plugins.json" 2>/dev/null; then echo '[{"id":"dugout@dugout","scope":"user"}]'; else echo '[]'; fi ;;
"plugin list")
  grep -q dugout@dugout "$cfg/plugins/installed_plugins.json" 2>/dev/null && echo '  dugout@dugout' ;;
"plugin marketplace add "*)
  mkdir -p "$cfg/plugins/marketplaces/dugout"
  echo '{"dugout":{}}' >"$cfg/plugins/known_marketplaces.json"
  echo '{"extraKnownMarketplaces":{"dugout":{}}}' >"$cfg/settings.json" ;;
"plugin install dugout@dugout")
  mkdir -p "$cfg/plugins/cache/dugout/dugout/1"
  echo '{"plugins":{"dugout@dugout":[]}}' >"$cfg/plugins/installed_plugins.json"
  echo '{"enabledPlugins":{"dugout@dugout":true}}' >"$cfg/settings.json" ;;
"plugin uninstall dugout@dugout")
  echo '{"plugins":{}}' >"$cfg/plugins/installed_plugins.json"
  echo '{"enabledPlugins":{}}' >"$cfg/settings.json"
  touch "$cfg/plugins/cache/dugout/dugout/1/.orphaned_at" ;;
"plugin marketplace remove dugout")
  echo '{}' >"$cfg/plugins/known_marketplaces.json"
  rm -rf "$cfg/plugins/marketplaces/dugout" ;;
*) exit 3 ;;
esac
exit 0
`

// setupWorld is the sandbox setup runs in: a temp HOME that already has a
// Claude Code config, fake security, secret-tool, launchctl, systemctl and
// claude first on the PATH, a git repo, and a serverless daemon on its own
// state dir, outside HOME, since the fake launchctl starts none.
type setupWorld struct {
	home, state, repo, tools string
	secDir, store            string
	claudeLog, serviceLog    string
	env                      []string
}

func newSetupWorld(t *testing.T) *setupWorld {
	t.Helper()
	skipUnlessService(t)
	secDir, store, _ := fakeSecurity(t)
	tools := t.TempDir()
	logs := t.TempDir()
	w := &setupWorld{home: t.TempDir(), state: shortDir(t), repo: overlapRepo(t), tools: tools, secDir: secDir, store: store,
		claudeLog: filepath.Join(logs, "claude.log"), serviceLog: filepath.Join(logs, "service.log")}
	writeFakeTool(t, tools, "launchctl", w.serviceLog, "")
	writeFakeTool(t, tools, "systemctl", w.serviceLog, "")
	if err := os.WriteFile(filepath.Join(tools, "claude"), []byte(strings.Replace(fakeClaude, `"$LOG"`, w.claudeLog, 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	w.env = []string{"HOME=" + w.home, statedir.EnvHome + "=" + w.state, "PATH=" + secDir + ":" + tools + ":/bin:/usr/bin",
		envServer + "=", wire.EnvToken + "="}
	guardEnv(t, w.env, secDir)
	writeFile(t, filepath.Join(w.home, ".claude", "settings.json"), "{\n  \"theme\": \"dark\"\n}\n", 0o644)
	writeFile(t, filepath.Join(w.home, ".claude.json"), "{\"numStartups\": 3}\n", 0o600)
	startLocalDaemon(t, w.state)
	return w
}

func writeFile(t *testing.T, path, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// run runs the built dugout in the repo with the world's environment.
func (w *setupWorld) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = w.repo, w.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// servicePathIn is where install puts this platform's service under home in
// a test env without XDG vars.
func servicePathIn(home string) string {
	if runtime.GOOS == "linux" {
		return systemd.UnitPath(home, noEnv)
	}
	return launchd.PlistPath(home)
}

// treeOf is every file under root, symlinks as their target, with modes,
// and with dirs its directories too.
func treeOf(t *testing.T, root string, dirs bool) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			files[rel] = "link " + target
		case d.Type().IsRegular():
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[rel] = info.Mode().Perm().String() + " " + string(raw)
		case dirs && d.IsDir() && p != root:
			files[rel+"/"] = "dir"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// personFiles drops Dugout's own state dir and logs from a tree of
// HOME: setup leaves them, as uninstall does.
func personFiles(home string, snap map[string]string) map[string]string {
	state, _ := filepath.Rel(home, statedir.DefaultDir(runtime.GOOS, home, noEnv))
	logs, _ := filepath.Rel(home, filepath.Dir(serviceLog(home)))
	out := map[string]string{}
	for k, v := range snap {
		if !strings.HasPrefix(k, state+"/") && !strings.HasPrefix(k, logs+"/") {
			out[k] = v
		}
	}
	return out
}

// TestSetupAndRemove: setup installs the service, adds the plugin, tracks
// the repo and ends with a green doctor; run twice it keeps the first
// backups; --remove leaves every file of the person as it was before, byte
// for byte and mode for mode.
func TestSetupAndRemove(t *testing.T) {
	w := newSetupWorld(t)
	before := treeOf(t, filepath.Join(w.home, ".claude"), true)
	beforeHome := treeOf(t, w.home, false)
	for i := range 2 {
		out, errOut, code := w.run(t, "setup", "-mode", "scoped")
		if code != 0 {
			t.Fatalf("setup %d: exit %d\n%s\n%s", i, code, out, errOut)
		}
		wantLines(t, out, "1/4 The daemon", "Dugout installed: the daemon starts with your session and talks to off.",
			"2/4 The Claude Code plugin", "3/4 This repo", "4/4 Check", serviceInstalled(), "✓ the daemon responds",
			"✓ the Claude Code plugin is installed", "Undo all of this with dugout setup --remove.")
		if !strings.Contains(out, "Dugout mode in ") || !strings.Contains(out, ": scoped: ") || strings.Contains(out, "✗") {
			t.Fatalf("setup %d output:\n%s", i, out)
		}
	}
	if _, err := os.Stat(servicePathIn(w.home)); err != nil {
		t.Fatalf("no service after setup: %v", err)
	}
	if target, err := os.Readlink(binLink(w.home)); err != nil || target == "" {
		t.Fatalf("no bin link after setup: %q %v", target, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(w.home, ".claude", "settings.json")); !strings.Contains(string(raw), "dugout@dugout") {
		t.Fatalf("the plugin is not in settings.json: %s", raw)
	}

	out, errOut, code := w.run(t, "setup", "--remove")
	if code != 0 || !strings.Contains(out, "Dugout setup removed.") || strings.Contains(out, "Kept ") {
		t.Fatalf("remove: exit %d\n%s\n%s", code, out, errOut)
	}
	after := treeOf(t, filepath.Join(w.home, ".claude"), true)
	if !mapsEqual(before, after) {
		t.Fatalf("~/.claude after remove:\n%v\nbefore setup:\n%v", after, before)
	}
	if got, want := personFiles(w.home, treeOf(t, w.home, false)), personFiles(w.home, beforeHome); !mapsEqual(got, want) {
		t.Fatalf("HOME after remove:\n%v\nbefore setup:\n%v", got, want)
	}
	if _, err := os.Stat(setupDir(w.state)); !os.IsNotExist(err) {
		t.Fatalf("the setup record is still there: %v", err)
	}
	calls := readLog(t, w.claudeLog)
	if !strings.Contains(strings.Join(calls, "\n"), "plugin uninstall dugout@dugout\nplugin marketplace remove dugout") {
		t.Fatalf("claude calls: %q", calls)
	}
	// A second remove has nothing left to undo.
	if out, _, code := w.run(t, "setup", "--remove"); code != 0 || !strings.Contains(out, "nothing to undo") {
		t.Fatalf("second remove: %d %s", code, out)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestSetupRemoveKeepsLaterEdits: a file the person changed after setup is
// not put back over their change; remove says so and keeps its backup.
func TestSetupRemoveKeepsLaterEdits(t *testing.T) {
	w := newSetupWorld(t)
	if out, errOut, code := w.run(t, "setup"); code != 0 {
		t.Fatalf("setup: exit %d\n%s\n%s", code, out, errOut)
	}
	settings := filepath.Join(w.home, ".claude", "settings.json")
	writeFile(t, settings, "{\"theme\": \"light\"}\n", 0o644)
	out, errOut, code := w.run(t, "setup", "--remove")
	if code != 0 || !strings.Contains(out, "Kept "+settings+": it changed after dugout setup. Its original is in ") {
		t.Fatalf("remove: exit %d\n%s\n%s", code, out, errOut)
	}
	backup := strings.TrimSuffix(out[strings.Index(out, "Its original is in ")+len("Its original is in "):], "\n")
	backup = backup[:strings.Index(backup, ".\n")]
	if raw, err := os.ReadFile(backup); err != nil || string(raw) != "{\n  \"theme\": \"dark\"\n}\n" {
		t.Fatalf("backup %s: %q %v", backup, raw, err)
	}
	// What did not change is back as it was.
	if raw, _ := os.ReadFile(filepath.Join(w.home, ".claude.json")); string(raw) != "{\"numStartups\": 3}\n" {
		t.Fatalf(".claude.json: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(w.home, ".claude", "plugins", "installed_plugins.json")); !os.IsNotExist(err) {
		t.Fatalf("installed_plugins.json is still there: %v", err)
	}
}

// TestSetupRemoveKeepsClaudeState: ~/.claude.json is Claude Code's own and
// changes all the time; remove keeps its content, takes the plugin out through
// the claude CLI, and restores the other files byte for byte.
func TestSetupRemoveKeepsClaudeState(t *testing.T) {
	w := newSetupWorld(t)
	before := treeOf(t, filepath.Join(w.home, ".claude"), true)
	if out, errOut, code := w.run(t, "setup"); code != 0 {
		t.Fatalf("setup: exit %d\n%s\n%s", code, out, errOut)
	}
	state := filepath.Join(w.home, ".claude.json")
	writeFile(t, state, "{\"numStartups\": 4}\n", 0o644)
	out, errOut, code := w.run(t, "setup", "--remove")
	if code != 0 || strings.Contains(out, "Kept ") {
		t.Fatalf("remove: exit %d\n%s\n%s", code, out, errOut)
	}
	if raw, _ := os.ReadFile(state); string(raw) != "{\"numStartups\": 4}\n" {
		t.Fatalf(".claude.json: %q", raw)
	}
	if !mapsEqual(before, treeOf(t, filepath.Join(w.home, ".claude"), true)) {
		t.Fatalf("~/.claude is not as it was before setup")
	}
	if calls := strings.Join(readLog(t, w.claudeLog), "\n"); !strings.Contains(calls, "plugin uninstall dugout@dugout\nplugin marketplace remove dugout") {
		t.Fatalf("claude calls: %q", calls)
	}
}

// TestSetupRemoveRestoresEarlierDaemon: a daemon installed by hand before
// setup comes back, against its own server, after remove.
func TestSetupRemoveRestoresEarlierDaemon(t *testing.T) {
	w := newSetupWorld(t)
	if out, errOut, code := w.run(t, "install", "-insecure", "-server", "http://10.0.0.1:7457"); code != 0 {
		t.Fatalf("install: %d %s %s", code, out, errOut)
	}
	earlier, _ := os.ReadFile(servicePathIn(w.home))
	// setup keeps the installed daemon's server when given none.
	out, _, _ := w.run(t, "setup")
	if !strings.Contains(out, "talks to http://10.0.0.1:7457.") {
		t.Fatalf("setup did not keep the installed server:\n%s", out)
	}
	if out, errOut, code := w.run(t, "setup", "-server", "off"); code != 0 {
		t.Fatalf("setup -server off: %d\n%s\n%s", code, out, errOut)
	}
	if out, errOut, code := w.run(t, "setup", "--remove"); code != 0 {
		t.Fatalf("remove: %d\n%s\n%s", code, out, errOut)
	}
	if raw, err := os.ReadFile(servicePathIn(w.home)); err != nil || !bytes.Equal(raw, earlier) {
		t.Fatalf("the earlier service is not back: %v\n%s", err, raw)
	}
	calls := readLog(t, w.serviceLog)
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last, "bootstrap ") && !strings.HasSuffix(last, "restart "+systemd.Unit) {
		t.Fatalf("the earlier daemon was not loaded again; service calls: %q", calls)
	}
}

// TestSetupAndJoinRefuseInsecureServer: setup and join take the same rule as
// install: an http:// server outside this machine needs -insecure, and join
// refuses before it sends the invite code there.
func TestSetupAndJoinRefuseInsecureServer(t *testing.T) {
	w := newSetupWorld(t)
	for _, args := range [][]string{
		{"setup", "-server", "http://10.0.0.5:7457"},
		{"join", "http://10.0.0.5:7457#dgi_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ01234-_"},
	} {
		out, errOut, code := w.run(t, args...)
		if code == 0 || !strings.Contains(out+errOut, "-insecure") {
			t.Errorf("dugout %s: code=%d out=%q err=%q, want a refusal naming -insecure", strings.Join(args, " "), code, out, errOut)
		}
	}
	if _, err := os.Stat(servicePathIn(w.home)); err == nil {
		t.Fatal("a refused setup installed the service")
	}
}

// TestSetupOutsideRepo: outside a git repo setup skips init and says how.
func TestSetupOutsideRepo(t *testing.T) {
	w := newSetupWorld(t)
	w.repo = t.TempDir()
	out, errOut, code := w.run(t, "setup")
	if code != 0 || !strings.Contains(out, "Not in a git repo: run dugout init in each repo you work in.") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
}

// TestSetupWithoutClaude: with no claude on the PATH the plugin step says so
// and doctor skips the plugin.
func TestSetupWithoutClaude(t *testing.T) {
	w := newSetupWorld(t)
	if err := os.Remove(filepath.Join(w.tools, "claude")); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := w.run(t, "setup")
	if code != 0 || !strings.Contains(out, "Claude Code is not on the PATH") || !strings.Contains(out, "– plugin: could not find claude on the PATH") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
}
