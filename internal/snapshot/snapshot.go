package snapshot

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/joacofg/dugout/internal/gitscan"
)

// ErrInProgress is Take refusing a worktree whose index and files are half a
// merge, rebase or cherry-pick.
var ErrInProgress = errors.New("worktree with a merge, rebase or cherry-pick half done")

// denylist holds the base-name patterns of files that never reach a
// snapshot, and maxFileSize the largest file that does. The patterns are
// lowercase and match ignoring case, as APFS and NTFS do. Each one is a
// credential by its name alone: a pattern that also hit ordinary code would
// hide it from the teammates who share it.
var denylist = []string{
	".env*", "*.pem", "*.key", "id_*",
	".npmrc", ".netrc", "_netrc", ".pgpass", ".git-credentials", ".pypirc", "credentials.json",
	"*.p12", "*.pfx", "*.jks", "*.keystore", "*.ppk",
	"*.tfstate", "*.tfstate.*",
}

const maxFileSize = 1 << 20

// identity makes a snapshot commit a function of its tree and parent alone.
var identity = []string{
	"GIT_AUTHOR_NAME=dugout", "GIT_AUTHOR_EMAIL=dugout@localhost", "GIT_AUTHOR_DATE=@0 +0000",
	"GIT_COMMITTER_NAME=dugout", "GIT_COMMITTER_EMAIL=dugout@localhost", "GIT_COMMITTER_DATE=@0 +0000",
}

// Snap is a worktree snapshot in the store: Commit's only parent is Base, the
// merge-base it was taken against.
type Snap struct{ Commit, Base string }

// IndexPath is the copy of the user's index that Take of ws works on, inside
// the store at dir.
func IndexPath(dir, ws string) string {
	h := fnv.New64a()
	h.Write([]byte(ws))
	return filepath.Join(dir, fmt.Sprintf("index-%016x", h.Sum64()))
}

// Take writes the worktree of scan, untracked files included and ignored ones
// left out, as a commit on top of scan.MergeBase and points RefFor(ws) at it.
// Excluded files keep their merge-base version and are never hashed into
// the store.
//
// A worktree half-way through a merge, rebase or cherry-pick is ErrInProgress
// and the ref keeps the previous snapshot. scan can be older than the
// worktree, so the markers are read again at the start and before the ref
// moves: an operation that started since leaves its files half written.
//
// Nothing is written to the user's repo: git runs in the worktree with
// the user's config, hooks and split index aside, but on a copy of its index
// in the store, writing objects into the store and reading the repo's as an
// alternate.
func (s *Store) Take(ctx context.Context, ws string, scan gitscan.Snapshot) (Snap, error) {
	if scan.InProgress || gitscan.MidOperation(scan.GitDir) {
		return Snap{}, ErrInProgress
	}
	index := IndexPath(s.Dir, ws)
	if err := copyIndex(filepath.Join(scan.GitDir, "index"), index); err != nil {
		return Snap{}, err
	}
	env := worktreeEnv(index, filepath.Join(s.Dir, "objects"), filepath.Join(scan.CommonDir, "objects"))

	// The denylist also goes as globs, for files created after the scan.
	addSpec := []string{"."}
	for _, pattern := range denylist {
		addSpec = append(addSpec, ":(exclude,glob,icase)**/"+pattern)
	}
	known := excluded(scan.Root, scan.AllFiles)
	for _, p := range known {
		addSpec = append(addSpec, ":(exclude,literal)"+p)
	}
	addFile, err := s.pathspecFile(addSpec)
	if err != nil {
		return Snap{}, err
	}
	defer os.Remove(addFile)
	if _, err := inWorktree(ctx, scan.Root, env, "add", "-A", "--pathspec-from-file="+addFile, "--pathspec-file-nul"); err != nil {
		return Snap{}, err
	}
	// A file git add -f put in the index after the scan is in the copy and
	// outside every exclude above: reset what the index holds that the scan
	// did not know and the denylist or size cap would have kept out.
	late, err := indexExcluded(ctx, scan, env)
	if err != nil {
		return Snap{}, err
	}
	var resetSpec []string
	for _, p := range append(known, late...) {
		resetSpec = append(resetSpec, ":(literal)"+p)
	}
	if len(resetSpec) > 0 {
		resetFile, err := s.pathspecFile(resetSpec)
		if err != nil {
			return Snap{}, err
		}
		defer os.Remove(resetFile)
		if _, err := inWorktree(ctx, scan.Root, env, "reset", "-q", scan.MergeBase, "--pathspec-from-file="+resetFile, "--pathspec-file-nul"); err != nil {
			return Snap{}, err
		}
	}
	tree, err := inWorktree(ctx, scan.Root, env, "write-tree")
	if err != nil {
		return Snap{}, err
	}
	out, err := s.gitEnv(ctx, identity, "commit-tree", tree, "-p", scan.MergeBase, "-m", "dugout-snapshot")
	if err != nil {
		return Snap{}, err
	}
	commit := strings.TrimSpace(string(out))
	if gitscan.MidOperation(scan.GitDir) {
		return Snap{}, ErrInProgress
	}
	if _, err := s.git(ctx, "update-ref", RefFor(ws), commit); err != nil {
		return Snap{}, err
	}
	return Snap{Commit: commit, Base: scan.MergeBase}, nil
}

