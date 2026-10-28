package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/secrets"
	"github.com/joacofg/dugout/internal/textual"
)

// ErrMissingBase is Import refusing a bundle whose base the store does not
// reach; FetchBase can bring it from origin.
var ErrMissingBase = errors.New("the bundle lacks a base the store cannot reach")

// localeC pins git's messages to English, which Import reads to tell a
// missing base from other failures.
var localeC = []string{"LC_ALL=C"}

// fetchTimeout bounds a base fetch.
const fetchTimeout = 60 * time.Second

// PeerRef is the ref that holds the imported snapshot of the remote
// workstream ws.
func PeerRef(ws string) string {
	return "refs/dugout/peer/" + strings.ReplaceAll(ws, ":", "/")
}

// outgoingRef is the ref Bundle points at the snapshot it packs: a bundle's
// heads are refs.
const outgoingRef = "refs/dugout/outgoing"

// Bundle packs snap on top of its base: snap.Commit itself, the snapshot the
// caller scanned, never whatever the workstream's ref holds by now (a Take
// whose timeout fired after its update-ref moved it). A snapshot that changes
// nothing bundles too, its commit alone: the other side learns that the
// change is gone.
func (s *Store) Bundle(ctx context.Context, snap Snap) ([]byte, error) {
	if _, err := s.git(ctx, "update-ref", outgoingRef, snap.Commit); err != nil {
		return nil, err
	}
	// The outgoing ref goes whatever happens, even if ctx was canceled.
	defer s.git(context.WithoutCancel(ctx), "update-ref", "-d", outgoingRef)
	return s.git(ctx, "bundle", "create", "-", outgoingRef, "^"+snap.Base)
}

// Narrow returns a snapshot on snap's base that changes only paths, each as
// snap has it (one snap deleted is deleted, one snap left alone stays as the
// base has it). It is what a peer needs to check the pairs those paths are
// in, and no more of the workstream: a teammate who pairs with one file does
// not get the rest of the change in flight. The same snap and paths give the
// same commit (identity).
func (s *Store) Narrow(ctx context.Context, snap Snap, paths []string) (Snap, error) {
	out, err := s.git(ctx, "diff-tree", "-r", "-z", "--no-renames", snap.Base, snap.Commit)
	if err != nil {
		return Snap{}, err
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	// Records: ":<old mode> <new mode> <old> <new> <status>" NUL path NUL.
	// A deletion has new mode 000000, which --index-info takes as a removal.
	var info strings.Builder
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 || !want[fields[i+1]] {
			continue
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", meta[1], meta[3], fields[i+1])
	}
	// GitEnv scrubs GIT_INDEX_FILE, so the store's own scratch index goes
	// after it. The store has one user at a time: one file name does.
	index := filepath.Join(s.Dir, "narrow-index")
	defer os.Remove(index)
	env := append(textual.GitEnv(), "GIT_INDEX_FILE="+index)
	if _, err := run(ctx, env, "--git-dir="+s.Dir, "read-tree", snap.Base); err != nil {
		return Snap{}, err
	}
	if info.Len() > 0 {
		if _, err := runIn(ctx, env, strings.NewReader(info.String()), "--git-dir="+s.Dir, "update-index", "-z", "--index-info"); err != nil {
			return Snap{}, err
		}
	}
	tree, err := run(ctx, env, "--git-dir="+s.Dir, "write-tree")
	if err != nil {
		return Snap{}, err
	}
	commit, err := s.gitEnv(ctx, identity, "commit-tree", strings.TrimSpace(string(tree)), "-p", snap.Base, "-m", "dugout-snapshot")
	if err != nil {
		return Snap{}, err
	}
	return Snap{Commit: strings.TrimSpace(string(commit)), Base: snap.Base}, nil
}

// Secret looks for a secret in the files snap adds or changes from its base,
// in path order. It returns the first path and the rule that matched, never
// the value; path is empty when there is none.
func (s *Store) Secret(ctx context.Context, snap Snap) (path, rule string, err error) {
	out, err := s.git(ctx, "diff-tree", "-r", "-z", "--no-renames", "--diff-filter=AMT", snap.Base, snap.Commit)
	if err != nil {
		return "", "", err
	}
	// Records: ":<modes> <old> <new> <status>" NUL path NUL.
	type entry struct{ path, blob string }
	var entries []entry
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 || meta[1] == "160000" {
			continue // submodule commit: no blob to read
		}
		entries = append(entries, entry{path: fields[i+1], blob: meta[3]})
	}
	if len(entries) == 0 {
		return "", "", nil
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.path, b.path) })

	var in strings.Builder
	for _, e := range entries {
		in.WriteString(e.blob + "\n")
	}
	batch, err := runIn(ctx, textual.GitEnv(), strings.NewReader(in.String()), "--git-dir="+s.Dir, "cat-file", "--batch")
	if err != nil {
		return "", "", err
	}
	r := bufio.NewReader(bytes.NewReader(batch))
	for _, e := range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			return "", "", fmt.Errorf("git cat-file --batch: %w", err)
		}
		hf := strings.Fields(header)
		if len(hf) != 3 {
			continue // "<sha> missing"
		}
		size, err := strconv.Atoi(hf[2])
		if err != nil {
			return "", "", fmt.Errorf("git cat-file --batch: bad header of %d bytes", len(header))
		}
		body := make([]byte, size+1) // content and its trailing newline
		if _, err := io.ReadFull(r, body); err != nil {
			return "", "", fmt.Errorf("git cat-file --batch: %w", err)
		}
		if rule, found := secrets.Scan(body[:size]); found {
			return e.path, rule, nil
		}
	}
	return "", "", nil
}

