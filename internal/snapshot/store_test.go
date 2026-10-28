package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/testgit"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	testgit.Isolate(t)
	s, err := Open(t.TempDir(), "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenCreatesStore(t *testing.T) {
	testgit.Isolate(t)
	state := t.TempDir()
	s1, err := Open(state, "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(state, "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if s1.Dir != s2.Dir {
		t.Fatalf("Dir differs: %q vs %q", s1.Dir, s2.Dir)
	}
	if filepath.Dir(s1.Dir) != filepath.Join(state, "repos") || !strings.HasSuffix(s1.Dir, ".git") {
		t.Fatalf("Dir = %q, want under %s/repos/*.git", s1.Dir, state)
	}
	if got := testgit.Git(t, s1.Dir, "config", "gc.auto"); got != "0" {
		t.Fatalf("gc.auto = %q, want 0", got)
	}
	body, err := os.ReadFile(filepath.Join(s1.Dir, "info", "attributes"))
	if err != nil || !strings.Contains(string(body), "* conflict-marker-size=32") {
		t.Fatalf("info/attributes = %q (%v)", body, err)
	}
}

func TestSetAlternates(t *testing.T) {
	f1 := testgit.NewFixture(t)
	f2 := testgit.NewFixture(t)
	testgit.WriteFile(t, filepath.Join(f2.Clone, "only2.txt"), "only in second\n")
	blob := testgit.Git(t, f2.Clone, "hash-object", "-w", "only2.txt")
	o1 := filepath.Join(f1.Clone, ".git", "objects")
	o2 := filepath.Join(f2.Clone, ".git", "objects")

	s := openStore(t)
	if err := s.SetAlternates([]string{o2, o1, o2}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "objects", "info", "alternates")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{o1, o2}
	slices.Sort(want)
	if string(body) != strings.Join(want, "\n")+"\n" {
		t.Fatalf("alternates = %q, want %q", body, want)
	}
	testgit.Git(t, s.Dir, "cat-file", "-e", blob)

	// Unchanged input leaves the file alone.
	before, _ := os.Stat(path)
	if err := s.SetAlternates([]string{o1, o2}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("alternates rewritten although unchanged")
	}
}

func TestProbe(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.Probe(ctx); err != nil {
		t.Fatalf("Probe with system git: %v", err)
	}
	t.Setenv("PATH", testgit.FailingMergeTree(t)+":"+os.Getenv("PATH"))
	if err := s.Probe(ctx); err == nil {
		t.Fatal("Probe passed although merge-tree fails")
	}
}

func TestPruneAndGC(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	commit := func() string {
		return testgit.Git(t, s.Dir, "commit-tree", "-m", "snap", testgit.Git(t, s.Dir, "mktree"))
	}
	ca, cb := commit(), commit()
	testgit.Git(t, s.Dir, "update-ref", RefFor("d1:a"), ca)
	testgit.Git(t, s.Dir, "update-ref", RefFor("d1:b"), cb)

	if got := RefFor("d1:a"); got != "refs/dugout/snap/d1/a" {
		t.Fatalf("RefFor = %q", got)
	}
	if err := s.Prune(ctx, []string{"d1:a"}); err != nil {
		t.Fatal(err)
	}
	refs := testgit.Git(t, s.Dir, "for-each-ref", "--format=%(refname)", "refs/dugout/snap/")
	if refs != "refs/dugout/snap/d1/a" {
		t.Fatalf("refs after Prune = %q", refs)
	}

	orphan := hashBlob(t, s, "orphan blob\n")
	if err := s.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := s.git(ctx, "cat-file", "-e", orphan); err == nil {
		t.Fatalf("orphan blob survived GC: %s", out)
	}
	testgit.Git(t, s.Dir, "cat-file", "-e", ca)
}

func hashBlob(t *testing.T, s *Store, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "blob")
	testgit.WriteFile(t, f, body)
	return testgit.Git(t, s.Dir, "hash-object", "-w", f)
}

// TestOpenClearsStaleLocks: lock files a git left when it was killed
// mid-write (a shutdown or a timeout) would make every later Take of that
// workstream, or every Prune, fail. No git can hold them when Open runs.
func TestOpenClearsStaleLocks(t *testing.T) {
	f, s := takeFixture(t)
	take(t, s, f.Clone)
	testgit.Git(t, s.Dir, "pack-refs", "--all")
	testgit.WriteFile(t, filepath.Join(s.Dir, RefFor(testWS)+".lock"), "")
	testgit.WriteFile(t, filepath.Join(s.Dir, "packed-refs.lock"), "")

	s, err := Open(filepath.Dir(filepath.Dir(s.Dir)), "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "package shared // after the locks\n")
	take(t, s, f.Clone)
	if err := s.Prune(context.Background(), nil); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if refs := testgit.Git(t, s.Dir, "for-each-ref", "refs/dugout/snap/"); refs != "" {
		t.Fatalf("refs after Prune = %q, want none", refs)
	}
}

// TestGitInterruptedOnCancel: a canceled git gets SIGINT, on which git drops
// its lock files, not SIGKILL.
func TestGitInterruptedOnCancel(t *testing.T) {
	dir, ready, interrupted := testgit.InterruptibleGit(t)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				return
			}
		}
	}()
	if _, err := run(ctx, os.Environ(), "status"); err == nil {
		t.Fatal("a canceled git returned no error")
	}
	if _, err := os.Stat(interrupted); err != nil {
		t.Fatalf("git did not get SIGINT: %v", err)
	}
}

