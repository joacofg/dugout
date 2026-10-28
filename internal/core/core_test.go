package core

import (
	"reflect"
	"testing"
)

func fullState(d DaemonID, ws ...Workstream) FullState {
	return FullState{Daemon: d, Workstreams: ws}
}

func ws(id WorkstreamID, repo string, files ...string) Workstream {
	return Workstream{ID: id, Repo: repo, Branch: "br-" + string(id), Files: files}
}

func applyAll(t *testing.T, events ...Event) (State, []Effect) {
	t.Helper()
	s := NewState()
	var last []Effect
	for _, e := range events {
		s, last = Apply(s, e)
	}
	return s, last
}

func TestOverlapSameRepo(t *testing.T) {
	s, _ := applyAll(t, fullState("d1", ws("a", "r", "x.go", "y.go")))
	s, effects := Apply(s, fullState("d2", ws("b", "r", "y.go")))

	warnings := s.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("want 1 warning, got %d: %+v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.Path != "y.go" || w.Repo != "r" {
		t.Fatalf("warning on wrong file: %+v", w)
	}
	if w.ID != WarningID("r", "y.go", "a", "b") {
		t.Fatalf("unstable id %q", w.ID)
	}
	want := []Effect{
		WarningsChanged{Daemon: "d1", Warnings: []Warning{w}},
		WarningsChanged{Daemon: "d2", Warnings: []Warning{w}},
	}
	if !reflect.DeepEqual(effects, want) {
		t.Fatalf("effects:\n got %+v\nwant %+v", effects, want)
	}
}

func TestWarningSidesSorted(t *testing.T) {
	s, _ := applyAll(t,
		fullState("d2", ws("b", "r", "y.go")),
		fullState("d1", ws("a", "r", "y.go")),
	)
	w := s.Warnings()[0]
	if w.Sides[0].Workstream != "a" || w.Sides[1].Workstream != "b" {
		t.Fatalf("sides not sorted by workstream: %+v", w.Sides)
	}
	if w.Sides[0].Daemon != "d1" || w.Sides[0].Branch != "br-a" {
		t.Fatalf("side a lost its daemon or branch: %+v", w.Sides[0])
	}
}

// TestComputeWarningsCarriesIntent: each side of a warning carries the intent
// of its workstream, so the other daemon can show it. A changed intent is a
// changed warning for both daemons, under the same ID.
func TestComputeWarningsCarriesIntent(t *testing.T) {
	a, b := ws("a", "r", "x.go"), ws("b", "r", "x.go")
	a.Intent = "fixing login"
	s, _ := applyAll(t, fullState("d1", a), fullState("d2", b))
	w := s.Warnings()
	if len(w) != 1 || w[0].Sides[0].Intent != "fixing login" || w[0].Sides[1].Intent != "" {
		t.Fatalf("want one warning with a's intent on its side and none on b's, got %+v", w)
	}
	id := w[0].ID
	b.Intent = "migrating the payments"
	s, effects := Apply(s, fullState("d2", b))
	w = s.Warnings()
	if len(w) != 1 || w[0].ID != id || w[0].Sides[1].Intent != "migrating the payments" {
		t.Fatalf("after b declared: want the same warning id %s with b's intent, got %+v", id, w)
	}
	if len(effects) != 2 {
		t.Fatalf("a changed intent should reach both daemons, got %+v", effects)
	}
}

// TestComputeWarningsCarriesPerson: each side names its human, so an
// escalation can name both.
func TestComputeWarningsCarriesPerson(t *testing.T) {
	a, b := ws("a", "r", "x.go"), ws("b", "r", "x.go")
	a.Person, b.Person = "Ana", "Bruno"
	s, _ := applyAll(t, fullState("d1", a), fullState("d2", b))
	w := s.Warnings()
	if len(w) != 1 || w[0].Sides[0].Person != "Ana" || w[0].Sides[1].Person != "Bruno" {
		t.Fatalf("want one warning naming Ana and Bruno, got %+v", w)
	}
}

func TestNoOverlapAcrossRepos(t *testing.T) {
	s, _ := applyAll(t,
		fullState("d1", ws("a", "r1", "x.go")),
		fullState("d2", ws("b", "r2", "x.go")),
	)
	if got := s.Warnings(); len(got) != 0 {
		t.Fatalf("want no warnings across repos, got %+v", got)
	}
}

