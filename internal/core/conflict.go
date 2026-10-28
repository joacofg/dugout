package core

import (
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
)

// Range is a span of lines, 1-based. Len == 0 is a point before line Start.
type Range struct {
	Start int `json:"start"`
	Len   int `json:"len"`
}

// Snippet is one side's section of a conflict block: its lines, capped, and
// how many more lines the section had.
type Snippet struct {
	Lines []string `json:"lines"`
	More  int      `json:"more,omitempty"`
}

// Stance is what one side of a conflict told the office registry about its
// span: nothing, that it keeps it (claim) or that it gives it to the other
// side (yield).
type Stance string

const (
	StanceNone  Stance = ""
	StanceClaim Stance = "claim"
	StanceYield Stance = "yield"
)

// Conflict says two workstreams of the same repo cannot merge cleanly in
// Path. Kind is "content" or "file". Sides are sorted by Workstream, as in
// Warning; Lines[i] are the lines of Sides[i] and Text[i] the text of its
// sections, one Snippet per block (the first few), with TextMore[i] counting
// the sections left out. Text and TextMore are teammates' code for the local
// agent: they never marshal. Stances[i] is what Sides[i] told this machine's
// registry; it never marshals either.
type Conflict struct {
	ID       string       `json:"id"`
	Repo     string       `json:"repo"`
	Path     string       `json:"path"`
	Kind     string       `json:"kind"`
	Sides    [2]Side      `json:"sides"`
	Lines    [2][]Range   `json:"lines"`
	Text     [2][]Snippet `json:"-"`
	TextMore [2]int       `json:"-"`
	Stances  [2]Stance    `json:"-"`
}

// Owner is the workstream of c whose lines stay, after what its sides told
// the registry. A side has a case for the span when it claims it or the other
// side yields it; when exactly one side has one, it keeps the span. When both
// or neither do (both claimed at once, both yielded because their messages
// crossed, nobody said anything), Keeper decides, so the outcome hangs on the
// stances alone, never on the order they came in.
func (c Conflict) Owner() WorkstreamID {
	wants := func(i int) bool { return c.Stances[i] == StanceClaim || c.Stances[1-i] == StanceYield }
	switch a, b := wants(0), wants(1); {
	case a && !b:
		return c.Sides[0].Workstream
	case b && !a:
		return c.Sides[1].Workstream
	}
	return c.Keeper()
}

// OwnerWhy says what decided the owner of a conflict's span.
type OwnerWhy string

const (
	OwnerClaimed  OwnerWhy = "claimed"  // the owner claimed it
	OwnerYielded  OwnerWhy = "yielded"  // the other side yielded it, and the owner said nothing
	OwnerDisputed OwnerWhy = "disputed" // both claimed or both yielded: Keeper decided
	OwnerRule     OwnerWhy = "rule"     // nobody said anything: Keeper decided
)

// Why is what decided Owner: the owner's claim, the other side's yield, or
// Keeper when both or neither side has a case for the span.
func (c Conflict) Why() OwnerWhy {
	wants := func(i int) bool { return c.Stances[i] == StanceClaim || c.Stances[1-i] == StanceYield }
	for i := range c.Sides {
		if wants(i) && !wants(1-i) {
			if c.Stances[i] == StanceClaim {
				return OwnerClaimed
			}
			return OwnerYielded
		}
	}
	if c.Stances[0] != StanceNone || c.Stances[1] != StanceNone {
		return OwnerDisputed
	}
	return OwnerRule
}

// Keeper is the workstream of c whose lines stay as they are: the other
// side's agent aligns its change to them or takes them out of it. It is the
// side whose workstream sorts first, whatever the order of Sides. The pair of
// workstreams is what ID hashes, so both daemons of a conflict always name
// the same keeper, and it does not change while the conflict lasts, however
// its lines, branches or intents move. The order is total, so three or more
// workstreams on one file never yield to each other in a circle.
func (c Conflict) Keeper() WorkstreamID {
	return min(c.Sides[0].Workstream, c.Sides[1].Workstream)
}

// ConflictID is stable for a (repo, path, pair of workstreams), whatever the
// order of a and b. It hashes like WarningID but with a "conflict" prefix, so
// the two never collide for the same arguments.
func ConflictID(repo, path string, a, b WorkstreamID) string {
	if b < a {
		a, b = b, a
	}
	h := fnv.New64a()
	for _, part := range []string{"conflict", repo, path, string(a), string(b)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	id := strconv.FormatUint(h.Sum64(), 16)
	return strings.Repeat("0", 16-len(id)) + id
}

// Pair is a pair of workstreams of one repo whose overlapping Files are worth
// a merge check. Sides are sorted by Workstream; Files sorted, no duplicates.
type Pair struct {
	Repo  string
	Sides [2]Side
	Files []string
}

// CandidatePairs groups warnings by workstream pair, keeping the pairs where
// at least one side belongs to daemon d, with the union of their paths.
// Sorted by (Sides[0].Workstream, Sides[1].Workstream).
//
// A pair that shares only ignored files is never checked: conflicts in
// ignored files are rare enough not to be worth a merge check.
func CandidatePairs(warnings []Warning, d DaemonID) []Pair {
	type key struct {
		repo string
		ws0  WorkstreamID
		ws1  WorkstreamID
	}
	index := map[key]*Pair{}
	var pairs []*Pair
	for _, w := range warnings {
		if w.Sides[0].Daemon != d && w.Sides[1].Daemon != d {
			continue
		}
		k := key{w.Repo, w.Sides[0].Workstream, w.Sides[1].Workstream}
		p, ok := index[k]
		if !ok {
			p = &Pair{Repo: w.Repo, Sides: w.Sides}
			index[k] = p
			pairs = append(pairs, p)
		}
		p.Files = append(p.Files, w.Path)
	}
	slices.SortFunc(pairs, func(a, b *Pair) int {
		if c := strings.Compare(string(a.Sides[0].Workstream), string(b.Sides[0].Workstream)); c != 0 {
			return c
		}
		return strings.Compare(string(a.Sides[1].Workstream), string(b.Sides[1].Workstream))
	})
	out := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		slices.Sort(p.Files)
		p.Files = slices.Compact(p.Files)
		out = append(out, *p)
	}
	return out
}
