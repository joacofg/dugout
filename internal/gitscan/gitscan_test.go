package gitscan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/repoconfig"
)

func TestArgsAlwaysHaveSafetyFlags(t *testing.T) {
	got := Args("/r", "status")
	want := append(slices.Clone(BaseArgs), "-C", "/r", "status")
	if !slices.Equal(got, want) {
		t.Fatalf("Args = %v, want %v", got, want)
	}
	if !slices.Contains(BaseArgs, "--no-optional-locks") {
		t.Fatalf("BaseArgs lost --no-optional-locks: %v", BaseArgs)
	}
}

// TestScanStartsNoFSMonitor: Scan never writes the index, so forcing
// core.fsmonitor on would only leave a `git fsmonitor--daemon` running per
// worktree without speeding anything up. Git follows the user's own config.
func TestScanStartsNoFSMonitor(t *testing.T) {
	f := newFixture(t)
	scan(t, f.clone)
	out, err := exec.Command("git", "-C", f.clone, "fsmonitor--daemon", "status").CombinedOutput()
	if err == nil {
		exec.Command("git", "-C", f.clone, "fsmonitor--daemon", "stop").Run()
		t.Fatalf("Scan started an fsmonitor daemon: %s", out)
	}
}

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Acme/App.git":            "github.com/acme/app",
		"https://user@github.com/Acme/App.git/":  "github.com/acme/app",
		"ssh://git@github.com/Acme/App":          "github.com/acme/app",
		"/x/origin.git":                          "/x/origin",
		"file:///x/origin.git":                   "/x/origin",
		"https://GitHub.com:443/acme/app.git":    "github.com/acme/app",
		"ssh://git@gitlab.example.com:2222/a/b/": "gitlab.example.com/a/b",
		// An identity normalizes to itself: the serve allowlist takes either.
		"github.com/acme/app":      "github.com/acme/app",
		"GitHub.com/Acme/App.git/": "github.com/acme/app",
		"host":                     "host",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func scan(t *testing.T, dir string) Snapshot {
	t.Helper()
	s, err := Scan(context.Background(), dir)
	if err != nil {
		t.Fatalf("Scan(%s): %v", dir, err)
	}
	return s
}

func TestScanInFlight(t *testing.T) {
	f := newFixture(t)
	gitT(t, f.clone, "switch", "-q", "-c", "feat")
	writeFile(t, filepath.Join(f.clone, "committed.txt"), "x\n")
	gitT(t, f.clone, "add", "committed.txt")
	gitT(t, f.clone, "commit", "-q", "-m", "c")
	writeFile(t, filepath.Join(f.clone, "staged.txt"), "x\n")
	gitT(t, f.clone, "add", "staged.txt")
	writeFile(t, filepath.Join(f.clone, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(f.clone, "untracked.txt"), "x\n")
	writeFile(t, filepath.Join(f.clone, "package-lock.json"), "changed\n")

	s := scan(t, f.clone)
	want := []string{"a.txt", "committed.txt", "staged.txt", "untracked.txt"}
	if !slices.Equal(s.Files, want) {
		t.Fatalf("Files = %v, want %v", s.Files, want)
	}
	if s.Target != "origin/main" || s.Branch != "feat" || s.Root != f.clone {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.Repo != NormalizeRemote(f.origin) || s.MergeBase == "" {
		t.Fatalf("repo %q mergebase %q", s.Repo, s.MergeBase)
	}
}

func TestScanCleanWorktreeIsEmpty(t *testing.T) {
	f := newFixture(t)
	if s := scan(t, f.clone); len(s.Files) != 0 {
		t.Fatalf("clean clone has files in flight: %v", s.Files)
	}
}

func TestScanIgnoresTargetDrift(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.seed, "drift.txt"), "x\n")
	gitT(t, f.seed, "add", "drift.txt")
	gitT(t, f.seed, "commit", "-q", "-m", "drift")
	gitT(t, f.seed, "push", "-q", "origin", "main")
	gitT(t, f.clone, "fetch", "-q")
	writeFile(t, filepath.Join(f.clone, "a.txt"), "changed\n")

	if s := scan(t, f.clone); !slices.Equal(s.Files, []string{"a.txt"}) {
		t.Fatalf("Files = %v, want only a.txt", s.Files)
	}
}

