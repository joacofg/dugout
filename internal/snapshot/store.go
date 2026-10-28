// Package snapshot keeps the daemon's private git store: a bare repo per
// repo identity, borrowing objects from the users' clones, where worktree
// snapshots are written and merges run without touching the users' repos.
package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/joacofg/dugout/internal/textual"
)

// Store is a bare repo under the daemon's state dir. It is not safe for
// concurrent use; the daemon's detector is its only user.
type Store struct{ Dir string }

// Open returns the store for repo under stateDir, creating it on first use.
// It is idempotent.
func Open(stateDir, repo string) (*Store, error) {
	h := fnv.New64a()
	h.Write([]byte(repo))
	dir := filepath.Join(stateDir, "repos", fmt.Sprintf("%016x.git", h.Sum64()))
	s := &Store{Dir: dir}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		if _, err := s.git(context.Background(), "init", "--bare", "-q", dir); err != nil {
			return nil, err
		}
	}
	if err := clearLocks(dir); err != nil {
		return nil, err
	}
	if _, err := s.git(context.Background(), "config", "gc.auto", "0"); err != nil {
		return nil, err
	}
	if err := textual.Prepare(dir); err != nil {
		return nil, err
	}
	return s, nil
}

// clearLocks removes the ref locks a git killed mid-write left in the store
// at dir: a *.lock under refs/ and packed-refs.lock. Either would make every
// later write of that ref, or every Prune, fail. Only one daemon runs per
// state dir and its detector opens a store before running any git on it, so
// no live git holds them.
func clearLocks(dir string) error {
	err := filepath.WalkDir(filepath.Join(dir, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".lock") {
			err = os.Remove(p)
		}
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "packed-refs.lock")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// SetAlternates points the store at the given object dirs, sorted and without
// duplicates. The file is only rewritten when it changes.
func (s *Store) SetAlternates(objectDirs []string) error {
	dirs := slices.Clone(objectDirs)
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)
	want := ""
	if len(dirs) > 0 {
		want = strings.Join(dirs, "\n") + "\n"
	}
	path := filepath.Join(s.Dir, "objects", "info", "alternates")
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(want), 0o644)
}

// Probe checks that the installed git can run the merges the store needs, by
// merging empty trees with merge-tree --write-tree.
func (s *Store) Probe(ctx context.Context) error {
	out, err := s.git(ctx, "hash-object", "-t", "tree", "-w", "--stdin")
	if err != nil {
		return err
	}
	empty := strings.TrimSpace(string(out))
	_, err = s.git(ctx, "merge-tree", "--write-tree", "--merge-base="+empty, empty, empty)
	return err
}

// RefFor is the ref that holds the snapshot of workstream ws.
func RefFor(ws string) string {
	return "refs/dugout/snap/" + strings.ReplaceAll(ws, ":", "/")
}

// Prune deletes the snapshot refs of every workstream not in keep.
func (s *Store) Prune(ctx context.Context, keep []string) error {
	return s.pruneRefs(ctx, "refs/dugout/snap/", RefFor, keep)
}

// pruneRefs deletes the refs under prefix that refFor does not map from a
// workstream in keep.
func (s *Store) pruneRefs(ctx context.Context, prefix string, refFor func(string) string, keep []string) error {
	out, err := s.git(ctx, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return err
	}
	kept := make(map[string]bool, len(keep))
	for _, ws := range keep {
		kept[refFor(ws)] = true
	}
	for ref := range strings.SplitSeq(string(out), "\n") {
		if ref == "" || kept[ref] {
			continue
		}
		if _, err := s.git(ctx, "update-ref", "-d", ref); err != nil {
			return err
		}
	}
	return nil
}

// PruneIndexes deletes the copied indexes of every workstream not in keep. It
// lists the store's dir rather than globbing it, so a state dir with glob
// syntax in its path ("[") still matches.
func (s *Store) PruneIndexes(keep []string) error {
	kept := make(map[string]bool, len(keep))
	for _, ws := range keep {
		kept[IndexPath(s.Dir, ws)] = true
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		f := filepath.Join(s.Dir, e.Name())
		if !strings.HasPrefix(e.Name(), "index-") || kept[f] {
			continue
		}
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Counts counts the store's loose objects and its packs: each import writes
// a pack of its own.
func (s *Store) Counts(ctx context.Context) (loose, packs int, err error) {
	out, err := s.git(ctx, "count-objects", "-v")
	if err != nil {
		return 0, 0, err
	}
	found := map[string]int{}
	for line := range strings.SplitSeq(string(out), "\n") {
		key, v, ok := strings.Cut(line, ":")
		if !ok || (key != "count" && key != "packs") {
			continue
		}
		if found[key], err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
			return 0, 0, fmt.Errorf("git count-objects: %s: %w", key, err)
		}
	}
	loose, okLoose := found["count"]
	packs, okPacks := found["packs"]
	if !okLoose || !okPacks {
		return 0, 0, fmt.Errorf("git count-objects: no count or packs in output")
	}
	return loose, packs, nil
}

// GC packs the store and drops what no ref reaches.
func (s *Store) GC(ctx context.Context) error {
	_, err := s.git(ctx, "gc", "--prune=now", "--quiet")
	return err
}

// git runs git on the store with the user's config out of the way and
// returns its stdout. The error carries stderr.
func (s *Store) git(ctx context.Context, args ...string) ([]byte, error) {
	return s.gitEnv(ctx, nil, args...)
}

// gitEnv is git with extra environment variables.
func (s *Store) gitEnv(ctx context.Context, extra []string, args ...string) ([]byte, error) {
	return run(ctx, textual.GitEnv(extra...), append([]string{"--git-dir=" + s.Dir}, args...)...)
}

// run runs git with env and returns its stdout. The error carries stderr. A
// canceled git is interrupted, so it drops its locks.
func run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return runIn(ctx, env, nil, args...)
}

// runIn is run with stdin; nil means no input (the null device).
func runIn(ctx context.Context, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := textual.Interruptible(exec.CommandContext(ctx, "git", args...))
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", redactUserinfo(strings.Join(args, " ")), err, redactUserinfo(strings.TrimSpace(stderr.String())))
	}
	return stdout.Bytes(), nil
}

// userinfoPattern is the credentials part of a URL: scheme://user:pass@.
var userinfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@`)

// redactUserinfo hides the credentials of every URL in s: a remote may carry
// a token (https://x-access-token:...@host), and errors end up in the log.
func redactUserinfo(s string) string { return userinfoPattern.ReplaceAllString(s, "${1}[token]@") }
