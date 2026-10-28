// Package testgit builds throwaway git repos for tests. It is only
// imported from _test.go files.
package testgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Isolate keeps the user's global and system git config (signing, hooks,
// default branch) out of the test, for setup commands and for the code
// under test, which inherits the environment.
func Isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

// Git runs git in dir and returns its trimmed output, failing the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// WriteFile writes body to path, creating parent directories.
func WriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Fixture is a bare origin on main, a seed clone that pushes to it and a
// fresh clone under test.
type Fixture struct {
	Base   string // parent dir, symlinks resolved
	Origin string
	Seed   string
	Clone  string
}

// NewFixture builds a Fixture whose main has a.txt, c.txt, shared.go and
// package-lock.json. It calls Isolate.
func NewFixture(t *testing.T) Fixture {
	t.Helper()
	Isolate(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := Fixture{
		Base:   base,
		Origin: filepath.Join(base, "origin.git"),
		Seed:   filepath.Join(base, "seed"),
		Clone:  filepath.Join(base, "clone"),
	}
	Git(t, base, "init", "-q", "--bare", "-b", "main", f.Origin)
	Git(t, base, "init", "-q", "-b", "main", f.Seed)
	for _, name := range []string{"a.txt", "c.txt", "shared.go", "package-lock.json"} {
		WriteFile(t, filepath.Join(f.Seed, name), name+"\n")
	}
	Git(t, f.Seed, "add", "-A")
	Git(t, f.Seed, "commit", "-q", "-m", "init")
	Git(t, f.Seed, "remote", "add", "origin", f.Origin)
	Git(t, f.Seed, "push", "-q", "origin", "main")
	Git(t, base, "clone", "-q", f.Origin, f.Clone)
	return f
}

// AddWorktree creates a linked worktree of the clone on a new branch.
func (f Fixture) AddWorktree(t *testing.T, name, branch string) string {
	t.Helper()
	dir := filepath.Join(f.Base, name)
	Git(t, f.Clone, "worktree", "add", "-q", "-b", branch, dir)
	return dir
}

// InterruptibleGit returns a directory holding a git script that never ends
// on its own: it creates the file ready once it traps SIGINT, and the file
// interrupted when one comes. Prepend the dir to PATH with t.Setenv.
func InterruptibleGit(t *testing.T) (dir, ready, interrupted string) {
	t.Helper()
	dir = t.TempDir()
	ready, interrupted = filepath.Join(dir, "ready"), filepath.Join(dir, "interrupted")
	script := "#!/bin/sh\n" +
		"trap ': > \"" + interrupted + "\"; exit 130' INT\n" +
		": > '" + ready + "'\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, ready, interrupted
}

// FailingMergeTree returns a directory holding a git script that exits 129
// for merge-tree, as a git too old for --write-tree does, and runs the real
// git otherwise. Prepend it to PATH with t.Setenv.
func FailingMergeTree(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The subcommand is the first argument that is not a global option;
	// -c and -C take a value.
	script := "#!/bin/sh\n" +
		"sub=; skip=0\n" +
		"for a in \"$@\"; do\n" +
		"  if [ $skip = 1 ]; then skip=0; continue; fi\n" +
		"  case \"$a\" in -c|-C) skip=1 ;; -*) ;; *) sub=$a; break ;; esac\n" +
		"done\n" +
		"if [ \"$sub\" = merge-tree ]; then exit 129; fi\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