func TestScanRenameAndDelete(t *testing.T) {
	f := newFixture(t)
	gitT(t, f.clone, "mv", "a.txt", "b.txt")
	if err := os.Remove(filepath.Join(f.clone, "c.txt")); err != nil {
		t.Fatal(err)
	}
	s := scan(t, f.clone)
	for _, want := range []string{"a.txt", "b.txt", "c.txt"} {
		if !slices.Contains(s.Files, want) {
			t.Errorf("Files = %v, missing %s", s.Files, want)
		}
	}
}

func TestScanOverrides(t *testing.T) {
	f := newFixture(t)
	gitT(t, f.seed, "push", "-q", "origin", "main:dev")
	gitT(t, f.clone, "fetch", "-q")
	writeFile(t, filepath.Join(f.clone, ".dugout.yml"), "repo_id: github.com/acme/app\ntarget_branch: origin/dev\n")

	s := scan(t, f.clone)
	if s.Repo != "github.com/acme/app" || s.Target != "origin/dev" {
		t.Fatalf("overrides ignored: %+v", s)
	}
}

// TestScanPersonAndEscalation: the scan names the worktree's human by its
// git user.name, "" when unset, and carries the repo's escalate_after.
func TestScanPersonAndEscalation(t *testing.T) {
	f := newFixture(t)
	s := scan(t, f.clone)
	if s.Person != "" || s.EscalateAfter != repoconfig.DefaultEscalateAfter || s.ConfigErr != "" {
		t.Fatalf("no user.name, no .dugout.yml: Person %q, EscalateAfter %v, ConfigErr %q", s.Person, s.EscalateAfter, s.ConfigErr)
	}
	gitT(t, f.clone, "config", "user.name", "Ana Pérez")
	writeFile(t, filepath.Join(f.clone, ".dugout.yml"), "escalate_after: 3m\n")
	if s := scan(t, f.clone); s.Person != "Ana Pérez" || s.EscalateAfter != 3*time.Minute {
		t.Fatalf("Person %q, EscalateAfter %v; want Ana Pérez, 3m", s.Person, s.EscalateAfter)
	}
	writeFile(t, filepath.Join(f.clone, ".dugout.yml"), "escalate_after: soon\n")
	if s := scan(t, f.clone); s.EscalateAfter != repoconfig.DefaultEscalateAfter || !strings.Contains(s.ConfigErr, "escalate_after") {
		t.Fatalf("bad escalate_after: EscalateAfter %v, ConfigErr %q", s.EscalateAfter, s.ConfigErr)
	}
}

// TestScanTip: the tip moves with a commit, and with a push that leaves HEAD
// where it was, but not with edits that are not committed; a remote branch of
// another name does not count.
func TestScanTip(t *testing.T) {
	f := newFixture(t)
	gitT(t, f.clone, "switch", "-q", "-c", "feat/tip")
	first := scan(t, f.clone).Tip
	if head := strings.TrimSpace(gitT(t, f.clone, "rev-parse", "HEAD")); first != head {
		t.Fatalf("Tip = %q, want HEAD %s alone before any push", first, head)
	}
	writeFile(t, filepath.Join(f.clone, "tip.txt"), "x\n")
	if s := scan(t, f.clone); s.Tip != first {
		t.Fatalf("an edit moved the tip: %q -> %q", first, s.Tip)
	}
	gitT(t, f.clone, "add", "tip.txt")
	gitT(t, f.clone, "commit", "-qm", "tip")
	committed := scan(t, f.clone).Tip
	if committed == first {
		t.Fatal("a commit did not move the tip")
	}
	gitT(t, f.clone, "push", "-q", "origin", "feat/tip")
	pushed := scan(t, f.clone).Tip
	if pushed == committed || !strings.HasPrefix(pushed, committed+"\n") {
		t.Fatalf("a push did not move the tip: %q -> %q", committed, pushed)
	}
	gitT(t, f.clone, "push", "-q", "origin", "HEAD:refs/heads/tip")
	if s := scan(t, f.clone); s.Tip != pushed {
		t.Fatalf("a remote branch of another name moved the tip: %q -> %q", pushed, s.Tip)
	}
}

