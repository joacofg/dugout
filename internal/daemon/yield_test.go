package daemon

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/joacofg/dugout/internal/core"
)

const (
	// first sorts before me, so it keeps every conflict it has with me and me yields.
	first core.WorkstreamID = "d0:first"
	// yieldRoleLine and keepRoleLine end a conflict's line (the answer's role marker).
	yieldRoleLine = "You yield."
	keepRoleLine  = "You keep it."
	alignSentence = "across the whole span shown"
	humanLine     = "Escalate it to both humans with office_escalate"
)

// conflictWith is a conflict on path between me and other, with me's lines at
// mine and the other side's text, as the daemon builds it.
func conflictWith(other core.WorkstreamID, path string, text ...core.Snippet) core.Conflict {
	c := core.Conflict{
		ID:   core.ConflictID("r", path, me, other),
		Repo: "r",
		Path: path,
		Kind: "content",
		Sides: [2]core.Side{
			{Daemon: "d1", Workstream: me, Branch: "mine"},
			{Daemon: "d0", Workstream: other, Branch: "theirs"},
		},
	}
	c.Lines = [2][]core.Range{{{Start: 3, Len: 2}}, {{Start: 3, Len: 2}}}
	if len(text) > 0 {
		c.Text[1] = text
	}
	if other < me {
		c.Sides[0], c.Sides[1] = c.Sides[1], c.Sides[0]
		c.Lines[0], c.Lines[1] = c.Lines[1], c.Lines[0]
		c.Text[0], c.Text[1] = c.Text[1], c.Text[0]
	}
	return c
}

// denyOf is ws's deny of an edit of cs[0]'s file, in a repo whose mode was
// never chosen.
func denyOf(t *testing.T, ws core.WorkstreamID, cs ...core.Conflict) string {
	t.Helper()
	return denyIn(t, "", ws, cs...)
}

// denyIn is denyOf in a repo in mode m.
func denyIn(t *testing.T, m Mode, ws core.WorkstreamID, cs ...core.Conflict) string {
	t.Helper()
	s := NewSession("s", "/repo", 0)
	s.Mode = m
	out := Respond("PreToolUse", "Edit", cs[0].Path, s, ws, View{Conflicts: cs})
	hso, _ := out["hookSpecificOutput"].(map[string]any)
	reason, _ := hso["permissionDecisionReason"].(string)
	if hso["permissionDecision"] != "deny" || reason == "" {
		t.Fatalf("%s: edit = %s, want deny", ws, asJSON(t, out))
	}
	return reason
}

// contextFor is ws's context after an edit, in a repo whose mode was never
// chosen.
func contextFor(t *testing.T, ws core.WorkstreamID, cs ...core.Conflict) string {
	t.Helper()
	return contextIn(t, "", ws, cs...)
}

// contextIn is contextFor in a repo in mode m.
func contextIn(t *testing.T, m Mode, ws core.WorkstreamID, cs ...core.Conflict) string {
	t.Helper()
	// Told of m already, as at its SessionStart.
	s := NewSession("s", "/repo", 0)
	s.Mode, s.modeTold = m, m
	out := Respond("PostToolUse", "", "", s, ws, View{Conflicts: cs})
	ctx := ctxOf(t, out)
	if ctx == "" {
		t.Fatalf("%s: no context for %s", ws, asJSON(t, out))
	}
	return ctx
}

// TestOneSideKeepsOtherYields: from both sides of the same conflict, through
// the hook answers, exactly one is told to keep and the other to yield; the
// same on every call.
func TestOneSideKeepsOtherYields(t *testing.T) {
	c := conflictWith(first, "x.go", core.Snippet{Lines: []string{"a := 1"}})
	for _, answer := range []func(*testing.T, core.WorkstreamID, ...core.Conflict) string{denyOf, contextFor} {
		for range 3 {
			mine, theirs := answer(t, me, c), answer(t, first, c)
			if !strings.Contains(mine, yieldRoleLine) || strings.Contains(mine, keepRoleLine) {
				t.Fatalf("%s should yield: %q", me, mine)
			}
			if !strings.Contains(theirs, keepRoleLine) || strings.Contains(theirs, yieldRoleLine) {
				t.Fatalf("%s should keep: %q", first, theirs)
			}
		}
	}
}

