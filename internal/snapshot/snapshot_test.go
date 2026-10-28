package snapshot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/testgit"
)

const testWS = "d1:feat-a"

// takeFixture is a fixture whose clone is on feat-a, and a store.
func takeFixture(t *testing.T) (testgit.Fixture, *Store) {
	t.Helper()
	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	return f, openStore(t)
}

func scanOf(t *testing.T, dir string) gitscan.Snapshot {
	t.Helper()
	scan, err := gitscan.Scan(context.Background(), dir)
	if err != nil {
		t.Fatalf("Scan(%s): %v", dir, err)
	}
	return scan
}

// take scans dir, points the store's alternates at its repo and snapshots it.
func take(t *testing.T, s *Store, dir string) (Snap, gitscan.Snapshot) {
	t.Helper()
	scan := scanOf(t, dir)
	if err := s.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Take(context.Background(), testWS, scan)
	if err != nil {
		t.Fatalf("Take(%s): %v", dir, err)
	}
	if snap.Base != scan.MergeBase {
		t.Fatalf("Base = %s, want merge-base %s", snap.Base, scan.MergeBase)
	}
	return snap, scan
}

func treeFiles(t *testing.T, s *Store, commit string) []string {
	t.Helper()
	var files []string
	for _, p := range strings.Split(testgit.Git(t, s.Dir, "ls-tree", "-r", "-z", "--name-only", commit), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files
}

// blobAt is the blob id of path in commit, read in the store.
func blobAt(t *testing.T, s *Store, commit, path string) string {
	t.Helper()
	return testgit.Git(t, s.Dir, "rev-parse", commit+":"+path)
}

// gitDirState maps every file under gitDir to the sha256 of its content.
// Object mtimes are left out: git refreshes them on objects it finds already
// written, which only delays their pruning.
func gitDirState(t *testing.T, gitDir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(gitDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		state[strings.TrimPrefix(p, gitDir)] = fmt.Sprintf("%x", sha256.Sum256(body))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func diffState(before, after map[string]string) []string {
	var diff []string
	for p, h := range before {
		if after[p] != h {
			diff = append(diff, "changed or removed: "+p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			diff = append(diff, "added: "+p)
		}
	}
	slices.Sort(diff)
	return diff
}

// TestTakeWritesNothingToClone: the daemon never writes to the user's repo,
// so no objects, no index and no refs in it. Also when its index is
// split, where writing an index can mean writing a sharedindex file next to
// it (always, with maxPercentChange 0), and when it has a post-index-change
// hook, which git runs after every index write, the copy's included.
func TestTakeWritesNothingToClone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, clone string)
	}{
		{"plain", func(*testing.T, string) {}},
		{"split index", func(t *testing.T, clone string) {
			testgit.Git(t, clone, "config", "core.splitIndex", "true")
			testgit.Git(t, clone, "config", "splitIndex.maxPercentChange", "0")
			testgit.Git(t, clone, "update-index", "--split-index")
		}},
		{"post-index-change hook", func(t *testing.T, clone string) {
			hook := filepath.Join(clone, ".git", "hooks", "post-index-change")
			testgit.WriteFile(t, hook, "#!/bin/sh\necho ran >> \"$(git rev-parse --git-dir)/HOOK_LOG\"\n")
			if err := os.Chmod(hook, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, s := takeFixture(t)
			c.setup(t, f.Clone)
			testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "package shared // changed\n")
			testgit.WriteFile(t, filepath.Join(f.Clone, "new.txt"), "new\n")
			scan := scanOf(t, f.Clone)
			if err := s.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
				t.Fatal(err)
			}

			gitDir := filepath.Join(f.Clone, ".git")
			index := filepath.Join(gitDir, "index")
			status := testgit.Git(t, f.Clone, "status", "--porcelain")
			before := gitDirState(t, gitDir)
			objects := countFiles(t, filepath.Join(gitDir, "objects"))
			indexStat, err := os.Stat(index)
			if err != nil {
				t.Fatal(err)
			}

			snap, err := s.Take(context.Background(), testWS, scan)
			if err != nil {
				t.Fatal(err)
			}

			if n := countFiles(t, filepath.Join(gitDir, "objects")); n != objects {
				t.Errorf("files under .git/objects: %d -> %d", objects, n)
			}
			if after, err := os.Stat(index); err != nil || !after.ModTime().Equal(indexStat.ModTime()) {
				t.Errorf(".git/index mtime changed (%v)", err)
			}
			if diff := diffState(before, gitDirState(t, gitDir)); len(diff) > 0 {
				t.Errorf("Take wrote to the clone's .git:\n%s", strings.Join(diff, "\n"))
			}
			if got := testgit.Git(t, f.Clone, "status", "--porcelain"); got != status {
				t.Errorf("status %q -> %q", status, got)
			}
			if got := treeFiles(t, s, snap.Commit); !slices.Contains(got, "new.txt") {
				t.Errorf("snapshot tree %v lacks new.txt", got)
			}
		})
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTakeDeterministic(t *testing.T) {
	f, s := takeFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "package shared // one\n")
	first, scan := take(t, s, f.Clone)
	second, _ := take(t, s, f.Clone)
	if first.Commit != second.Commit {
		t.Fatalf("unchanged worktree, commits %s and %s", first.Commit, second.Commit)
	}

	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "package shared // two\n")
	third, _ := take(t, s, f.Clone)
	if third.Commit == first.Commit {
		t.Fatal("edited worktree, same commit")
	}
	if got := testgit.Git(t, s.Dir, "rev-parse", third.Commit+"^"); got != scan.MergeBase {
		t.Fatalf("parent = %s, want merge-base %s", got, scan.MergeBase)
	}
	if got := testgit.Git(t, s.Dir, "rev-parse", RefFor(testWS)); got != third.Commit {
		t.Fatalf("%s = %s, want %s", RefFor(testWS), got, third.Commit)
	}
}

