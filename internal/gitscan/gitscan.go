// Package gitscan derives a worktree's in-flight change from git: the diff,
// untracked files included, against the merge-base with the target branch.
// Git is the source of truth, so edits from Bash, codegen or the IDE count
// too. Every command goes through Args, which keeps git from taking the
// user's index lock.
//
// No -c core.fsmonitor or core.untrackedCache: both only pay off when git
// can write their state back to the index, which --no-optional-locks
// forbids, and forcing fsmonitor on leaves a daemon running per worktree.
// Git follows the user's own config instead, so a user who runs fsmonitor
// gets fast scans from their own daemon.
package gitscan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/repoconfig"
)

// BaseArgs go before every git command the daemon runs.
var BaseArgs = []string{"--no-optional-locks"}

var (
	ErrNotRepo  = errors.New("not a git repo")
	ErrNoRepoID = errors.New("the repo has neither a remote origin nor a repo_id in .dugout.yml")
	ErrNoTarget = errors.New("could not resolve the target branch: origin/HEAD or target_branch in .dugout.yml is missing")
)

// Args returns the full git argument list for running args in dir.
func Args(dir string, args ...string) []string {
	out := slices.Clone(BaseArgs)
	out = append(out, "-C", dir)
	return append(out, args...)
}

// Run runs git in dir and returns its raw stdout. The error carries stderr.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", Args(dir, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func runLine(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Run(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// gitRefused reports whether err is git itself exiting with an error, as
// opposed to git not running at all (missing binary, canceled context),
// which says nothing about the repo.
func gitRefused(ctx context.Context, err error) bool {
	var exit *exec.ExitError
	return ctx.Err() == nil && errors.As(err, &exit)
}

// Root returns the top level of the worktree that contains path, with
// symlinks resolved. Only git refusing the path is ErrNotRepo.
func Root(ctx context.Context, path string) (string, error) {
	top, err := runLine(ctx, path, "rev-parse", "--show-toplevel")
	switch {
	case err != nil && gitRefused(ctx, err):
		return "", fmt.Errorf("%w: %s", ErrNotRepo, path)
	case err != nil:
		return "", err
	case top == "":
		return "", fmt.Errorf("%w: %s", ErrNotRepo, path)
	}
	return filepath.EvalSymlinks(top)
}

// NormalizeRemote turns a remote URL into a repo identity that is the same
// for every clone: host and path in lower case, without user, port, ".git"
// or trailing slash. Local paths keep their case. An identity normalizes to
// itself, so the serve allowlist takes a remote or an identity alike.
func NormalizeRemote(raw string) string {
	u := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if rest, ok := strings.CutPrefix(u, "file://"); ok {
		return localRemote(rest)
	}
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") {
		return localRemote(u)
	}
	var hostport, p string
	if _, rest, ok := strings.Cut(u, "://"); ok {
		hostport, p, _ = strings.Cut(rest, "/")
	} else if h, rest, ok := strings.Cut(u, ":"); ok { // scp-like: git@host:org/repo
		hostport, p = h, rest
	} else { // an identity already: host/org/repo
		hostport, p, _ = strings.Cut(u, "/")
	}
	if i := strings.LastIndex(hostport, "@"); i >= 0 {
		hostport = hostport[i+1:]
	}
	host, _, _ := strings.Cut(hostport, ":")
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	if p == "" {
		return strings.ToLower(host)
	}
	return strings.ToLower(host + "/" + p)
}

func localRemote(p string) string {
	return strings.TrimSuffix(filepath.Clean(p), ".git")
}

// Snapshot is one scan of a worktree.
type Snapshot struct {
	Root      string
	Repo      string
	Branch    string
	Target    string
	MergeBase string
	Files     []string // repo-relative, sorted, ignore list applied
	AllFiles  []string // same change before the ignore list: sorted, deduplicated

	// InProgress is true while a merge, rebase or cherry-pick is half done,
	// or the status has unmerged entries.
	InProgress bool
	// GitDir is this worktree's git dir, CommonDir the one shared by all
	// worktrees of the repo. Both absolute.
	GitDir, CommonDir string

	// Person is the worktree's git user.name, "" when unset: the human an
	// escalation names for this side.
	Person string
	// Tip is HEAD and every remote branch of Branch's name, as one string
	// that moves when the worktree commits or pushes (core.Ledger). It is
	// object names only: no code.
	Tip string
	// EscalateAfter is the repo's escalate_after (repoconfig), 0 when off;
	// ConfigErr says why a value it could not read fell back to the default.
	EscalateAfter time.Duration
	ConfigErr     string
}

// inProgressMarkers are the GitDir entries that exist while a merge, rebase
// or cherry-pick is half done.
var inProgressMarkers = []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD", "rebase-merge", "rebase-apply"}

