// Package core is Dugout's deterministic state machine:
// Apply(state, event) -> (state, effects). It has no I/O, clock or
// randomness (guard_test.go enforces it), so every effect follows from the
// events alone. It detects file overlap between in-flight workstreams.
package core

import (
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
)

// DaemonID identifies one daemon across restarts.
type DaemonID string

// WorkstreamID identifies a (repo, worktree, branch) on one machine.
type WorkstreamID string

// Workstream is one unit of in-flight change. Files are repo-relative
// paths, already filtered by the repo's ignore list. Intent is what its agent
// declared it is doing, already sanitized by its daemon; "" when none.
// Person is the name of the human it works for, as the worktree's git
// user.name has it, so an escalation names both humans; "" when unset.
type Workstream struct {
	ID     WorkstreamID `json:"id"`
	Repo   string       `json:"repo"`
	Branch string       `json:"branch"`
	Files  []string     `json:"files"`
	Intent string       `json:"intent,omitempty"`
	Person string       `json:"person,omitempty"`
}

// Event is an input to Apply.
type Event interface{ isEvent() }

// Effect is an output of Apply for the I/O layer to carry out.
type Effect interface{ isEffect() }

// FullState is a daemon's complete set of workstreams. It replaces whatever
// the server held for that daemon.
type FullState struct {
	Daemon      DaemonID     `json:"daemon_id"`
	Workstreams []Workstream `json:"workstreams"`
	// Epoch grows with each start: statedir.NextEpoch gives the stored epoch
	// plus one, or the clock in milliseconds if that is more. Seq orders a
	// run's full states: an older one is ignored. Epoch 0 means the
	// daemon sends none: always applied.
	Epoch uint64 `json:"epoch,omitempty"`
	Seq   uint64 `json:"seq,omitempty"`
}

func (FullState) isEvent() {}

// Side is one of the two workstreams in a warning, with its intent and its
// human's name (Workstream.Person). Session is the name of the Claude Code
// session working on it, the address its agent is messaged at; only the
// daemon of that workstream knows it, and it never marshals.
type Side struct {
	Daemon     DaemonID     `json:"daemon_id"`
	Workstream WorkstreamID `json:"workstream_id"`
	Branch     string       `json:"branch"`
	Intent     string       `json:"intent,omitempty"`
	Person     string       `json:"person,omitempty"`
	Session    string       `json:"-"`
}

// Warning says two workstreams of the same repo both have Path in flight.
// Sides are sorted by Workstream.
type Warning struct {
	ID    string  `json:"id"`
	Repo  string  `json:"repo"`
	Path  string  `json:"path"`
	Sides [2]Side `json:"sides"`
}

// WarningsChanged carries the new complete set of warnings that touch any
// workstream of Daemon, sorted by ID. Warnings is never nil.
type WarningsChanged struct {
	Daemon   DaemonID
	Warnings []Warning
}

func (WarningsChanged) isEffect() {}

// State is the server's view of every daemon. Apply never mutates a State
// it receives; workstream slices inside it are treated as immutable.
type State struct {
	daemons  map[DaemonID][]Workstream
	clocks   map[DaemonID]Clock // daemons that ever sent an epoch
	warnings []Warning          // sorted by ID
}

// Clock orders one daemon's messages of one kind: the (epoch, seq) of
// the last one admitted. A daemon's epoch grows with each run and its seq
// within a run, so a message is newer when its epoch is higher, or equal
// with a higher seq. The zero Clock has admitted nothing.
type Clock struct{ epoch, seq uint64 }

// Admit reports whether a message stamped (epoch, seq) is newer than every
// one admitted before, and returns the clock after it. Epoch 0 is a sender
// that stamps none: always admitted, the clock untouched.
func (c Clock) Admit(epoch, seq uint64) (Clock, bool) {
	if epoch == 0 {
		return c, true
	}
	if epoch < c.epoch || (epoch == c.epoch && seq <= c.seq) {
		return c, false
	}
	return Clock{epoch: epoch, seq: seq}, true
}

// Epoch is the epoch of the last message admitted, 0 before any.
func (c Clock) Epoch() uint64 { return c.epoch }

// NewState returns an empty State.
func NewState() State {
	return State{daemons: map[DaemonID][]Workstream{}, clocks: map[DaemonID]Clock{}, warnings: []Warning{}}
}

// Apply returns the state after e and the effects it causes, sorted by daemon.
func Apply(s State, e Event) (State, []Effect) {
	switch e := e.(type) {
	case FullState:
		return applyFullState(s, e)
	default:
		return s, nil
	}
}