func TestOverlapSameDaemon(t *testing.T) {
	s, effects := applyAll(t, fullState("d1", ws("a", "r", "z.go"), ws("b", "r", "z.go")))
	if got := s.Warnings(); len(got) != 1 || got[0].Path != "z.go" {
		t.Fatalf("want one warning on z.go, got %+v", got)
	}
	if len(effects) != 1 || effects[0].(WarningsChanged).Daemon != "d1" {
		t.Fatalf("want a single effect for d1, got %+v", effects)
	}
}

func TestFullStateReplaces(t *testing.T) {
	s, _ := applyAll(t,
		fullState("d1", ws("a", "r", "y.go")),
		fullState("d2", ws("b", "r", "y.go")),
	)
	s, effects := Apply(s, fullState("d2"))

	if got := s.Warnings(); len(got) != 0 {
		t.Fatalf("warning survived the replace: %+v", got)
	}
	if got := s.Workstreams(); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("want only workstream a, got %+v", got)
	}
	want := []Effect{
		WarningsChanged{Daemon: "d1", Warnings: []Warning{}},
		WarningsChanged{Daemon: "d2", Warnings: []Warning{}},
	}
	if !reflect.DeepEqual(effects, want) {
		t.Fatalf("effects:\n got %+v\nwant %+v", effects, want)
	}
}

func TestReapplyNoEffects(t *testing.T) {
	e := fullState("d1", ws("a", "r", "y.go"), ws("b", "r", "y.go"))
	s, _ := applyAll(t, e)
	_, effects := Apply(s, e)
	if len(effects) != 0 {
		t.Fatalf("re-applying the same full state emitted %+v", effects)
	}
}

func TestApplyDoesNotMutateInput(t *testing.T) {
	s0, _ := applyAll(t,
		fullState("d1", ws("a", "r", "y.go")),
		fullState("d2", ws("b", "r", "y.go")),
	)
	before := s0.Warnings()
	_, _ = Apply(s0, fullState("d2"))
	if after := s0.Warnings(); !reflect.DeepEqual(before, after) {
		t.Fatalf("Apply mutated its input: before %+v after %+v", before, after)
	}
	if len(s0.Workstreams()) != 2 {
		t.Fatalf("Apply mutated the input workstreams: %+v", s0.Workstreams())
	}
}

func TestFilesNormalized(t *testing.T) {
	s, _ := applyAll(t, fullState("d1", ws("a", "r", "b.go", "a.go", "b.go")))
	if got := s.Workstreams()[0].Files; !reflect.DeepEqual(got, []string{"a.go", "b.go"}) {
		t.Fatalf("files not sorted and deduped: %v", got)
	}
}

func TestOwner(t *testing.T) {
	s, _ := applyAll(t, fullState("d1", ws("ws1", "r", "a.go")))
	if d, ok := s.Owner("ws1"); !ok || d != "d1" {
		t.Fatalf("Owner(ws1) = %q, %v", d, ok)
	}
	if d, ok := s.Owner("nothing"); ok {
		t.Fatalf("Owner(nothing) = %q, true", d)
	}
}

func stamped(d DaemonID, epoch, seq uint64, ws ...Workstream) FullState {
	fs := fullState(d, ws...)
	fs.Epoch, fs.Seq = epoch, seq
	return fs
}

func TestApplyIgnoresStaleSeq(t *testing.T) {
	s, _ := applyAll(t, stamped("d1", 1, 2, ws("new", "r", "a.go")))
	s, effects := Apply(s, stamped("d1", 1, 1, ws("old", "r", "a.go")))
	if len(effects) != 0 {
		t.Fatalf("stale seq emitted %+v", effects)
	}
	if got := s.Workstreams(); len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("stale seq replaced the state: %+v", got)
	}
}

func TestApplyDuplicateSeqNoEffects(t *testing.T) {
	s, _ := applyAll(t, fullState("d2", ws("b", "r", "a.go")))
	e := stamped("d1", 1, 1, ws("a", "r", "a.go"))
	s, first := Apply(s, e)
	_, second := Apply(s, e)
	if len(first) == 0 || len(second) != 0 {
		t.Fatalf("want one set of effects, got first %+v second %+v", first, second)
	}
}

