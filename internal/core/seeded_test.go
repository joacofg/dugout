package core

import (
	"math/rand/v2"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// randomEvents builds a seeded sequence of full states from 3 daemons with
// up to 3 workstreams each, drawing repos and files from small pools so
// overlaps are frequent.
func randomEvents(seed uint64, n int) []FullState {
	r := rand.New(rand.NewPCG(seed, seed))
	repos := []string{"r1", "r2"}
	files := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}
	daemons := []DaemonID{"d1", "d2", "d3"}
	events := make([]FullState, 0, n)
	for range n {
		d := daemons[r.IntN(len(daemons))]
		// Strictly increasing seq under one epoch, as one daemon run sends them.
		fs := FullState{Daemon: d, Epoch: 1, Seq: uint64(len(events)) + 1}
		for i := range r.IntN(4) {
			w := Workstream{
				ID:     WorkstreamID(string(d) + "-w" + strconv.Itoa(i)),
				Repo:   repos[r.IntN(len(repos))],
				Branch: "b" + strconv.Itoa(r.IntN(3)),
			}
			for _, f := range files {
				if r.IntN(3) == 0 {
					w.Files = append(w.Files, f)
				}
			}
			fs.Workstreams = append(fs.Workstreams, w)
		}
		events = append(events, fs)
	}
	return events
}

func runTrace(events []FullState) (State, [][]Effect) {
	s := NewState()
	trace := make([][]Effect, 0, len(events))
	for _, e := range events {
		var eff []Effect
		s, eff = Apply(s, e)
		trace = append(trace, eff)
	}
	return s, trace
}

// bruteForce compares every pair of workstreams in the last full state of
// each daemon, independently of the core's index.
func bruteForce(events []FullState) []Warning {
	last := map[DaemonID]FullState{}
	for _, e := range events {
		last[e.Daemon] = e
	}
	type entry struct {
		side Side
		repo string
		file string
	}
	var all []entry
	for d, fs := range last {
		for _, w := range fs.Workstreams {
			seen := map[string]bool{}
			for _, f := range w.Files {
				if seen[f] {
					continue
				}
				seen[f] = true
				all = append(all, entry{Side{Daemon: d, Workstream: w.ID, Branch: w.Branch}, w.Repo, f})
			}
		}
	}
	var out []Warning
	for i := range all {
		for j := range all {
			a, b := all[i], all[j]
			if a.repo != b.repo || a.file != b.file || a.side.Workstream >= b.side.Workstream {
				continue
			}
			out = append(out, Warning{
				ID:    WarningID(a.repo, a.file, a.side.Workstream, b.side.Workstream),
				Repo:  a.repo,
				Path:  a.file,
				Sides: [2]Side{a.side, b.side},
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if out == nil {
		out = []Warning{}
	}
	return out
}

func TestApplySeeded(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		events := randomEvents(seed, 40)
		final, trace := runTrace(events)
		_, again := runTrace(events)
		if !reflect.DeepEqual(trace, again) {
			t.Fatalf("seed %d: same events gave different effect traces", seed)
		}

		if got, want := final.Warnings(), bruteForce(events); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: warnings differ from brute force\n got %+v\nwant %+v", seed, got, want)
		}

		folded := map[DaemonID][]Warning{}
		for step, effects := range trace {
			for i, e := range effects {
				wc := e.(WarningsChanged)
				if i > 0 && effects[i-1].(WarningsChanged).Daemon >= wc.Daemon {
					t.Fatalf("seed %d step %d: effects not sorted by daemon", seed, step)
				}
				if !sort.SliceIsSorted(wc.Warnings, func(a, b int) bool { return wc.Warnings[a].ID < wc.Warnings[b].ID }) {
					t.Fatalf("seed %d step %d: warnings not sorted by id", seed, step)
				}
				folded[wc.Daemon] = wc.Warnings
			}
		}
		for _, d := range []DaemonID{"d1", "d2", "d3"} {
			want := final.WarningsFor(d)
			got, ok := folded[d]
			if !ok {
				got = []Warning{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d: folded effects for %s = %+v, state says %+v", seed, d, got, want)
			}
		}
	}
}