func applyFullState(s State, e FullState) (State, []Effect) {
	c, ok := s.clocks[e.Daemon].Admit(e.Epoch, e.Seq)
	if !ok {
		return s, nil
	}
	next := State{
		daemons: make(map[DaemonID][]Workstream, len(s.daemons)+1),
		clocks:  make(map[DaemonID]Clock, len(s.clocks)+1),
	}
	for d, ws := range s.daemons {
		next.daemons[d] = ws
	}
	for d, k := range s.clocks {
		next.clocks[d] = k
	}
	if e.Epoch != 0 {
		next.clocks[e.Daemon] = c
	}
	if ws := normalize(e.Workstreams); len(ws) > 0 {
		next.daemons[e.Daemon] = ws
	} else {
		delete(next.daemons, e.Daemon)
	}
	next.warnings = computeWarnings(next.daemons)

	affected := map[DaemonID]bool{e.Daemon: true}
	for _, list := range [][]Warning{s.warnings, next.warnings} {
		for _, w := range list {
			affected[w.Sides[0].Daemon] = true
			affected[w.Sides[1].Daemon] = true
		}
	}
	var effects []Effect
	for _, d := range sortedKeys(affected) {
		after := next.WarningsFor(d)
		if !slices.Equal(s.WarningsFor(d), after) {
			effects = append(effects, WarningsChanged{Daemon: d, Warnings: after})
		}
	}
	return next, effects
}

// normalize copies ws sorted by ID, without duplicate IDs, with each file
// list sorted and deduplicated.
func normalize(ws []Workstream) []Workstream {
	out := make([]Workstream, 0, len(ws))
	for _, w := range ws {
		files := slices.Clone(w.Files)
		slices.Sort(files)
		w.Files = slices.Compact(files)
		out = append(out, w)
	}
	slices.SortStableFunc(out, func(a, b Workstream) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return slices.CompactFunc(out, func(a, b Workstream) bool { return a.ID == b.ID })
}

type fileKey struct{ repo, path string }

func computeWarnings(daemons map[DaemonID][]Workstream) []Warning {
	index := map[fileKey][]Side{}
	for _, d := range sortedKeys(daemons) {
		for _, w := range daemons[d] {
			side := Side{Daemon: d, Workstream: w.ID, Branch: w.Branch, Intent: w.Intent, Person: w.Person}
			for _, f := range w.Files {
				k := fileKey{w.Repo, f}
				index[k] = append(index[k], side)
			}
		}
	}
	warnings := []Warning{}
	for k, sides := range index {
		slices.SortFunc(sides, func(a, b Side) int { return strings.Compare(string(a.Workstream), string(b.Workstream)) })
		for i := range sides {
			for j := i + 1; j < len(sides); j++ {
				if sides[i].Workstream == sides[j].Workstream {
					continue
				}
				warnings = append(warnings, Warning{
					ID:    WarningID(k.repo, k.path, sides[i].Workstream, sides[j].Workstream),
					Repo:  k.repo,
					Path:  k.path,
					Sides: [2]Side{sides[i], sides[j]},
				})
			}
		}
	}
	slices.SortFunc(warnings, func(a, b Warning) int { return strings.Compare(a.ID, b.ID) })
	return warnings
}

// WarningID is stable for a (repo, path, pair of workstreams), whatever the
// order of a and b, so a re-sent full state yields the same warning.
func WarningID(repo, path string, a, b WorkstreamID) string {
	if b < a {
		a, b = b, a
	}
	h := fnv.New64a()
	for _, part := range []string{repo, path, string(a), string(b)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	id := strconv.FormatUint(h.Sum64(), 16)
	return strings.Repeat("0", 16-len(id)) + id
}

// Warnings returns every current warning, sorted by ID.
func (s State) Warnings() []Warning {
	return slices.Clone(s.warnings)
}

// WarningsFor returns the warnings that touch any workstream of d, sorted
// by ID. The result is never nil.
func (s State) WarningsFor(d DaemonID) []Warning {
	out := []Warning{}
	for _, w := range s.warnings {
		if w.Sides[0].Daemon == d || w.Sides[1].Daemon == d {
			out = append(out, w)
		}
	}
	return out
}

// Workstreams returns every workstream of every daemon, sorted by ID.
func (s State) Workstreams() []Workstream {
	var out []Workstream
	for _, ws := range s.daemons {
		out = append(out, ws...)
	}
	slices.SortFunc(out, func(a, b Workstream) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// Clock is the clock of daemon d's full states.
func (s State) Clock(d DaemonID) Clock { return s.clocks[d] }

// Owner returns the daemon that reported workstream ws.
func (s State) Owner(ws WorkstreamID) (DaemonID, bool) {
	for d, list := range s.daemons {
		for _, w := range list {
			if w.ID == ws {
				return d, true
			}
		}
	}
	return "", false
}

func sortedKeys[V any](m map[DaemonID]V) []DaemonID {
	keys := make([]DaemonID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