func TestTakeIncludesUntrackedRespectsGitignore(t *testing.T) {
	f, s := takeFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, ".gitignore"), "*.log\n")
	testgit.WriteFile(t, filepath.Join(f.Clone, "new.txt"), "new\n")
	testgit.WriteFile(t, filepath.Join(f.Clone, "x.log"), "log\n")
	snap, _ := take(t, s, f.Clone)
	got := treeFiles(t, s, snap.Commit)
	if !slices.Contains(got, "new.txt") || slices.Contains(got, "x.log") {
		t.Fatalf("snapshot tree = %v, want new.txt and no x.log", got)
	}
}

func TestTakeLinkedWorktree(t *testing.T) {
	f, s := takeFixture(t)
	wt := f.AddWorktree(t, "wt", "feat-b")
	testgit.WriteFile(t, filepath.Join(wt, "shared.go"), "package shared // linked\n")
	snap, _ := take(t, s, wt)
	if got, want := blobAt(t, s, snap.Commit, "shared.go"), testgit.Git(t, wt, "hash-object", "shared.go"); got != want {
		t.Fatalf("shared.go in snapshot = %s, want %s", got, want)
	}
}

// TestTakeExcludesDenylistAndLarge: excluded files keep their
// merge-base version and are never hashed into the store. An inherited
// GIT_LITERAL_PATHSPECS must not turn the exclusions into plain paths.
func TestTakeExcludesDenylistAndLarge(t *testing.T) {
	for _, literal := range []bool{false, true} {
		t.Run(fmt.Sprintf("literal=%v", literal), func(t *testing.T) {
			f, s := takeFixture(t)
			testgit.WriteFile(t, filepath.Join(f.Clone, "id_rsa"), "PRIVATE KEY\n")
			testgit.Git(t, f.Clone, "add", "id_rsa")
			testgit.Git(t, f.Clone, "commit", "-q", "-m", "key")
			testgit.WriteFile(t, filepath.Join(f.Clone, ".env"), "SECRET=1\n")
			testgit.WriteFile(t, filepath.Join(f.Clone, "a.txt"), strings.Repeat("x", 2<<20))
			testgit.WriteFile(t, filepath.Join(f.Clone, "new.txt"), "new\n")
			if literal {
				t.Setenv("GIT_LITERAL_PATHSPECS", "1")
			}

			snap, scan := take(t, s, f.Clone)
			got := treeFiles(t, s, snap.Commit)
			if slices.Contains(got, ".env") || slices.Contains(got, "id_rsa") || !slices.Contains(got, "new.txt") {
				t.Fatalf("snapshot tree = %v, want new.txt and neither .env nor id_rsa", got)
			}
			if got, want := blobAt(t, s, snap.Commit, "a.txt"), blobAt(t, s, scan.MergeBase, "a.txt"); got != want {
				t.Fatalf("a.txt in snapshot = %s, want merge-base version %s", got, want)
			}
			for _, name := range []string{".env", "a.txt"} {
				blob := testgit.Git(t, f.Clone, "hash-object", name)
				if out, err := s.git(context.Background(), "cat-file", "-e", blob); err == nil {
					t.Fatalf("%s was hashed into the store: %s", name, out)
				}
			}
			if left, _ := filepath.Glob(filepath.Join(s.Dir, "pathspec-*")); len(left) > 0 {
				t.Fatalf("pathspec files left behind: %v", left)
			}
		})
	}
}