// TestScanNormalizesRepoID: a repo_id written as a remote URL, with capitals
// or ".git", scans as the canonical identity the serve allowlist stores.
func TestScanNormalizesRepoID(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.clone, ".dugout.yml"), "repo_id: github.com/Acme/App.git\n")
	if s := scan(t, f.clone); s.Repo != "github.com/acme/app" {
		t.Fatalf("Repo = %q, want github.com/acme/app", s.Repo)
	}
}

func TestScanErrors(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	if _, err := Scan(ctx, t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Errorf("non-repo dir: err = %v, want ErrNotRepo", err)
	}

	bare := t.TempDir()
	gitT(t, bare, "init", "-q", "-b", "main")
	if _, err := Scan(ctx, bare); !errors.Is(err, ErrNoRepoID) {
		t.Errorf("repo without origin: err = %v, want ErrNoRepoID", err)
	}

	f := newFixture(t)
	gitT(t, f.clone, "remote", "set-head", "origin", "-d")
	if _, err := Scan(ctx, f.clone); !errors.Is(err, ErrNoTarget) {
		t.Errorf("no origin/HEAD: err = %v, want ErrNoTarget", err)
	}
}

// TestScanTransientFailuresAreNotPermanent: a canceled scan or a missing
// git binary says nothing about the repo, so it must not look like
// ErrNotRepo or ErrNoRepoID, which make the daemon drop the worktree.
func TestScanTransientFailuresAreNotPermanent(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Scan(ctx, f.clone)
	if err == nil || errors.Is(err, ErrNotRepo) || errors.Is(err, ErrNoRepoID) {
		t.Errorf("canceled scan: err = %v, want a non-permanent error", err)
	}

	t.Setenv("PATH", "")
	_, err = Scan(context.Background(), f.clone)
	if err == nil || errors.Is(err, ErrNotRepo) || errors.Is(err, ErrNoRepoID) {
		t.Errorf("git missing from PATH: err = %v, want a non-permanent error", err)
	}
}

func TestScanLinkedWorktree(t *testing.T) {
	f := newFixture(t)
	wt := filepath.Join(f.base, "wt")
	gitT(t, f.clone, "worktree", "add", "-q", "-b", "feat-b", wt)
	writeFile(t, filepath.Join(wt, "shared.go"), "changed\n")

	main, linked := scan(t, f.clone), scan(t, wt)
	if linked.Repo != main.Repo || linked.Root != wt || linked.Branch != "feat-b" {
		t.Fatalf("linked worktree snapshot %+v (clone repo %q)", linked, main.Repo)
	}
	if !slices.Equal(linked.Files, []string{"shared.go"}) {
		t.Fatalf("linked Files = %v", linked.Files)
	}
}

func manyFilesFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	for i := range 200 {
		writeFile(t, filepath.Join(f.clone, "files", fmt.Sprintf("f%03d.txt", i)), "x\n")
	}
	gitT(t, f.clone, "add", "-A")
	gitT(t, f.clone, "commit", "-q", "-m", "many files")
	return f
}