// Import verifies a bundle against what was announced and, only then, points
// PeerRef(ws) at its head: the bundle must have one head, want.Commit, whose
// only parent is want.Base. Objects are fsck'd on the way in. A base the
// store does not reach is ErrMissingBase.
func (s *Store) Import(ctx context.Context, ws string, bundle []byte, want Snap) (err error) {
	// One file name for every import: git's messages name the bundle's file,
	// and a caller that logs each message once sees the same one when the
	// same bundle fails again. The store has one user at a time.
	f, err := os.Create(filepath.Join(s.Dir, "incoming.bundle"))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(bundle)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := s.gitEnv(ctx, localeC, "bundle", "verify", f.Name()); err != nil {
		if strings.Contains(err.Error(), "lacks these prerequisite commits") {
			return ErrMissingBase
		}
		return err
	}
	out, err := s.git(ctx, "bundle", "list-heads", f.Name())
	if err != nil {
		return err
	}
	heads := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(heads) != 1 {
		return fmt.Errorf("the bundle carries %d heads, expected one", len(heads))
	}
	commit, ref, _ := strings.Cut(heads[0], " ")
	if commit != want.Commit {
		return fmt.Errorf("the bundle carries commit %s, %s was announced", commit, want.Commit)
	}
	// The incoming ref goes whatever happens, even if ctx was canceled.
	defer s.git(context.WithoutCancel(ctx), "update-ref", "-d", "refs/dugout/incoming")
	if _, err := s.git(ctx, "-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "--no-write-fetch-head",
		f.Name(), "+"+ref+":refs/dugout/incoming"); err != nil {
		return err
	}
	out, err = s.git(ctx, "rev-parse", want.Commit+"^@")
	if err != nil {
		return err
	}
	if parents := strings.TrimSpace(string(out)); parents != want.Base {
		return fmt.Errorf("the bundle's parent is %q, %s was announced", parents, want.Base)
	}
	_, err = s.git(ctx, "update-ref", PeerRef(ws), want.Commit)
	return err
}

// FetchBase fetches origin's branch at url into refs/dugout/base/<branch> of
// the store. It is the one git call that runs with the user's environment
// and config, for their credentials; hooks and prompts stay off and stdin is
// the null device. The fetch runs on the store, so git reads the user's
// global and system config but not the clone's own .git/config, nor an
// includeIf "gitdir:" that matches the clone: sshCommand is the
// core.sshCommand the caller read in the clone, empty for none. Whatever
// else only the clone's config sets stays invisible to the fetch: a per-repo
// credential helper, http.* settings (a proxy, an extra header, a CA file).
func (s *Store) FetchBase(ctx context.Context, url, branch, sshCommand string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	_, err := run(ctx, fetchEnv(os.Environ(), sshCommand), "--git-dir="+s.Dir, "-c", "core.hooksPath=/dev/null", "-c", "credential.interactive=false",
		"fetch", "--no-tags", "--no-write-fetch-head", "--quiet", url, "+refs/heads/"+branch+":refs/dugout/base/"+branch)
	return err
}

// fetchEnv is environ for a base fetch: nothing that redirects git or injects
// config, no prompts (askpass helpers included), and ssh as the user's git
// in their clone would run it, by git's own precedence: GIT_SSH_COMMAND in
// environ; else sshCommand, the clone's core.sshCommand, as
// GIT_SSH_COMMAND; else a legacy GIT_SSH in environ; else ssh in batch
// mode.
func fetchEnv(environ []string, sshCommand string) []string {
	var env []string
	envCommand, envSSH := false, false
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if textual.Redirects(name) || name == "GIT_ASKPASS" || name == "SSH_ASKPASS" {
			continue
		}
		switch name {
		case "GIT_SSH_COMMAND":
			envCommand = true
		case "GIT_SSH":
			envSSH = true
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	switch {
	case envCommand:
	case sshCommand != "":
		env = append(env, "GIT_SSH_COMMAND="+sshCommand)
	case !envSSH:
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// PrunePeers deletes the peer refs of every remote workstream not in keep.
func (s *Store) PrunePeers(ctx context.Context, keep []string) error {
	return s.pruneRefs(ctx, "refs/dugout/peer/", PeerRef, keep)
}