// TestDenied: the denylist ignores case, since APFS and NTFS do and a .ENV is
// as secret as a .env, and stops at credential files by construction.
func TestDenied(t *testing.T) {
	for _, name := range []string{
		".env", ".env.local", ".ENV", "prod.KEY", "ID_RSA", "server.Pem",
		".npmrc", ".netrc", "_netrc", ".pgpass", ".git-credentials", ".pypirc",
		"cert.p12", "cert.PFX", "release.jks", "app.keystore", "putty.ppk",
		"terraform.tfstate", "terraform.tfstate.backup", "credentials.json",
	} {
		if !denied(name) {
			t.Errorf("denied(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"main.go", "keys.go", "credentials.go", "environment.ts", "tfstate.go",
		"keystore.go", "README.md", "npmrc.md", "package.json",
	} {
		if denied(name) {
			t.Errorf("denied(%q) = true, want false", name)
		}
	}
}

// TestTakeExcludesDenylistIgnoringCase: the globs that catch files created
// after the scan ignore case too.
func TestTakeExcludesDenylistIgnoringCase(t *testing.T) {
	f, s := takeFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "new.txt"), "new\n")
	scan := scanOf(t, f.Clone)
	if err := s.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
		t.Fatal(err)
	}
	secrets := []string{".ENV", "sub/prod.KEY", "ID_RSA", "sub/.npmrc", "infra/terraform.tfstate.backup"}
	for _, name := range secrets {
		testgit.WriteFile(t, filepath.Join(f.Clone, name), "SECRET "+name+"\n")
	}

	snap, err := s.Take(context.Background(), testWS, scan)
	if err != nil {
		t.Fatal(err)
	}
	got := treeFiles(t, s, snap.Commit)
	if !slices.Contains(got, "new.txt") {
		t.Fatalf("snapshot tree = %v, want new.txt", got)
	}
	for _, name := range secrets {
		if slices.Contains(got, name) {
			t.Errorf("snapshot tree = %v, want no %s", got, name)
		}
		blob := testgit.Git(t, f.Clone, "hash-object", name)
		if out, err := s.git(context.Background(), "cat-file", "-e", blob); err == nil {
			t.Errorf("%s was hashed into the store: %s", name, out)
		}
	}
}

// TestTakeExcludesDenylistCreatedAfterScan: a secret that appears between
// Scan and Take is missing from the scan's exclusions, and the denylist still
// keeps it out.
func TestTakeExcludesDenylistCreatedAfterScan(t *testing.T) {
	f, s := takeFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "new.txt"), "new\n")
	scan := scanOf(t, f.Clone)
	if err := s.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
		t.Fatal(err)
	}
	testgit.WriteFile(t, filepath.Join(f.Clone, ".env"), "SECRET=1\n")
	testgit.WriteFile(t, filepath.Join(f.Clone, "sub", "x.pem"), "PEM\n")

	snap, err := s.Take(context.Background(), testWS, scan)
	if err != nil {
		t.Fatal(err)
	}
	got := treeFiles(t, s, snap.Commit)
	if slices.Contains(got, ".env") || slices.Contains(got, "sub/x.pem") || !slices.Contains(got, "new.txt") {
		t.Fatalf("snapshot tree = %v, want new.txt and neither .env nor sub/x.pem", got)
	}
	for _, name := range []string{".env", "sub/x.pem"} {
		blob := testgit.Git(t, f.Clone, "hash-object", name)
		if out, err := s.git(context.Background(), "cat-file", "-e", blob); err == nil {
			t.Fatalf("%s was hashed into the store: %s", name, out)
		}
	}
}

