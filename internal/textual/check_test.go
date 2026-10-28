package textual

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/testgit"
)

// deleted, as a file's content in history.commit, removes the file.
const deleted = "\x00deleted"

const tenLines = "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"

// history builds commits with plumbing, so a test can stack any change on
// any parent without touching a worktree.
type history struct {
	t      *testing.T
	dir    string
	gitDir string
	index  string // temporary index for building trees
	n      int
	base   string // f.txt with lines 1..10, g.txt with "g"
}

func newHistory(t *testing.T) *history {
	t.Helper()
	testgit.Isolate(t)
	dir := t.TempDir()
	testgit.Git(t, dir, "init", "-q")
	h := &history{t: t, dir: dir, gitDir: filepath.Join(dir, ".git"), index: filepath.Join(t.TempDir(), "index")}
	if err := Prepare(h.gitDir); err != nil {
		t.Fatal(err)
	}
	h.base = h.commit("", map[string]string{"f.txt": tenLines, "g.txt": "g\n"})
	return h
}

// git runs git in the history's repo against its temporary index.
func (h *history) git(stdin string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", h.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+h.index)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			stderr = exit.Stderr
		}
		h.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// commit creates a commit on parent ("" for a root) that keeps the parent's
// files and applies files on top.
func (h *history) commit(parent string, files map[string]string) string {
	h.t.Helper()
	if err := os.Remove(h.index); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.t.Fatal(err)
	}
	if parent != "" {
		h.git("", "read-tree", parent)
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if files[path] == deleted {
			h.git("", "update-index", "--force-remove", "--", path)
			continue
		}
		oid := h.git(files[path], "hash-object", "-w", "--stdin")
		h.git("", "update-index", "--add", "--cacheinfo", "100644,"+oid+","+path)
	}
	tree := h.git("", "write-tree")
	h.n++
	args := []string{"commit-tree", tree, "-m", fmt.Sprintf("c%d", h.n)}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return h.git("", args...)
}

// shared is the intersection of what each side changed from its base.
func (h *history) shared(a, b Side) []string {
	h.t.Helper()
	ca, err := Changed(h.t.Context(), h.gitDir, a.Base, a.Head)
	if err != nil {
		h.t.Fatal(err)
	}
	cb, err := Changed(h.t.Context(), h.gitDir, b.Base, b.Head)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, p := range ca {
		if slices.Contains(cb, p) {
			out = append(out, p)
		}
	}
	return out
}

func (h *history) check(a, b Side, shared []string) Result {
	h.t.Helper()
	res, err := Check(h.t.Context(), h.gitDir, a, b, shared)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// line returns body with line n (1-based) replaced by text.
func line(body string, n int, text string) string {
	lines := strings.SplitAfter(body, "\n")
	eol := ""
	if strings.HasSuffix(lines[n-1], "\n") {
		eol = "\n"
	}
	lines[n-1] = text + eol
	return strings.Join(lines, "")
}

// pair builds a and b on their bases, each changing f.txt line n of body.
func (h *history) pair(base, body string, n int) (a, b Side) {
	h.t.Helper()
	a = Side{base, h.commit(base, map[string]string{"f.txt": line(body, n, fmt.Sprintf("A%d", n))})}
	b = Side{base, h.commit(base, map[string]string{"f.txt": line(body, n, fmt.Sprintf("B%d", n))})}
	return a, b
}

func paths(res Result) []string {
	var out []string
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	return out
}

// at is the single line n.
func at(n int) []core.Range { return []core.Range{{Start: n, Len: 1}} }

func TestCheckSameBaseConflictLines(t *testing.T) {
	h := newHistory(t)
	a, b := h.pair(h.base, tenLines, 3)
	res := h.check(a, b, h.shared(a, b))
	want := []FileConflict{{Path: "f.txt", Kind: "content", Lines: [2][]core.Range{at(3), at(3)},
		Text: [2][]core.Snippet{{{Lines: []string{"A3"}}}, {{Lines: []string{"B3"}}}}}}
	if res.Status != Conflict || res.How != "same-base" || !reflect.DeepEqual(res.Files, want) {
		t.Fatalf("got %+v, want conflict same-base %+v", res, want)
	}
}

func TestCheckDifferentRegionsClean(t *testing.T) {
	h := newHistory(t)
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 2, "A2")})}
	b := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 9, "B9")})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Clean || len(res.Files) != 0 {
		t.Fatalf("got %+v, want clean", res)
	}
}

func TestCheckAdjacentLinesConflict(t *testing.T) {
	h := newHistory(t)
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 4, "A4")})}
	b := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 5, "B5")})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || !slices.Equal(paths(res), []string{"f.txt"}) {
		t.Fatalf("got %+v, want conflict in f.txt", res)
	}
}

