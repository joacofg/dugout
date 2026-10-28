package textual

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

func TestParseHunks(t *testing.T) {
	cases := []struct {
		name    string
		diff    string
		want    []Hunk
		wantErr bool
	}{
		{"single line", "@@ -3 +3 @@", []Hunk{{3, 1, 3, 1}}, false},
		{"insertion with trailer", "@@ -5,0 +6,2 @@ func x", []Hunk{{5, 0, 6, 2}}, false},
		{"deletion", "@@ -1,2 +0,0 @@", []Hunk{{1, 2, 0, 0}}, false},
		{"no hunks", "diff --git a/f b/f\n--- a/f\n+++ b/f\n", nil, false},
		{"bad number", "@@ -x +1 @@", nil, true},
		{
			"several, body lines ignored",
			"--- a\n+++ b\n@@ -1 +1 @@\n-a\n+b\n@@ -9,0 +10,3 @@\n+x\n+y\n+z\n",
			[]Hunk{{1, 1, 1, 1}, {9, 0, 10, 3}}, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseHunks(c.diff)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestMapToOld(t *testing.T) {
	hunks := []Hunk{{3, 1, 3, 1}, {10, 0, 11, 4}}
	cases := []struct {
		name string
		in   core.Range
		want core.Range
	}{
		{"before every hunk", core.Range{Start: 2, Len: 1}, core.Range{Start: 2, Len: 1}},
		{"inside a replacement", core.Range{Start: 3, Len: 1}, core.Range{Start: 3, Len: 1}},
		{"inside a pure insertion", core.Range{Start: 12, Len: 2}, core.Range{Start: 11, Len: 0}},
		{"after an insertion", core.Range{Start: 20, Len: 1}, core.Range{Start: 16, Len: 1}},
		{"point stays a point", core.Range{Start: 20, Len: 0}, core.Range{Start: 16, Len: 0}},
		{"spans outside and inside a hunk", core.Range{Start: 9, Len: 4}, core.Range{Start: 9, Len: 2}},
		{"spans two regions", core.Range{Start: 2, Len: 3}, core.Range{Start: 2, Len: 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MapToOld(hunks, c.in); got != c.want {
				t.Fatalf("MapToOld(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestMapToOldDeletionAndReplacedBlock(t *testing.T) {
	// Old lines 1-2 deleted, old line 6 becomes new lines 4-5.
	hunks := []Hunk{{1, 2, 0, 0}, {6, 1, 4, 2}}
	if got := MapToOld(hunks, core.Range{Start: 1, Len: 2}); got != (core.Range{Start: 3, Len: 2}) {
		t.Fatalf("after deletion: %v", got)
	}
	if got := MapToOld(hunks, core.Range{Start: 4, Len: 2}); got != (core.Range{Start: 6, Len: 1}) {
		t.Fatalf("inside replacement: %v", got)
	}
}

func marks(c string) string { return strings.Repeat(c, MarkerSize) }

func block(label string, ours, base, theirs string) string {
	return marks("<") + " " + label + "\n" + ours +
		marks("|") + " base\n" + base +
		marks("=") + "\n" + theirs +
		marks(">") + " other\n"
}

func TestResolve(t *testing.T) {
	merged := "head\n" +
		block("ours", "o1\no2\n", "b1\n", "t1\nt2\nt3\n") +
		"mid\n" +
		block("ours", "", "b\n", "T\n") +
		"tail"

	res, blocks, err := Resolve([]byte(merged), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := "head\no1\no2\nmid\ntail"; string(res) != want {
		t.Fatalf("side 0 text = %q, want %q", res, want)
	}
	if want := []core.Range{{Start: 2, Len: 2}, {Start: 5, Len: 0}}; !reflect.DeepEqual(blocks, want) {
		t.Fatalf("side 0 blocks = %v, want %v", blocks, want)
	}

	res, blocks, err = Resolve([]byte(merged), 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "head\nt1\nt2\nt3\nmid\nT\ntail"; string(res) != want {
		t.Fatalf("side 1 text = %q, want %q", res, want)
	}
	if want := []core.Range{{Start: 2, Len: 3}, {Start: 6, Len: 1}}; !reflect.DeepEqual(blocks, want) {
		t.Fatalf("side 1 blocks = %v, want %v", blocks, want)
	}
}

func TestResolveLookalikeMarkersAreText(t *testing.T) {
	merged := "a\n<<<<<<< note\nb\n" + marks("<") + "x\nc\n"
	res, blocks, err := Resolve([]byte(merged), 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != merged || len(blocks) != 0 {
		t.Fatalf("res = %q, blocks = %v", res, blocks)
	}
}

func TestResolveUnclosedBlock(t *testing.T) {
	merged := marks("<") + " ours\no\n" + marks("|") + " base\nb\n" + marks("=") + "\nt\n"
	if _, _, err := Resolve([]byte(merged), 0); err == nil {
		t.Fatal("expected an error for a block without closing marker")
	}
}

func TestResolveNoTrailingNewlineAndEmptyTheirs(t *testing.T) {
	merged := block("ours", "o\n", "b\n", "") + "end"
	res, blocks, err := Resolve([]byte(merged), 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != "end" {
		t.Fatalf("res = %q", res)
	}
	if want := []core.Range{{Start: 1, Len: 0}}; !reflect.DeepEqual(blocks, want) {
		t.Fatalf("blocks = %v, want %v", blocks, want)
	}
}

func TestSections(t *testing.T) {
	// numbered returns n lines "l1".."ln" starting after offset.
	numbered := func(from, n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString("l" + strconv.Itoa(from+i) + "\n")
		}
		return b.String()
	}
	want := func(from, n, more int) core.Snippet {
		s := core.Snippet{Lines: []string{}, More: more}
		for i := range n {
			s.Lines = append(s.Lines, "l"+strconv.Itoa(from+i))
		}
		return s
	}
	long, edge := strings.Repeat("ë", 300), strings.Repeat("ë", MaxSnippetRunes)
	cases := []struct {
		name   string
		body   string
		blocks []core.Range
		want   []core.Snippet
		more   int // sections left out
	}{
		{"1, 12 and 15 lines", numbered(1, 28),
			[]core.Range{{Start: 1, Len: 1}, {Start: 2, Len: 12}, {Start: 14, Len: 15}},
			[]core.Snippet{want(1, 1, 0), want(2, 12, 0), want(14, 12, 3)}, 0},
		{"empty section", "a\nb\n", []core.Range{{Start: 2, Len: 0}},
			[]core.Snippet{{Lines: []string{}}}, 0},
		{"four blocks keep three", numbered(1, 4),
			[]core.Range{{Start: 1, Len: 1}, {Start: 2, Len: 1}, {Start: 3, Len: 1}, {Start: 4, Len: 1}},
			[]core.Snippet{want(1, 1, 0), want(2, 1, 0), want(3, 1, 0)}, 1},
		{"a long line is cut with a marker", long + "\n" + edge + "\n", []core.Range{{Start: 1, Len: 2}},
			[]core.Snippet{{Lines: []string{strings.Repeat("ë", MaxSnippetRunes-1) + "…", edge}}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, more := Sections([]byte(c.body), c.blocks)
			if !reflect.DeepEqual(got, c.want) || more != c.more {
				t.Fatalf("got %+v and %d more, want %+v and %d more", got, more, c.want, c.more)
			}
		})
	}
}
