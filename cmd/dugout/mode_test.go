package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
)

// dugoutIn runs the built binary in dir against the daemon of home.
func dugoutIn(t *testing.T, home, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// TestModeCommand: dugout mode tells the repo's mode, the default until one
// is chosen, and sets it for every worktree of the repo; an unknown mode is
// refused.
func TestModeCommand(t *testing.T) {
	f := testgit.NewFixture(t)
	wt := f.AddWorktree(t, "wtb", "feat-b")
	home := shortDir(t)
	startLocalDaemon(t, home)
	if out, code := dugoutIn(t, home, f.Clone, "mode"); code != 0 || !strings.Contains(out, ": auto (default): ") || !strings.Contains(out, "/dugout") {
		t.Fatalf("dugout mode = %d %s", code, out)
	}
	if out, code := dugoutIn(t, home, f.Clone, "mode", "supervised"); code != 0 || !strings.Contains(out, ": supervised: your agent asks you") {
		t.Fatalf("dugout mode supervised = %d %s", code, out)
	}
	if out, code := dugoutIn(t, home, wt, "mode"); code != 0 || !strings.Contains(out, ": supervised: ") {
		t.Fatalf("dugout mode in the other worktree = %d %s", code, out)
	}
	if out, code := dugoutIn(t, home, f.Clone, "mode", "yolo"); code != 2 || !strings.Contains(out, "unknown mode") {
		t.Fatalf("dugout mode yolo = %d %s", code, out)
	}
	// What /dugout runs with no argument.
	if out, code := dugoutIn(t, home, f.Clone, "mode", ""); code != 0 || !strings.Contains(out, ": supervised: ") {
		t.Fatalf(`dugout mode "" = %d %s`, code, out)
	}
	if out, code := dugoutIn(t, home, shortDir(t), "mode"); code != 1 || !strings.Contains(out, "not a git repo") {
		t.Fatalf("dugout mode outside a repo = %d %s", code, out)
	}
}

// TestInitAsksModeOnce: dugout init tracks the repo and asks its mode on a
// terminal, auto on enter; a repo with a mode is not asked again, -mode sets
// it without asking, and without a terminal it stays in the default.
func TestInitAsksModeOnce(t *testing.T) {
	home := shortDir(t)
	startLocalDaemon(t, home)
	t.Setenv(statedir.EnvHome, home)
	init := func(stdin string, terminal bool, args ...string) (string, int) {
		t.Helper()
		interactive = func(io.Reader) bool { return terminal }
		t.Cleanup(func() { interactive = func(io.Reader) bool { return false } })
		var out, errb bytes.Buffer
		code := run(append([]string{"init"}, args...), strings.NewReader(stdin), &out, &errb)
		return out.String() + errb.String(), code
	}

	asked := testgit.NewFixture(t)
	out, code := init("nothing\n3\n", true, asked.Clone)
	if code != 0 || !strings.Contains(out, "1) auto (recommended): ") || !strings.Contains(out, "Choose 1, 2 or 3") || !strings.Contains(out, ": supervised: ") {
		t.Fatalf("init on a terminal = %d %s", code, out)
	}
	if roots, err := statedir.LoadWorktrees(home); err != nil || len(roots) != 1 || filepath.Base(roots[0]) != filepath.Base(asked.Clone) {
		t.Fatalf("tracked after init = %v, %v", roots, err)
	}
	if out, code := init("1\n", true, asked.Clone); code != 0 || strings.Contains(out, "Choose") || !strings.Contains(out, ": supervised: ") {
		t.Fatalf("second init = %d %s, want no question", code, out)
	}
	if out, code := init("", true, "-mode", "scoped", asked.Clone); code != 0 || strings.Contains(out, "Choose") || !strings.Contains(out, ": scoped: ") {
		t.Fatalf("init -mode scoped = %d %s", code, out)
	}

	enter := testgit.NewFixture(t)
	if out, code := init("\n", true, enter.Clone); code != 0 || !strings.Contains(out, ": auto: ") {
		t.Fatalf("init, enter = %d %s, want auto chosen", code, out)
	}
	piped := testgit.NewFixture(t)
	if out, code := init("", false, piped.Clone); code != 0 || strings.Contains(out, "Choose") || !strings.Contains(out, ": auto (default): ") {
		t.Fatalf("init without a terminal = %d %s", code, out)
	}
	if out, code := init("", false, "-mode", "yolo", piped.Clone); code != 2 || !strings.Contains(out, "unknown mode") {
		t.Fatalf("init -mode yolo = %d %s", code, out)
	}
}