// indexExcluded returns the files in the index at env that excluded rejects
// and the scan did not list: those git add -f put there since. Files the scan
// listed were already judged, so only the new ones are looked at on disk.
func indexExcluded(ctx context.Context, scan gitscan.Snapshot, env []string) ([]string, error) {
	out, err := inWorktree(ctx, scan.Root, env, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	scanned := make(map[string]bool, len(scan.AllFiles))
	for _, f := range scan.AllFiles {
		scanned[f] = true
	}
	var fresh []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" && !scanned[f] {
			fresh = append(fresh, f)
		}
	}
	return excluded(scan.Root, fresh), nil
}

// excluded returns the files that never reach a snapshot: a base name on the
// denylist, or more than maxFileSize bytes. Lstat, since git stores a
// symlink's target path, not the file it points at. Deleted files stay in.
func excluded(root string, files []string) []string {
	var out []string
	for _, f := range files {
		if denied(path.Base(f)) {
			out = append(out, f)
		} else if fi, err := os.Lstat(filepath.Join(root, f)); err == nil && fi.Size() > maxFileSize {
			out = append(out, f)
		}
	}
	return out
}

func denied(name string) bool {
	name = strings.ToLower(name)
	for _, pattern := range denylist {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// copyIndex copies the worktree's index to dst, mtime included: git compares
// it with the entries' mtimes to spot files changed right after the index was
// written. It also drops the lock a git killed mid-write (a timed-out check)
// leaves behind.
func copyIndex(src, dst string) error {
	if err := os.Remove(dst + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// pathspecFile writes entries NUL-separated to a temp file in the store and
// returns its path.
func (s *Store) pathspecFile(entries []string) (string, error) {
	f, err := os.CreateTemp(s.Dir, "pathspec-")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(strings.Join(entries, "\x00") + "\x00")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// inWorktree runs git in the user's worktree at root with env and returns its
// trimmed stdout. Hooks and a split index stay off: git runs a
// post-index-change hook in the user's repo after writing the copy, and
// writing a split index writes a sharedindex file into the repo.
func inWorktree(ctx context.Context, root string, env []string, args ...string) (string, error) {
	off := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.splitIndex=false"}
	out, err := run(ctx, env, gitscan.Args(root, append(off, args...)...)...)
	return strings.TrimSpace(string(out)), err
}

// worktreeEnv is the process environment for git in a user's worktree, with
// the index and object writes redirected to the store. The global pathspec
// switches go too: the pathspecs Take writes spell out their own magic, and
// GIT_LITERAL_PATHSPECS would read them as file names.
func worktreeEnv(index, objects, alternates string) []string {
	var env []string
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_INDEX_FILE="+index, "GIT_OBJECT_DIRECTORY="+objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+alternates)
}
