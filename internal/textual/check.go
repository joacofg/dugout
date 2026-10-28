package textual

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/joacofg/dugout/internal/core"
)

// Side is one workstream's in-flight change: Head (a commit or a tree)
// against Base, the commit it started from.
type Side struct{ Base, Head string }

// Status is the outcome of Check.
type Status string

const (
	Clean    Status = "clean"
	Conflict Status = "conflict"
	Stale    Status = "stale"
)

// FileConflict is one file where the two sides cannot merge cleanly. Kind is
// "content" when git left conflict blocks, with the lines of each block in
// each side's own head, or "file" when the whole file conflicts (modify and
// delete, binary) and there are no lines. Lines[0] is the caller's a. Text[i]
// is the text of each side's section of the blocks, capped (see Sections),
// and TextMore[i] how many sections the cap left out.
type FileConflict struct {
	Path     string
	Kind     string
	Lines    [2][]core.Range
	Text     [2][]core.Snippet
	TextMore [2]int
}

// Result of Check. How tells how the sides were brought onto one base:
// "same-base", "forward" or "backward"; it is empty when Stale. Stale lists
// the shared files where both moves conflicted.
type Result struct {
	Status Status
	How    string
	Files  []FileConflict
	Stale  []string
}

// markerAttr makes every conflict marker MarkerSize long, so Resolve can tell
// git's markers from lookalikes in the files themselves.
var markerAttr = "* conflict-marker-size=" + strconv.Itoa(MarkerSize)

// Prepare adds markerAttr to <gitDir>/info/attributes unless it is already
// there.
func Prepare(gitDir string) error {
	path := filepath.Join(gitDir, "info", "attributes")
	body, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) == markerAttr {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	return os.WriteFile(path, append(body, markerAttr+"\n"...), 0o644)
}

// Changed lists the files head changes from base, sorted.
func Changed(ctx context.Context, gitDir, base, head string) ([]string, error) {
	out, err := repo(gitDir).run(ctx, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", base, head)
	if err != nil {
		return nil, err
	}
	paths := splitZ(out)
	slices.Sort(paths)
	return paths, nil
}

// leg is one side as the final merge saw it: tree is what was merged, moved
// the head it was moved from, empty when it was merged as is.
type leg struct{ tree, moved string }

// Check merges the changes of a and b, each taken from its own base, and
// reports the files of shared where they conflict. When the bases differ,
// the side with the older base is first moved onto the newer one (forward);
// if that move conflicts in shared, the newer side is moved back onto the
// older base instead (backward). A move may conflict outside shared, in
// files only the drift between the bases touches; those don't concern the
// pair, so the move still counts. When both moves conflict in shared, or
// neither base contains the other, the pair can't be measured: Stale.
func Check(ctx context.Context, gitDir string, a, b Side, shared []string) (Result, error) {
	r := repo(gitDir)
	var (
		final      string
		conflicted []string
		how        string
		legs       [2]leg
		swapped    bool // legs[0] is b
		err        error
	)
	if a.Base == b.Base {
		how, legs = "same-base", [2]leg{{tree: a.Head}, {tree: b.Head}}
		if final, conflicted, err = r.merge(ctx, a.Base, a.Head, b.Head); err != nil {
			return Result{}, err
		}
	} else {
		older, newer := a, b
		aOlder, err := r.isAncestor(ctx, a.Base, b.Base)
		if err != nil {
			return Result{}, err
		}
		if !aOlder {
			bOlder, err := r.isAncestor(ctx, b.Base, a.Base)
			if err != nil {
				return Result{}, err
			}
			if !bOlder {
				return Result{Status: Stale}, nil
			}
			older, newer, swapped = b, a, true
		}
		fwd, cf, err := r.merge(ctx, older.Base, newer.Base, older.Head)
		if err != nil {
			return Result{}, err
		}
		if len(within(cf, shared)) == 0 {
			how, legs = "forward", [2]leg{{tree: fwd, moved: older.Head}, {tree: newer.Head}}
			if final, conflicted, err = r.merge(ctx, newer.Base, fwd, newer.Head); err != nil {
				return Result{}, err
			}
		} else {
			back, cb, err := r.merge(ctx, newer.Base, older.Base, newer.Head)
			if err != nil {
				return Result{}, err
			}
			if len(within(cb, shared)) > 0 {
				return Result{Status: Stale, Stale: within(append(cf, cb...), shared)}, nil
			}
			how, legs = "backward", [2]leg{{tree: older.Head}, {tree: back, moved: newer.Head}}
			if final, conflicted, err = r.merge(ctx, older.Base, older.Head, back); err != nil {
				return Result{}, err
			}
		}
	}

	res := Result{Status: Clean, How: how}
	for _, path := range within(conflicted, shared) {
		fc, err := r.fileConflict(ctx, final, path, legs)
		if err != nil {
			return Result{}, err
		}
		if swapped {
			fc.Lines[0], fc.Lines[1] = fc.Lines[1], fc.Lines[0]
			fc.Text[0], fc.Text[1] = fc.Text[1], fc.Text[0]
			fc.TextMore[0], fc.TextMore[1] = fc.TextMore[1], fc.TextMore[0]
		}
		res.Files = append(res.Files, fc)
	}
	if len(res.Files) > 0 {
		res.Status = Conflict
	}
	return res, nil
}

// fileConflict reads path in the final merge and locates its conflict
// blocks in each leg.
func (r repo) fileConflict(ctx context.Context, final, path string, legs [2]leg) (FileConflict, error) {
	fc := FileConflict{Path: path, Kind: "file"}
	oid, err := r.blob(ctx, final, path)
	if err != nil || oid == "" {
		return fc, err
	}
	merged, err := r.run(ctx, nil, "cat-file", "blob", oid)
	if err != nil {
		return fc, err
	}
	for side, l := range legs {
		resolved, blocks, err := Resolve(merged, side)
		if err != nil {
			return fc, fmt.Errorf("%s: %w", path, err)
		}
		if len(blocks) == 0 { // no blocks: the whole file conflicts
			return fc, nil
		}
		fc.Kind = "content"
		if fc.Lines[side], err = r.lines(ctx, path, l, resolved, blocks); err != nil {
			return fc, err
		}
		fc.Text[side], fc.TextMore[side] = Sections(resolved, blocks)
	}
	return fc, nil
}

// lines carries blocks, ranges of resolved, back to the leg's own head:
// first to the tree the final merge saw, then, for a moved leg, to the head
// before the move. A leg without the file has no lines.
func (r repo) lines(ctx context.Context, path string, l leg, resolved []byte, blocks []core.Range) ([]core.Range, error) {
	side, err := r.blob(ctx, l.tree, path)
	if err != nil || side == "" {
		return nil, err
	}
	resolvedOID, err := r.run(ctx, resolved, "hash-object", "-w", "--stdin")
	if err != nil {
		return nil, err
	}
	steps := make([][]Hunk, 0, 2)
	h, err := r.hunks(ctx, side, strings.TrimSpace(string(resolvedOID)))
	if err != nil {
		return nil, err
	}
	steps = append(steps, h)
	if l.moved != "" {
		head, err := r.blob(ctx, l.moved, path)
		if err != nil || head == "" {
			return nil, err
		}
		if h, err = r.hunks(ctx, head, side); err != nil {
			return nil, err
		}
		steps = append(steps, h)
	}
	out := make([]core.Range, 0, len(blocks))
	for _, b := range blocks {
		for _, h := range steps {
			b = MapToOld(h, b)
		}
		out = append(out, b)
	}
	return out, nil
}

// repo is a git dir the daemon owns (the store), so git runs on it without
// the user's global and system config.
type repo string

// run runs git on r and returns its stdout, also on error. The error carries
// stderr; exitCode reads git's exit code from it.
func (r repo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := Interruptible(exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + string(r)}, args...)...))
	cmd.Env = GitEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// exitCode is the code git exited with: 0 for a nil err, -1 when git did not