func TestApplyNewEpochReplaces(t *testing.T) {
	s, _ := applyAll(t, stamped("d1", 1, 5, ws("a", "r", "a.go")))
	s, _ = Apply(s, stamped("d1", 2, 1, ws("b", "r", "a.go")))
	if got := s.Workstreams(); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("new epoch did not replace: %+v", got)
	}
	s, effects := Apply(s, stamped("d1", 1, 6, ws("a", "r", "a.go")))
	if len(effects) != 0 {
		t.Fatalf("an older epoch emitted %+v", effects)
	}
	if got := s.Workstreams(); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("an older epoch got applied: %+v", got)
	}
}

// TestApplyLowerEpochAfterHigherRejected is the case the simulation found
// with random epochs: a serve that never saw the old run gets the new run's
// first full state, then a late one of the old run, then the new run's next.
// The old one is ignored and the new run goes on.
func TestApplyLowerEpochAfterHigherRejected(t *testing.T) {
	s, _ := applyAll(t, stamped("d1", 2, 1, ws("b", "r", "a.go")))
	s, effects := Apply(s, stamped("d1", 1, 9, ws("a", "r", "a.go")))
	if len(effects) != 0 || s.Workstreams()[0].ID != "b" {
		t.Fatalf("a late full state of an older run applied: %+v %+v", effects, s.Workstreams())
	}
	s, _ = Apply(s, stamped("d1", 2, 2, ws("c", "r", "a.go")))
	if got := s.Workstreams(); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("the new run's next full state was ignored: %+v", got)
	}
	if e := s.Clock("d1").Epoch(); e != 2 {
		t.Fatalf("d1's clock is at epoch %d, want 2", e)
	}
}

func TestApplyZeroEpochAlwaysApplies(t *testing.T) {
	s, _ := applyAll(t, fullState("d1", ws("a", "r", "a.go")))
	s, _ = Apply(s, fullState("d1", ws("b", "r", "a.go")))
	s, _ = Apply(s, fullState("d1", ws("a", "r", "a.go")))
	if got := s.Workstreams(); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("zero-epoch full state was ignored: %+v", got)
	}
	// Seq alone, without an epoch, never filters either.
	zero := fullState("d1", ws("c", "r", "a.go"))
	zero.Seq = 1
	s, _ = Apply(s, zero)
	if got := s.Workstreams(); got[0].ID != "c" {
		t.Fatalf("zero epoch with seq was ignored: %+v", got)
	}
}

func TestApplyEmptyStateNotResurrected(t *testing.T) {
	s, _ := applyAll(t, stamped("d1", 1, 3, ws("a", "r", "a.go")))
	s, _ = Apply(s, stamped("d1", 2, 1))
	if got := s.Workstreams(); len(got) != 0 {
		t.Fatalf("empty full state of a new epoch did not remove the daemon: %+v", got)
	}
	s, effects := Apply(s, stamped("d1", 1, 4, ws("a", "r", "a.go")))
	if len(effects) != 0 || len(s.Workstreams()) != 0 {
		t.Fatalf("old epoch resurrected the daemon: %+v %+v", effects, s.Workstreams())
	}
}

func TestClockAdmit(t *testing.T) {
	var c Clock
	if next, ok := c.Admit(0, 9); !ok || !reflect.DeepEqual(next, c) {
		t.Fatalf("epoch 0: Admit = %+v, %v; want true and the clock untouched", next, ok)
	}
	c, ok := c.Admit(7, 2)
	if !ok {
		t.Fatal("first message refused")
	}
	// Not newer: the same seq, a lower one, a lower epoch even with a higher
	// seq, and a lower epoch never seen.
	for _, m := range [][2]uint64{{7, 2}, {7, 1}, {6, 100}, {3, 1}} {
		if next, ok := c.Admit(m[0], m[1]); ok || !reflect.DeepEqual(next, c) {
			t.Fatalf("Admit(%d, %d) = %+v, %v; want false and the clock untouched", m[0], m[1], next, ok)
		}
	}
	for _, m := range [][2]uint64{{7, 3}, {9, 1}} {
		if c, ok = c.Admit(m[0], m[1]); !ok {
			t.Fatalf("Admit(%d, %d) refused a newer message", m[0], m[1])
		}
	}
	if _, ok := c.Admit(7, 4); ok {
		t.Fatal("an older epoch admitted after a newer one")
	}
	if _, ok := c.Admit(0, 1); !ok {
		t.Fatal("epoch 0 refused after an epoch")
	}
}