func TestCheckIdenticalChangeClean(t *testing.T) {
	h := newHistory(t)
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 3, "X")})}
	b := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 3, "X")})}
	if res := h.check(a, b, h.shared(a, b)); res.Status != Clean {
		t.Fatalf("got %+v, want clean", res)
	}
}

// forwardPair is A on base changing line 8 and B on main1, which adds five
// lines on top of f.txt, changing the same original line, now 13.
func forwardPair(h *history) (a, b Side) {
	h.t.Helper()
	main1Body := "p1\np2\np3\np4\np5\n" + tenLines
	main1 := h.commit(h.base, map[string]string{"f.txt": main1Body})
	a = Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 8, "A8")})}
	b = Side{main1, h.commit(main1, map[string]string{"f.txt": line(main1Body, 13, "B13")})}
	return a, b
}

func TestCheckForwardMapsLines(t *testing.T) {
	h := newHistory(t)
	a, b := forwardPair(h)
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || res.How != "forward" || len(res.Files) != 1 {
		t.Fatalf("got %+v, want one forward conflict", res)
	}
	if got := res.Files[0].Lines; !reflect.DeepEqual(got, [2][]core.Range{at(8), at(13)}) {
		t.Fatalf("lines = %v, want [[{8 1}] [{13 1}]]", got)
	}
}

func TestCheckDriftOutsideSharedStillMeasured(t *testing.T) {
	h := newHistory(t)
	main1 := h.commit(h.base, map[string]string{"g.txt": "main\n"})
	a := Side{h.base, h.commit(h.base, map[string]string{"g.txt": "a\n", "f.txt": line(tenLines, 2, "A2")})}
	b := Side{main1, h.commit(main1, map[string]string{"f.txt": line(tenLines, 9, "B9")})}
	res := h.check(a, b, []string{"f.txt"})
	if res.Status != Clean || res.How != "forward" {
		t.Fatalf("got %+v, want clean forward", res)
	}
}

func TestCheckBackward(t *testing.T) {
	h := newHistory(t)
	main1Body := line(tenLines, 2, "M2")
	main1 := h.commit(h.base, map[string]string{"f.txt": main1Body})
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 2, "A2")})}
	b := Side{main1, h.commit(main1, map[string]string{"f.txt": line(main1Body, 8, "B8")})}
	res := h.check(a, b, []string{"f.txt"})
	if res.Status != Clean || res.How != "backward" {
		t.Fatalf("got %+v, want clean backward", res)
	}
}

func TestCheckStale(t *testing.T) {
	h := newHistory(t)
	main1Body := line(tenLines, 2, "M2")
	main1 := h.commit(h.base, map[string]string{"f.txt": main1Body})
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 2, "A2")})}
	b := Side{main1, h.commit(main1, map[string]string{"f.txt": line(main1Body, 2, "B2")})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Stale || res.How != "" || !slices.Equal(res.Stale, []string{"f.txt"}) {
		t.Fatalf("got %+v, want stale in f.txt", res)
	}
}

func TestCheckUnrelatedBases(t *testing.T) {
	h := newHistory(t)
	root1 := h.commit("", map[string]string{"f.txt": "one\n"})
	root2 := h.commit("", map[string]string{"f.txt": "two\n"})
	a := Side{root1, h.commit(root1, map[string]string{"f.txt": "a\n"})}
	b := Side{root2, h.commit(root2, map[string]string{"f.txt": "b\n"})}
	if res := h.check(a, b, h.shared(a, b)); res.Status != Stale {
		t.Fatalf("got %+v, want stale", res)
	}
}