// run to completion (missing binary, canceled context).
func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// merge runs a three-way merge-tree of x and y over base and returns the
// resulting tree, with conflict blocks in diff3 style, and the conflicted
// paths. Exit code 1 means conflicts; anything but 0 or 1 is an error.
func (r repo) merge(ctx context.Context, base, x, y string) (tree string, conflicted []string, err error) {
	out, err := r.run(ctx, nil, "-c", "merge.conflictStyle=diff3", "merge-tree", "--write-tree", "-z",
		"--name-only", "--no-messages", "--merge-base="+base, x, y)
	code := exitCode(err)
	if code != 0 && code != 1 {
		return "", nil, err
	}
	fields := splitZ(out)
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("git merge-tree %s %s: no tree in output", x, y)
	}
	if code == 1 {
		conflicted = fields[1:]
	}
	return fields[0], conflicted, nil
}

func (r repo) isAncestor(ctx context.Context, x, y string) (bool, error) {
	_, err := r.run(ctx, nil, "merge-base", "--is-ancestor", x, y)
	switch exitCode(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// blob returns the id of the file at path in rev, or "" when rev has no file
// there (no such path, a directory or a submodule).
func (r repo) blob(ctx context.Context, rev, path string) (string, error) {
	out, err := r.run(ctx, []byte(rev+":"+path+"\x00"), "cat-file", "--batch-check=%(objectname) %(objecttype)", "-Z")
	if err != nil {
		return "", err
	}
	// "<oid> <type>", or "<rev>:<path> missing" when there is nothing.
	oid, typ, _ := strings.Cut(strings.TrimSuffix(string(out), "\x00"), " ")
	if typ != "blob" {
		return "", nil
	}
	return oid, nil
}

// hunks diffs blob from against blob to with no context.
func (r repo) hunks(ctx context.Context, from, to string) ([]Hunk, error) {
	if from == to {
		return nil, nil
	}
	out, err := r.run(ctx, nil, "diff", "-U0", "--no-color", "--no-ext-diff", from, to)
	if err != nil {
		return nil, err
	}
	return ParseHunks(string(out))
}

// within returns the paths that are also in shared, sorted and without
// duplicates.
func within(paths, shared []string) []string {
	var out []string
	for _, p := range paths {
		if slices.Contains(shared, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func splitZ(out []byte) []string {
	var parts []string
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
