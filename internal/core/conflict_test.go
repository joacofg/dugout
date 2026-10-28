package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConflictIDStableAndDistinct(t *testing.T) {
	a, b := WorkstreamID("d1:a"), WorkstreamID("d1:b")
	if ConflictID("r", "x.go", a, b) != ConflictID("r", "x.go", b, a) {
		t.Fatal("ConflictID depends on side order")
	}
	if ConflictID("r", "x.go", a, b) == WarningID("r", "x.go", a, b) {
		t.Fatal("ConflictID collides with WarningID")
	}
	if len(ConflictID("r", "x.go", a, b)) != 16 {
		t.Fatal("ConflictID is not 16 hex chars")
	}
}

func TestCandidatePairs(t *testing.T) {
	w := func(path string, x, y Side) Warning {
		return Warning{ID: WarningID("r", path, x.Workstream, y.Workstream), Repo: "r", Path: path, Sides: [2]Side{x, y}}
	}
	a, b := Side{Daemon: "d1", Workstream: "d1:a"}, Side{Daemon: "d1", Workstream: "d1:b"}
	c := Side{Daemon: "d2", Workstream: "d2:c"}
	x, y := Side{Daemon: "d3", Workstream: "d3:x"}, Side{Daemon: "d3", Workstream: "d3:y"}
	got := CandidatePairs([]Warning{w("y.go", a, b), w("z.go", a, c), w("x.go", a, b), w("q.go", x, y), w("x.go", a, b)}, "d1")
	want := []Pair{
		{Repo: "r", Sides: [2]Side{a, b}, Files: []string{"x.go", "y.go"}},
		{Repo: "r", Sides: [2]Side{a, c}, Files: []string{"z.go"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CandidatePairs = %+v, want %+v", got, want)
	}
}

// TestConflictTextNotMarshaled: a conflict's text is teammates' code, for
// the local agent only; it never goes out in the conflict's JSON.
func TestConflictTextNotMarshaled(t *testing.T) {
	c := Conflict{ID: "0123456789abcdef", Repo: "r", Path: "x.go", Kind: "content",
		Text: [2][]Snippet{{{Lines: []string{"secret of a"}}}, {{Lines: []string{"secret of b"}, More: 2}}}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["text"]; ok || strings.Contains(string(b), "secret") {
		t.Fatalf("Conflict JSON carries the text: %s", b)
	}
}

// TestKeeperDeterministic: both daemons of a conflict name the same keeper,
// whatever the order they hold its sides in and whatever else of it moved;
// it is the workstream that sorts first, so daemons order before workstreams.
func TestKeeperDeterministic(t *testing.T) {
	a := Side{Daemon: "d1", Workstream: "d1:ffff", Branch: "a", Intent: "one"}
	b := Side{Daemon: "d2", Workstream: "d2:0000", Branch: "b"}
	c := Conflict{ID: ConflictID("r", "x.go", a.Workstream, b.Workstream), Repo: "r", Path: "x.go", Kind: "content",
		Sides: [2]Side{a, b}, Lines: [2][]Range{{{Start: 3, Len: 1}}, {{Start: 9, Len: 2}}}}
	if got := c.Keeper(); got != a.Workstream {
		t.Fatalf("Keeper = %s, want %s", got, a.Workstream)
	}
	swapped := c
	swapped.Sides = [2]Side{b, a}
	swapped.Lines = [2][]Range{c.Lines[1], c.Lines[0]}
	if swapped.Keeper() != c.Keeper() {
		t.Fatal("Keeper depends on side order")
	}
	moved := c
	moved.Kind = "file"
	moved.Lines = [2][]Range{}
	moved.Sides[0].Branch, moved.Sides[0].Intent, moved.Sides[1].Intent = "renamed", "", "other"
	if moved.Keeper() != c.Keeper() {
		t.Fatal("Keeper changed with the conflict's lines, kind, branch or intents")
	}
}

// TestKeeperTotal: with three workstreams on one file, the one that keeps
// against both others is the same for every pair, so no one yields in a
// circle.
func TestKeeperTotal(t *testing.T) {
	ws := []WorkstreamID{"d2:a", "d1:z", "d1:b"}
	pair := func(x, y WorkstreamID) Conflict {
		return Conflict{ID: ConflictID("r", "x.go", x, y), Sides: [2]Side{{Workstream: x}, {Workstream: y}}}
	}
	keeps := map[WorkstreamID]int{}
	for i := range ws {
		for j := i + 1; j < len(ws); j++ {
			keeps[pair(ws[i], ws[j]).Keeper()]++
		}
	}
	if keeps["d1:b"] != 2 || keeps["d1:z"] != 1 || keeps["d2:a"] != 0 {
		t.Fatalf("keeps = %v, want d1:b twice, d1:z once, d2:a never", keeps)
	}
}

// TestOwnerFromStances: who keeps a conflict follows only from what each side
// said through the office registry, never from the order they said it in;
// when both said the same, or nothing, the keeper rule decides.
func TestOwnerFromStances(t *testing.T) {
	k, y := Side{Workstream: "d1:a"}, Side{Workstream: "d1:b"} // k keeps by the rule
	cases := []struct {
		k, y Stance
		want WorkstreamID
	}{
		{StanceNone, StanceNone, k.Workstream},
		{StanceNone, StanceClaim, y.Workstream},
		{StanceYield, StanceNone, y.Workstream},
		{StanceClaim, StanceNone, k.Workstream},
		{StanceNone, StanceYield, k.Workstream},
		{StanceYield, StanceClaim, y.Workstream},
		{StanceClaim, StanceYield, k.Workstream},
		{StanceClaim, StanceClaim, k.Workstream}, // both claim at once: the fixed tiebreaker
		{StanceYield, StanceYield, k.Workstream}, // both yield, their messages crossed: the same
	}
	for _, tc := range cases {
		for _, sides := range [][2]Side{{k, y}, {y, k}} {
			c := Conflict{Sides: sides}
			for i, s := range sides {
				if s == k {
					c.Stances[i] = tc.k
				} else {
					c.Stances[i] = tc.y
				}
			}
			if got := c.Owner(); got != tc.want {
				t.Errorf("keeper says %q, yielder %q, sides %v: Owner = %s, want %s", tc.k, tc.y, sides, got, tc.want)
			}
		}
	}
}

// TestStancesNotMarshaled: the registry is this machine's; a conflict's
// stances never go out in its JSON.
func TestStancesNotMarshaled(t *testing.T) {
	b, err := json.Marshal(Conflict{ID: "0123456789abcdef", Stances: [2]Stance{StanceClaim, StanceYield}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "claim") || strings.Contains(string(b), "yield") {
		t.Fatalf("Conflict JSON carries the stances: %s", b)
	}
	b, _ = json.Marshal(Side{Workstream: "d1:a", Session: "a-12"})
	if strings.Contains(string(b), "a-12") {
		t.Fatalf("Side JSON carries the session: %s", b)
	}
}

// TestConflictWhy: the reason a side keeps the span agrees with Owner, and
// says whether a claim, a yield or the fixed rule decided it.
func TestConflictWhy(t *testing.T) {
	k, y := Side{Workstream: "d1:a"}, Side{Workstream: "d1:b"} // k keeps by the rule
	cases := []struct {
		k, y Stance
		want OwnerWhy
	}{
		{StanceNone, StanceNone, OwnerRule},
		{StanceClaim, StanceNone, OwnerClaimed},
		{StanceNone, StanceClaim, OwnerClaimed},
		{StanceNone, StanceYield, OwnerYielded},
		{StanceYield, StanceNone, OwnerYielded},
		{StanceClaim, StanceYield, OwnerClaimed},
		{StanceYield, StanceClaim, OwnerClaimed},
		{StanceClaim, StanceClaim, OwnerDisputed},
		{StanceYield, StanceYield, OwnerDisputed},
	}
	for _, tc := range cases {
		for _, sides := range [][2]Side{{k, y}, {y, k}} {
			c := Conflict{Sides: sides}
			for i, s := range sides {
				if s == k {
					c.Stances[i] = tc.k
				} else {
					c.Stances[i] = tc.y
				}
			}
			if got := c.Why(); got != tc.want {
				t.Errorf("keeper says %q, yielder %q, sides %v: Why = %s, want %s", tc.k, tc.y, sides, got, tc.want)
			}
		}
	}
}