func TestCheckAddAdd(t *testing.T) {
	h := newHistory(t)
	a := Side{h.base, h.commit(h.base, map[string]string{"n.txt": "a1\na2\n"})}
	b := Side{h.base, h.commit(h.base, map[string]string{"n.txt": "b1\nb2\n"})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || len(res.Files) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	if f := res.Files[0]; f.Kind != "content" || len(f.Lines[0]) == 0 || len(f.Lines[1]) == 0 {
		t.Fatalf("got %+v, want content with lines on both sides", f)
	}
}

func TestCheckModifyDelete(t *testing.T) {
	h := newHistory(t)
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": deleted})}
	b := Side{h.base, h.commit(h.base, map[string]string{"f.txt": line(tenLines, 3, "B3")})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || len(res.Files) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	if f := res.Files[0]; f.Kind != "file" || len(f.Lines[0]) != 0 || len(f.Lines[1]) != 0 {
		t.Fatalf("got %+v, want file conflict without lines", f)
	}
}

func TestCheckText(t *testing.T) {
	h := newHistory(t)
	a, b := h.pair(h.base, tenLines, 2)
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || len(res.Files) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	want := [2][]core.Snippet{{{Lines: []string{"A2"}}}, {{Lines: []string{"B2"}}}}
	if got := res.Files[0].Text; !reflect.DeepEqual(got, want) {
		t.Fatalf("text = %+v, want %+v", got, want)
	}
}

// TestCheckTextMore: past MaxSnippets blocks, each side counts the sections
// it left out.
func TestCheckTextMore(t *testing.T) {
	h := newHistory(t)
	body := func(side string) string { // lines 1, 3, 5 and 7 changed: four blocks
		s := tenLines
		for _, n := range []int{1, 3, 5, 7} {
			s = line(s, n, fmt.Sprintf("%s%d", side, n))
		}
		return s
	}
	a := Side{h.base, h.commit(h.base, map[string]string{"f.txt": body("A")})}
	b := Side{h.base, h.commit(h.base, map[string]string{"f.txt": body("B")})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || len(res.Files) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	f := res.Files[0]
	if len(f.Lines[0]) != 4 || len(f.Text[0]) != MaxSnippets || f.TextMore != [2]int{1, 1} {
		t.Fatalf("got %+v, want 4 blocks, %d sections and 1 more per side", f, MaxSnippets)
	}
	if want := []core.Snippet{{Lines: []string{"B1"}}, {Lines: []string{"B3"}}, {Lines: []string{"B5"}}}; !reflect.DeepEqual(f.Text[1], want) {
		t.Fatalf("b's text = %+v, want %+v", f.Text[1], want)
	}
}

// With a on the newer base the sides are swapped inside Check; Text must
// still follow the caller's order.
func TestCheckTextFollowsSwap(t *testing.T) {
	h := newHistory(t)
	older, newer := forwardPair(h)
	res := h.check(newer, older, h.shared(newer, older))
	if res.Status != Conflict || len(res.Files) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	want := [2][]core.Snippet{{{Lines: []string{"B13"}}}, {{Lines: []string{"A8"}}}}
	if got := res.Files[0].Text; !reflect.DeepEqual(got, want) {
		t.Fatalf("text = %+v, want %+v", got, want)
	}
}

func TestCheckSymmetric(t *testing.T) {
	h := newHistory(t)
	a, b := forwardPair(h)
	ab := h.check(a, b, h.shared(a, b))
	ba := h.check(b, a, h.shared(b, a))
	if ab.Status != Conflict || ba.Status != ab.Status || !slices.Equal(paths(ba), paths(ab)) {
		t.Fatalf("a,b = %+v; b,a = %+v", ab, ba)
	}
	for i := range ab.Files {
		want := [2][]core.Range{ab.Files[i].Lines[1], ab.Files[i].Lines[0]}
		if got := ba.Files[i].Lines; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: b,a lines = %v, want %v", ab.Files[i].Path, got, want)
		}
	}
}

func TestCheckMarkerLookalike(t *testing.T) {
	h := newHistory(t)
	body := line(tenLines, 6, "<<<<<<< note")
	base := h.commit(h.base, map[string]string{"f.txt": body})
	a, b := h.pair(base, body, 3)
	res := h.check(a, b, h.shared(a, b))
	want := []FileConflict{{Path: "f.txt", Kind: "content", Lines: [2][]core.Range{at(3), at(3)},
		Text: [2][]core.Snippet{{{Lines: []string{"A3"}}}, {{Lines: []string{"B3"}}}}}}
	if res.Status != Conflict || !reflect.DeepEqual(res.Files, want) {
		t.Fatalf("got %+v, want %+v", res, want)
	}
}

func TestCheckIsolatedConfig(t *testing.T) {
	h := newHistory(t)
	base := h.commit(h.base, map[string]string{".gitattributes": "f.txt merge=evil\n"})
	a, b := h.pair(base, tenLines, 3)

	// merge-tree reads no .gitattributes from the trees it merges, so the
	// evil config also points core.attributesFile at one; otherwise the
	// driver would never be reachable and the test would prove nothing.
	tmp := t.TempDir()
	ran := filepath.Join(tmp, "ran")
	attrs := filepath.Join(tmp, "attributes")
	testgit.WriteFile(t, attrs, "f.txt merge=evil\n")
	cfg := filepath.Join(tmp, "evil.gitconfig")
	testgit.WriteFile(t, cfg, "[core]\n\tattributesFile = "+attrs+"\n[merge \"evil\"]\n\tdriver = touch "+ran+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)

	res := h.check(a, b, h.shared(a, b))
	if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the user's merge driver ran (stat: %v)", err)
	}
	if res.Status != Conflict || !slices.Equal(paths(res), []string{"f.txt"}) {
		t.Fatalf("got %+v, want conflict in f.txt", res)
	}
}

// Config injected through the environment (GIT_CONFIG_COUNT and friends)
// must not reach the store's git either.
func TestCheckIsolatedEnvConfig(t *testing.T) {
	h := newHistory(t)
	base := h.commit(h.base, map[string]string{".gitattributes": "f.txt merge=evil\n"})
	a, b := h.pair(base, tenLines, 3)

	tmp := t.TempDir()
	ran := filepath.Join(tmp, "ran")
	attrs := filepath.Join(tmp, "attributes")
	testgit.WriteFile(t, attrs, "f.txt merge=evil\n")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.attributesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", attrs)
	t.Setenv("GIT_CONFIG_KEY_1", "merge.evil.driver")
	t.Setenv("GIT_CONFIG_VALUE_1", "touch "+ran)

	res := h.check(a, b, h.shared(a, b))
	if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a merge driver injected through the environment ran (stat: %v)", err)
	}
	if res.Status != Conflict || !slices.Equal(paths(res), []string{"f.txt"}) {
		t.Fatalf("got %+v, want conflict in f.txt", res)
	}
}