// MidOperation reports whether a merge, rebase or cherry-pick is half done in
// the worktree whose git dir is gitDir, from its markers alone. It runs no
// git, so it can recheck a Snapshot that may be older than the worktree;
// unlike InProgress it does not see unmerged entries without a marker.
func MidOperation(gitDir string) bool {
	if gitDir == "" {
		return false
	}
	for _, m := range inProgressMarkers {
		if _, err := os.Stat(filepath.Join(gitDir, m)); err == nil {
			return true
		}
	}
	return false
}

// RepoID is the repo identity of the worktree at root, as Scan names it,
// without scanning it.
func RepoID(ctx context.Context, root string) (string, error) {
	cfg, err := repoconfig.Load(root)
	if err != nil {
		return "", err
	}
	return repoID(ctx, root, cfg)
}

// repoID is cfg's repo_id, else origin's URL. Either is normalized like a
// remote: the identity is the same for every clone and matches what the serve
// allowlist stores.
func repoID(ctx context.Context, root string, cfg repoconfig.Config) (string, error) {
	if id := NormalizeRemote(cfg.RepoID); id != "" {
		return id, nil
	}
	url, err := runLine(ctx, root, "remote", "get-url", "origin")
	var exit *exec.ExitError
	switch {
	case err != nil && gitRefused(ctx, err) && errors.As(err, &exit) && exit.ExitCode() == 2: // no such remote
		return "", ErrNoRepoID
	case err != nil:
		return "", err
	case url == "":
		return "", ErrNoRepoID
	}
	return NormalizeRemote(url), nil
}

// Scan reads the in-flight change of the worktree that contains path.
func Scan(ctx context.Context, path string) (Snapshot, error) {
	root, err := Root(ctx, path)
	if err != nil {
		return Snapshot{}, err
	}
	cfg, err := repoconfig.Load(root)
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{Root: root, Target: cfg.TargetBranch}
	if s.EscalateAfter, err = cfg.EscalationDelay(); err != nil {
		s.ConfigErr = err.Error()
	}
	if s.Repo, err = repoID(ctx, root, cfg); err != nil {
		return Snapshot{}, err
	}
	if s.Target == "" {
		ref, err := runLine(ctx, root, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
		if err != nil || ref == "" {
			return Snapshot{}, ErrNoTarget
		}
		s.Target = strings.TrimPrefix(ref, "refs/remotes/")
	}
	if _, err := runLine(ctx, root, "rev-parse", "--verify", "-q", s.Target+"^{commit}"); err != nil {
		return Snapshot{}, fmt.Errorf("%w (%s)", ErrNoTarget, s.Target)
	}
	if s.Branch, err = runLine(ctx, root, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || s.Branch == "" {
		s.Branch = "(detached)"
	}
	if s.MergeBase, err = runLine(ctx, root, "merge-base", "HEAD", s.Target); err != nil {
		return Snapshot{}, err
	}

	diff, err := Run(ctx, root, "diff", "--name-only", "--no-renames", "-z", s.MergeBase)
	if err != nil {
		return Snapshot{}, err
	}
	status, err := Run(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Snapshot{}, err
	}
	dirs, err := runLine(ctx, root, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir", "HEAD")
	if err != nil {
		return Snapshot{}, err
	}
	lines := strings.Split(dirs, "\n")
	if len(lines) != 3 {
		return Snapshot{}, fmt.Errorf("git rev-parse: unexpected git dirs output %q", dirs)
	}
	s.GitDir, s.CommonDir, s.Tip = strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])
	if s.Branch != "(detached)" {
		// A push moves the remote branch even when HEAD stays: refs/remotes/*/<branch>,
		// where * matches one remote name.
		remotes, err := runLine(ctx, root, "for-each-ref", "--format=%(objectname)", "refs/remotes/*/"+s.Branch)
		if err != nil {
			return Snapshot{}, err
		}
		if remotes != "" {
			s.Tip += "\n" + remotes
		}
	}

	// Unset is exit 1, and no name: the escalation names the branch alone.
	s.Person, _ = runLine(ctx, root, "config", "--get", "user.name")

	untr, unmerged := parseStatus(status)
	s.InProgress = unmerged || MidOperation(s.GitDir)

	all := append(splitZ(diff), untr...)
	slices.Sort(all)
	s.AllFiles = slices.Compact(all)
	files := repoconfig.NewMatcher(cfg).Filter(slices.Clone(s.AllFiles))
	slices.Sort(files)
	s.Files = slices.Compact(files)
	return s, nil
}

func splitZ(out string) []string {
	var parts []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// parseStatus reads `git status --porcelain=v1 -z`: the "??" entries, and
// whether any entry is unmerged. Rename and copy entries carry their original
// path in the next field, which is skipped.
func parseStatus(status string) (untracked []string, unmerged bool) {
	fields := splitZ(status)
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		switch xy := entry[:2]; {
		case xy == "??":
			untracked = append(untracked, entry[3:])
		case xy == "DD" || xy == "AU" || xy == "UD" || xy == "UA" || xy == "DU" || xy == "AA" || xy == "UU":
			unmerged = true
		case xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C':
			i++
		}
	}
	return untracked, unmerged
}