// TestTakeExcludesForcedAddAfterScan: a denylisted or oversized file that git
// add -f puts in the index between Scan and Take is no ignored file the add
// glob skips: it is already in the index Take copies, so only a reset keeps it
// out of the snapshot.
func TestTakeExcludesForcedAddAfterScan(t *testing.T) {
	f, s := takeFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, ".gitignore"), ".env\n*.pem\n")
	testgit.WriteFile(t, filepath.Join(f.Clone, "new.txt"), "new\n")
	scan := scanOf(t, f.Clone)
	if err := s.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
		t.Fatal(err)
	}
	forced := map[string]string{".env": "SECRET=1\n", "sub/x.pem": "PEM\n", "big.txt": strings.Repeat("x", 2<<20)}
	for name, content := range forced {
		testgit.WriteFile(t, filepath.Join(f.Clone, name), content)
		testgit.Git(t, f.Clone, "add", "-f", name)
	}

	snap, err := s.Take(context.Background(), testWS, scan)
	if err != nil {
		t.Fatal(err)
	}
	got := treeFiles(t, s, snap.Commit)
	if !slices.Contains(got, "new.txt") {
		t.Fatalf("snapshot tree = %v, want new.txt", got)
	}
	for name := range forced {
		if slices.Contains(got, name) {
			t.Errorf("snapshot tree = %v, want no %s", got, name)
		}
		// git add -f wrote the blob into the clone's own objects, which the
		// store reads as an alternate: look only in the store's.
		blob := testgit.Git(t, f.Clone, "hash-object", name)
		if _, err := os.Stat(filepath.Join(s.Dir, "objects", blob[:2], blob[2:])); err == nil {
			t.Errorf("%s was hashed into the store", name)
		}
	}
}

func TestTakeInProgress(t *testing.T) {
	f, s := takeFixture(t)
	scan := scanOf(t, f.Clone)
	scan.InProgress = true
	if _, err := s.Take(context.Background(), testWS, scan); !errors.Is(err, ErrInProgress) {
		t.Fatalf("Take = %v, want ErrInProgress", err)
	}
	if out, err := s.git(context.Background(), "rev-parse", "--verify", "-q", RefFor(testWS)); err == nil {
		t.Fatalf("%s exists: %s", RefFor(testWS), out)
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, "index-*")); len(left) > 0 {
		t.Fatalf("Take touched the store: %v", left)
	}
}

// TestTakeStaleScanInProgress: a merge that started after the scan still
// stops the Take, and the ref keeps the previous snapshot.
func TestTakeStaleScanInProgress(t *testing.T) {
	f, s := takeFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "before\n")
	prev, scan := take(t, s, f.Clone)
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "half a merge\n")
	testgit.WriteFile(t, filepath.Join(scan.GitDir, "MERGE_HEAD"), testgit.Git(t, f.Clone, "rev-parse", "HEAD")+"\n")
	if _, err := s.Take(context.Background(), testWS, scan); !errors.Is(err, ErrInProgress) {
		t.Fatalf("Take with a scan from before the merge = %v, want ErrInProgress", err)
	}
	if got := testgit.Git(t, s.Dir, "rev-parse", RefFor(testWS)); got != prev.Commit {
		t.Fatalf("%s = %s, want the previous snapshot %s", RefFor(testWS), got, prev.Commit)
	}
}

// TestTakePathWithSpaces: the real clones live under directories with spaces,
// and so can the daemon's state dir.
func TestTakePathWithSpaces(t *testing.T) {
	f := testgit.NewFixture(t)
	clone := filepath.Join(f.Base, "with space", "clone")
	testgit.Git(t, f.Base, "clone", "-q", f.Origin, clone)
	testgit.Git(t, clone, "switch", "-q", "-c", "feat-a")
	s, err := Open(filepath.Join(t.TempDir(), "state with space"), "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	testgit.WriteFile(t, filepath.Join(clone, "shared.go"), "package shared // spaced\n")
	testgit.WriteFile(t, filepath.Join(clone, "with space.txt"), "new\n")
	testgit.WriteFile(t, filepath.Join(clone, ".env local"), "SECRET=1\n")

	snap, _ := take(t, s, clone)
	got := treeFiles(t, s, snap.Commit)
	if !slices.Contains(got, "with space.txt") || slices.Contains(got, ".env local") {
		t.Fatalf("snapshot tree = %v, want with space.txt and no .env local", got)
	}
	if got, want := blobAt(t, s, snap.Commit, "shared.go"), testgit.Git(t, clone, "hash-object", "shared.go"); got != want {
		t.Fatalf("shared.go in snapshot = %s, want %s", got, want)
	}
}

func TestTakeDetachedHead(t *testing.T) {
	f, s := takeFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "--detach", "HEAD~0")
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "package shared // detached\n")
	snap, scan := take(t, s, f.Clone)
	if got := testgit.Git(t, s.Dir, "rev-parse", snap.Commit+"^"); got != scan.MergeBase {
		t.Fatalf("parent = %s, want merge-base %s", got, scan.MergeBase)
	}
	if got, want := blobAt(t, s, snap.Commit, "shared.go"), testgit.Git(t, f.Clone, "hash-object", "shared.go"); got != want {
		t.Fatalf("shared.go in snapshot = %s, want %s", got, want)
	}
}