// touch changes the mtime of n tracked files without changing their content,
// which leaves stat-dirty index entries that a plain `git status` or
// `git diff` refreshes and writes back under index.lock.
func touch(t *testing.T, f fixture, from, n int, mtime time.Time) {
	t.Helper()
	for j := range n {
		p := filepath.Join(f.clone, "files", fmt.Sprintf("f%03d.txt", (from+j)%200))
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// TestScanNeverWritesIndex is the deterministic half of the index-lock
// guarantee: with stat-dirty entries, Scan leaves the user's index byte for byte
// as it was, so it never needs index.lock.
func TestScanNeverWritesIndex(t *testing.T) {
	f := manyFilesFixture(t)
	touch(t, f, 0, 40, time.Now().Add(time.Hour))
	index := filepath.Join(f.clone, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, f.clone)
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatal("Scan rewrote .git/index; some git command lost --no-optional-locks")
	}
}

// TestNoIndexLockContention checks the index-lock guarantee end to end: the
// daemon scanning in a loop never makes the user's own git commands fail on
// index.lock.
func TestNoIndexLockContention(t *testing.T) {
	f := manyFilesFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var scanErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if _, err := Scan(ctx, f.clone); err != nil && ctx.Err() == nil {
				scanErr = err
				return
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 50 || time.Now().Before(deadline); i++ {
		touch(t, f, i*40, 40, time.Now().Add(time.Duration(i)*time.Second))
		out, err := exec.Command("git", "-C", f.clone, "add", "-A").CombinedOutput()
		if strings.Contains(string(out), "index.lock") {
			cancel()
			wg.Wait()
			t.Fatalf("git add hit index.lock on iteration %d: %s", i, out)
		}
		if err != nil {
			t.Fatalf("git add: %v\n%s", err, out)
		}
	}
	cancel()
	wg.Wait()
	if scanErr != nil {
		t.Fatalf("Scan failed during contention: %v", scanErr)
	}
}

func TestScanAllFilesUnfiltered(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.clone, "package-lock.json"), "changed\n")
	writeFile(t, filepath.Join(f.clone, "shared.go"), "changed\n")

	s := scan(t, f.clone)
	if want := []string{"package-lock.json", "shared.go"}; !slices.Equal(s.AllFiles, want) {
		t.Fatalf("AllFiles = %v, want %v", s.AllFiles, want)
	}
	if want := []string{"shared.go"}; !slices.Equal(s.Files, want) {
		t.Fatalf("Files = %v, want %v", s.Files, want)
	}
}

func TestScanInProgress(t *testing.T) {
	f := newFixture(t)
	if scan(t, f.clone).InProgress {
		t.Fatal("clean clone reports a merge in progress")
	}
	gitT(t, f.clone, "switch", "-q", "-c", "one")
	writeFile(t, filepath.Join(f.clone, "a.txt"), "one\n")
	gitT(t, f.clone, "commit", "-q", "-am", "one")
	gitT(t, f.clone, "switch", "-q", "-c", "two", "main")
	writeFile(t, filepath.Join(f.clone, "a.txt"), "two\n")
	gitT(t, f.clone, "commit", "-q", "-am", "two")
	if err := exec.Command("git", "-C", f.clone, "merge", "one").Run(); err == nil {
		t.Fatal("merge was expected to conflict")
	}

	if !scan(t, f.clone).InProgress {
		t.Fatal("conflicted merge not reported as in progress")
	}
	gitT(t, f.clone, "merge", "--abort")
	if scan(t, f.clone).InProgress {
		t.Fatal("aborted merge still reported as in progress")
	}
}

func TestMidOperation(t *testing.T) {
	if MidOperation("") {
		t.Fatal(`MidOperation("") = true`)
	}
	dir := t.TempDir()
	if MidOperation(dir) {
		t.Fatal("git dir without markers reported mid-operation")
	}
	if err := os.Mkdir(filepath.Join(dir, "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !MidOperation(dir) {
		t.Fatal("rebase-merge not reported as mid-operation")
	}
}

func TestScanGitDirs(t *testing.T) {
	f := newFixture(t)
	wt := filepath.Join(f.base, "wtb")
	gitT(t, f.clone, "worktree", "add", "-q", "-b", "wtb", wt)
	s := scan(t, wt)
	if want := filepath.Join(f.clone, ".git"); s.CommonDir != want {
		t.Errorf("CommonDir = %q, want %q", s.CommonDir, want)
	}
	if want := "/.git/worktrees/wtb"; !strings.HasSuffix(s.GitDir, want) {
		t.Errorf("GitDir = %q, want suffix %q", s.GitDir, want)
	}
}