// The user's attributes must not reach the store's git either: neither the
// default core.attributesFile under XDG_CONFIG_HOME or HOME, which
// GIT_CONFIG_GLOBAL does not turn off, nor a tree named by GIT_ATTR_SOURCE.
// merge=union there would merge the conflict away.
func TestCheckIsolatedAttributes(t *testing.T) {
	const union = "* merge=union\n"
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, h *history)
	}{
		{"XDG_CONFIG_HOME", func(t *testing.T, h *history) {
			xdg := t.TempDir()
			testgit.WriteFile(t, filepath.Join(xdg, "git", "attributes"), union)
			t.Setenv("XDG_CONFIG_HOME", xdg)
		}},
		{"HOME", func(t *testing.T, h *history) {
			home := t.TempDir()
			testgit.WriteFile(t, filepath.Join(home, ".config", "git", "attributes"), union)
			t.Setenv("XDG_CONFIG_HOME", "") // git takes an empty one as unset
			t.Setenv("HOME", home)
		}},
		{"GIT_ATTR_SOURCE", func(t *testing.T, h *history) {
			t.Setenv("GIT_ATTR_SOURCE", h.commit("", map[string]string{".gitattributes": union}))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHistory(t)
			a, b := h.pair(h.base, tenLines, 3)
			shared := h.shared(a, b)
			c.setup(t, h)
			res := h.check(a, b, shared)
			if res.Status != Conflict || !slices.Equal(paths(res), []string{"f.txt"}) {
				t.Fatalf("got %+v, want conflict in f.txt", res)
			}
		})
	}
}

func TestCheckPathsWithSpacesAndUnicode(t *testing.T) {
	h := newHistory(t)
	const p = "dir with space/zoë.md"
	base := h.commit(h.base, map[string]string{p: tenLines})
	a := Side{base, h.commit(base, map[string]string{p: line(tenLines, 3, "A3")})}
	b := Side{base, h.commit(base, map[string]string{p: line(tenLines, 3, "B3")})}
	res := h.check(a, b, h.shared(a, b))
	if res.Status != Conflict || len(res.Files) == 0 || res.Files[0].Path != p {
		t.Fatalf("got %+v, want conflict in %q", res, p)
	}
}

func TestCheckNoTrailingNewline(t *testing.T) {
	h := newHistory(t)
	body := strings.TrimSuffix(tenLines, "\n")
	base := h.commit(h.base, map[string]string{"f.txt": body})
	a, b := h.pair(base, body, 10)
	res := h.check(a, b, h.shared(a, b))
	if len(res.Files) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	if got := res.Files[0].Lines; !reflect.DeepEqual(got, [2][]core.Range{at(10), at(10)}) {
		t.Fatalf("lines = %v, want [[{10 1}] [{10 1}]]", got)
	}
}

func TestPrepareIdempotent(t *testing.T) {
	gitDir := t.TempDir()
	path := filepath.Join(gitDir, "info", "attributes")
	for range 2 {
		if err := Prepare(gitDir); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "* conflict-marker-size=32\n" {
		t.Fatalf("fresh: got %q", got)
	}

	testgit.WriteFile(t, path, "*.bin binary")
	for range 2 {
		if err := Prepare(gitDir); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "*.bin binary\n* conflict-marker-size=32\n" {
		t.Fatalf("existing: got %q", got)
	}
}