// TestDenyKeeperRetriesYielderAligns: in ModeScoped, a denied edit tells the
// keeper to retry, with no word of its human, and the yielder to retry aligned
// or without the part, within its task, escalating only if that would drop
// part of it.
func TestDenyKeeperRetriesYielderAligns(t *testing.T) {
	c := conflictWith(first, "x.go")
	keeper, yielder := denyIn(t, ModeScoped, first, c), denyIn(t, ModeScoped, me, c)
	if !strings.Contains(keeper, "retry the edit") || strings.Contains(keeper, humanLine) || strings.Contains(keeper, "You yield") {
		t.Fatalf("keeper deny = %q", keeper)
	}
	for _, want := range []string{"retry the edit aligned with the other one", "or without that span", "within the scope of your task", humanLine + " only if yielding would force you to leave part of your task undone"} {
		if !strings.Contains(yielder, want) {
			t.Fatalf("yielder deny lacks %q: %q", want, yielder)
		}
	}
	for _, ctx := range []string{contextFor(t, first, c), contextFor(t, me, c)} {
		if strings.Contains(ctx, "tell your human about the conflict") {
			t.Fatalf("the old line to the human is back: %q", ctx)
		}
	}
	if ctx := contextFor(t, first, c); strings.Contains(ctx, humanLine) {
		t.Fatalf("keeper context sends it to its human: %q", ctx)
	}
}

// TestAlignAdviceOnlyWhenYieldedLinesShown: the advice to align with the shown
// block is in the answer only when the other side's lines of a conflict ws
// yields are in it.
func TestAlignAdviceOnlyWhenYieldedLinesShown(t *testing.T) {
	snip := core.Snippet{Lines: []string{"a := 1"}}
	withText, without := conflictWith(first, "x.go", snip), conflictWith(first, "x.go")
	keptWithText := conflictWith("d2:later", "y.go", snip) // me keeps this one
	cases := []struct {
		name string
		cs   []core.Conflict
		want bool
	}{
		{"yielded, lines shown", []core.Conflict{withText}, true},
		{"yielded, no lines to show", []core.Conflict{without}, false},
		{"kept, lines shown", []core.Conflict{keptWithText}, false},
	}
	for _, tc := range cases {
		for name, got := range map[string]string{"deny": denyOf(t, me, tc.cs...), "context": contextFor(t, me, tc.cs...)} {
			if strings.Contains(got, alignSentence) != tc.want {
				t.Errorf("%s/%s: advice present = %v, want %v: %q", tc.name, name, !tc.want, tc.want, got)
			}
		}
	}
}

// TestMixedRolesTightRoom: me keeps one conflict and yields another, under
// every room from the smallest to the full answer: the answer never passes its
// room, and the align advice appears only when the yielded conflict's lines
// do, not when only the kept conflict's lines fit.
func TestMixedRolesTightRoom(t *testing.T) {
	long := func(tag string) core.Snippet {
		var ls []string
		for range 6 {
			ls = append(ls, tag+" := strings.Repeat(\"x\", 40)")
		}
		return core.Snippet{Lines: ls}
	}
	kept := conflictWith("d2:later", "a.go", long("kept"))
	yielded := conflictWith(first, "b.go", long("yielded"))
	cs := []core.Conflict{kept, yielded}
	base := utf8.RuneCountInString(RenderContext(me, View{}, false, ModeScoped))
	full := utf8.RuneCountInString(RenderContext(me, View{Conflicts: cs}, false, ModeScoped))
	sawKeptOnly, sawBoth := false, false
	for room := base; room <= full; room++ {
		ctx := renderContext(me, View{Conflicts: cs}, false, ModeScoped, room)
		if n := utf8.RuneCountInString(ctx); n > room {
			t.Fatalf("room %d: %d runes", room, n)
		}
		keptShown, yieldedShown := strings.Contains(ctx, "kept := "), strings.Contains(ctx, "yielded := ")
		if got := strings.Contains(ctx, alignSentence); got != yieldedShown {
			t.Fatalf("room %d: align advice = %v with the yielded lines shown = %v: %q", room, got, yieldedShown, ctx)
		}
		sawKeptOnly = sawKeptOnly || (keptShown && !yieldedShown && strings.Contains(ctx, yieldRoleLine))
		sawBoth = sawBoth || (keptShown && yieldedShown)
	}
	if !sawKeptOnly || !sawBoth {
		t.Fatalf("sweep never reached both layouts: keptOnly=%v both=%v", sawKeptOnly, sawBoth)
	}
}
