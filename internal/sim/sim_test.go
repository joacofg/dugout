package sim

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

// seeds is how many seeds TestSimSeeds runs: 200, or DUGOUT_SIM_SEEDS.
func seeds(t *testing.T) uint64 {
	t.Helper()
	v := os.Getenv("DUGOUT_SIM_SEEDS")
	if v == "" {
		return 200
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		t.Fatalf("DUGOUT_SIM_SEEDS=%q: want a positive number", v)
	}
	return n
}

// tail is the last n lines of a trace, for a failure message.
func tail(trace []string, n int) string {
	return strings.Join(trace[max(len(trace)-n, 0):], "\n")
}

func TestSimSeeds(t *testing.T) {
	n := seeds(t)
	for seed := uint64(1); seed <= n; seed++ {
		if res := Run(seed, DefaultConfig()); len(res.Violations) > 0 {
			t.Fatalf("seed %d broke the invariants:\n%s\n\nlast 50 lines of its trace:\n%s",
				seed, strings.Join(res.Violations, "\n"), tail(res.Trace, 50))
		}
	}
	t.Logf("%d seeds, no violations", n)
}

func TestSimDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	a, b := Run(42, cfg), Run(42, cfg)
	if !slices.Equal(a.Trace, b.Trace) || !slices.Equal(a.Violations, b.Violations) {
		for i := range min(len(a.Trace), len(b.Trace)) {
			if a.Trace[i] != b.Trace[i] {
				t.Fatalf("seed 42 ran twice gave different traces, first at line %d:\n%s\n%s", i, a.Trace[i], b.Trace[i])
			}
		}
		t.Fatalf("seed 42 ran twice gave different traces: %d and %d lines, violations %q and %q",
			len(a.Trace), len(b.Trace), a.Violations, b.Violations)
	}
	if len(a.Trace) < cfg.Steps {
		t.Fatalf("trace has %d lines for %d steps, want one per step", len(a.Trace), cfg.Steps)
	}
	if other := Run(43, cfg); slices.Equal(a.Trace, other.Trace) {
		t.Fatal("seeds 42 and 43 gave the same trace: the run ignores its seed")
	}
}

// TestSimCatchesStaleOverwrite: with serve taking every message whatever its
// epoch and seq, a stale full state or conflict set that lands last stays.
// The invariants checked once the system settled show it on some seed, and
// invariant 7, checked at every message against the order they were posted
// in, on most. The sim catches what the (epoch, seq) order fixes.
func TestSimCatchesStaleOverwrite(t *testing.T) {
	unordered := func(s core.State, e core.Event) (core.State, []core.Effect) {
		if fs, ok := e.(core.FullState); ok {
			fs.Epoch, fs.Seq = 0, 0
			e = fs
		}
		return core.Apply(s, e)
	}
	blind := func(c core.Clock, _, _ uint64) (core.Clock, bool) { return c.Admit(0, 0) }
	broke := func(violations []string, prefixes ...string) bool {
		return slices.ContainsFunc(violations, func(v string) bool {
			return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(v, p) })
		})
	}
	for _, tc := range []struct {
		name    string
		inject  func(*world)
		settled []string // the invariants, by prefix, any of which shows the overwrite at the end
	}{
		{"full states", func(w *world) { w.apply = unordered }, []string{"invariant 1:", "invariant 2:", "invariant 4:"}},
		{"conflicts", func(w *world) { w.admit = blind }, []string{"invariant 4:", "invariant 6:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settled, each := 0, 0
			for seed := uint64(1); seed <= 200; seed++ {
				w := newWorld(seed, DefaultConfig())
				tc.inject(w)
				violations := w.run().Violations
				if broke(violations, tc.settled...) {
					settled++
				}
				if broke(violations, "invariant 7:") {
					each++
				}
			}
			t.Logf("of seeds 1..200, %d broke %v and %d invariant 7", settled, tc.settled, each)
			if settled == 0 {
				t.Errorf("no seed in 1..200 broke %v with serve ignoring epoch and seq", tc.settled)
			}
			if each <= 100 {
				t.Errorf("invariant 7 caught %d of seeds 1..200, want most", each)
			}
		})
	}
}