// TestCounts: a blob written loose adds one loose object, and a pack of it,
// as an import writes one, adds one pack.
func TestCounts(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	loose0, packs0, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blob := hashBlob(t, s, "one more\n")
	loose1, packs1, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loose1 != loose0+1 || packs1 != packs0 {
		t.Fatalf("after a loose blob: loose %d -> %d, packs %d -> %d; want +1 and the same", loose0, loose1, packs0, packs1)
	}
	cmd := exec.Command("git", "--git-dir="+s.Dir, "pack-objects", "-q", filepath.Join(s.Dir, "objects", "pack", "pack"))
	cmd.Stdin = strings.NewReader(blob + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pack-objects: %v\n%s", err, out)
	}
	_, packs2, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if packs2 != packs1+1 {
		t.Fatalf("after a pack: packs %d -> %d, want +1", packs1, packs2)
	}
}

func TestGCPrunesIndexes(t *testing.T) {
	f := testgit.NewFixture(t)
	s := openStore(t)
	scan := gitscan.Snapshot{Root: f.Clone, GitDir: filepath.Join(f.Clone, ".git"), CommonDir: filepath.Join(f.Clone, ".git"),
		MergeBase: testgit.Git(t, f.Clone, "rev-parse", "HEAD")}
	if err := s.SetAlternates([]string{filepath.Join(f.Clone, ".git", "objects")}); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"ws1", "ws2"} {
		if _, err := s.Take(context.Background(), ws, scan); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(IndexPath(s.Dir, ws)); err != nil {
			t.Fatalf("Take of %s left no index at IndexPath: %v", ws, err)
		}
	}
	if err := s.PruneIndexes([]string{"ws1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(IndexPath(s.Dir, "ws1")); err != nil {
		t.Errorf("index of ws1 gone: %v", err)
	}
	if _, err := os.Stat(IndexPath(s.Dir, "ws2")); err == nil {
		t.Error("index of ws2 still there")
	}
}

// TestPruneIndexesDirWithBracket: a state dir whose path holds glob syntax
// still has its indexes pruned.
func TestPruneIndexesDirWithBracket(t *testing.T) {
	testgit.Isolate(t)
	f := testgit.NewFixture(t)
	s, err := Open(filepath.Join(t.TempDir(), "state[x]"), "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	scan := gitscan.Snapshot{Root: f.Clone, GitDir: filepath.Join(f.Clone, ".git"), CommonDir: filepath.Join(f.Clone, ".git"),
		MergeBase: testgit.Git(t, f.Clone, "rev-parse", "HEAD")}
	if err := s.SetAlternates([]string{filepath.Join(f.Clone, ".git", "objects")}); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"ws1", "ws2"} {
		if _, err := s.Take(context.Background(), ws, scan); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneIndexes([]string{"ws1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(IndexPath(s.Dir, "ws1")); err != nil {
		t.Errorf("index of ws1 gone: %v", err)
	}
	if _, err := os.Stat(IndexPath(s.Dir, "ws2")); err == nil {
		t.Error("index of ws2 still there")
	}
}

// TestRedactUserinfo: credentials in a URL never reach an error: scheme://user:pass@host
// becomes scheme://[token]@host, wherever it appears in the text.
func TestRedactUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"fetch https://x-access-token:ghp_abc123@github.com/o/r.git +refs":               "fetch https://[token]@github.com/o/r.git +refs",
		"fatal: unable to access 'https://user:pw@host/x.git/': boom":                    "fatal: unable to access 'https://[token]@host/x.git/': boom",
		"ssh://git@host/x.git and HTTPS://tok@a.b/c":                                     "ssh://[token]@host/x.git and HTTPS://[token]@a.b/c",
		"https://github.com/o/r.git has no userinfo, nor does git@github.com:o/r.git ok": "https://github.com/o/r.git has no userinfo, nor does git@github.com:o/r.git ok",
	} {
		if got := redactUserinfo(in); got != want {
			t.Errorf("redactUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFetchBaseErrorHasNoCredentials: the error of a failed fetch, which the
// daemon logs, carries the remote without its userinfo.
func TestFetchBaseErrorHasNoCredentials(t *testing.T) {
	s := openStore(t)
	err := s.FetchBase(context.Background(), "https://x-access-token:ghp_SECRET123@127.0.0.1:1/x.git", "main", "")
	if err == nil {
		t.Fatal("FetchBase of an unreachable url succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, "ghp_SECRET123") || strings.Contains(msg, "x-access-token") || !strings.Contains(msg, "https://[token]@127.0.0.1:1/x.git") {
		t.Fatalf("error = %q, want the url without its userinfo", msg)
	}
}
