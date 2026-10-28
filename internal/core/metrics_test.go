package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const grace = Millis(120_000)

// pairConflict is a conflict on path between d1:a and the workstream other,
// of daemon od.
func pairConflict(path string, od DaemonID, other WorkstreamID) Conflict {
	return Conflict{ID: ConflictID("r", path, "d1:a", other), Repo: "r", Path: path, Kind: "content",
		Sides: [2]Side{{Daemon: "d1", Workstream: "d1:a", Branch: "feat-a"}, {Daemon: od, Workstream: other, Branch: "feat-b"}}}
}

// local is a conflict on path between two workstreams of d1.
func local(path string) Conflict { return pairConflict(path, "d1", "d1:b") }

func tipsOf(kv ...string) map[WorkstreamID]string {
	m := map[WorkstreamID]string{}
	for i := 0; i < len(kv); i += 2 {
		m[WorkstreamID(kv[i])] = kv[i+1]
	}
	return m
}

func settleOne(t *testing.T, l *Ledger, now Millis) Record {
	t.Helper()
	rs := l.Settle(now)
	if len(rs) != 1 {
		t.Fatalf("Settle(%d) = %d records, want 1: %+v", now, len(rs), rs)
	}
	return rs[0]
}

// TestLedgerResolvedOverTheChannel, over synthetic events: a conflict the agents
// settled by messages, with an edit held and retried changed, ends resolved
// before any commit, with its four metrics, once the grace passes and not
// before.
func TestLedgerResolvedOverTheChannel(t *testing.T) {
	l := NewLedger(grace)
	c := local("x.go")
	tips := tipsOf("d1:a", "h1", "d1:b", "h2")
	l.Sync("d1", []Conflict{c}, tips, 1000)
	l.Held([]string{c.ID})
	l.Message("d1:a", "d1:b")
	l.Message("d1:b", "d1:a")
	l.Message("d1:a", "d1:a") // to itself: not between the sides
	l.Retried([]string{c.ID}, true)
	l.Sync("d1", nil, tips, 5000)
	l.Message("d1:b", "d1:a") // "done, I aligned": within the grace, counted
	if rs := l.Settle(5000 + grace - 1); len(rs) != 0 {
		t.Fatalf("settled within the grace: %+v", rs)
	}
	r := settleOne(t, l, 5000+grace)
	want := Record{ID: c.ID, Repo: "r", Daemons: [2]DaemonID{"d1", "d1"}, Opened: 1000, Closed: 5000, Outcome: OutcomeResolved,
		Tally: Tally{Messages: 3, Held: 1, Changed: 1}, Path: "x.go", Branches: [2]string{"feat-a", "feat-b"},
		Workstreams: [2]WorkstreamID{"d1:a", "d1:b"}}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("record\n got %+v\nwant %+v", r, want)
	}
	if rs := l.Settle(1 << 40); len(rs) != 0 {
		t.Fatalf("settled twice: %+v", rs)
	}
	if _, ok := l.Get(c.ID); ok {
		t.Fatal("a settled record is still there")
	}
}

// TestLedgerCommitted: a side whose tip moves while the conflict stands
// commits it; one that moves after it ended, or with the same scan that saw it
// go, does not.
func TestLedgerCommitted(t *testing.T) {
	cases := []struct {
		name  string
		steps func(l *Ledger, c Conflict)
		want  Outcome
	}{
		{"commit while standing", func(l *Ledger, c Conflict) {
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h2"), 1000)
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h3"), 2000)
			l.Sync("d1", nil, tipsOf("d1:a", "h1", "d1:b", "h3"), 3000)
		}, OutcomeCommitted},
		{"push only: the remote branch moved", func(l *Ledger, c Conflict) {
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h2"), 1000)
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1+o1", "d1:b", "h2"), 2000)
			l.Sync("d1", nil, tipsOf("d1:a", "h1+o1", "d1:b", "h2"), 3000)
		}, OutcomeCommitted},
		{"commit seen with the resolution", func(l *Ledger, c Conflict) {
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h2"), 1000)
			l.Sync("d1", nil, tipsOf("d1:a", "h1", "d1:b", "h3"), 2000)
		}, OutcomeResolved},
		{"commit after it ended", func(l *Ledger, c Conflict) {
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h2"), 1000)
			l.Sync("d1", nil, tipsOf("d1:a", "h1", "d1:b", "h2"), 2000)
			l.Sync("d1", nil, tipsOf("d1:a", "h9", "d1:b", "h2"), 3000)
		}, OutcomeResolved},
		{"first tip of a workstream is no commit", func(l *Ledger, c Conflict) {
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1"), 1000)
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h2"), 2000)
			l.Sync("d1", nil, tipsOf("d1:a", "h1", "d1:b", "h2"), 3000)
		}, OutcomeResolved},
		{"commit, then it comes back within the grace and resolves", func(l *Ledger, c Conflict) {
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h1", "d1:b", "h2"), 1000)
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h5", "d1:b", "h2"), 2000)
			l.Sync("d1", nil, tipsOf("d1:a", "h5", "d1:b", "h2"), 3000)
			l.Sync("d1", []Conflict{c}, tipsOf("d1:a", "h5", "d1:b", "h2"), 4000)
			l.Sync("d1", nil, tipsOf("d1:a", "h5", "d1:b", "h2"), 5000)
		}, OutcomeCommitted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLedger(grace)
			c := local("x.go")
			tc.steps(l, c)
			r := settleOne(t, l, 1<<40)
			if r.Outcome != tc.want || r.Committed != (tc.want == OutcomeCommitted) {
				t.Fatalf("outcome %q committed %v, want %q", r.Outcome, r.Committed, tc.want)
			}
		})
	}
}

// TestLedgerGone: a conflict whose local workstream went away ended gone; one
// whose other side is another daemon's is judged on this daemon's side only.
func TestLedgerGone(t *testing.T) {
	l := NewLedger(grace)
	c := local("x.go")
	r := pairConflict("y.go", "d2", "d2:z")
	l.Sync("d1", []Conflict{c, r}, tipsOf("d1:a", "h1", "d1:b", "h2"), 1000)
	l.Sync("d1", nil, tipsOf("d1:a", "h1"), 2000) // d1:b's worktree is gone; d2:z is not d1's to judge
	got := map[string]Outcome{}
	for _, rec := range l.Settle(1 << 40) {
		got[rec.Path] = rec.Outcome
	}
	if want := map[string]Outcome{"x.go": OutcomeGone, "y.go": OutcomeResolved}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes %v, want %v", got, want)
	}
}

// TestLedgerFlap: a conflict that comes back within the grace is the same
// record, its tally kept; past the grace it is a new one.
func TestLedgerFlap(t *testing.T) {
	l := NewLedger(grace)
	c := local("x.go")
	tips := tipsOf("d1:a", "h1", "d1:b", "h2")
	l.Sync("d1", []Conflict{c}, tips, 1000)
	l.Message("d1:a", "d1:b")
	l.Sync("d1", nil, tips, 2000)
	l.Sync("d1", []Conflict{c}, tips, 3000)
	if rs := l.Settle(2000 + grace); len(rs) != 0 {
		t.Fatalf("a conflict that came back settled: %+v", rs)
	}
	if r, _ := l.Get(c.ID); r.Closed != 0 || r.Outcome != OutcomeOpen || r.Opened != 1000 || r.Messages != 1 {
		t.Fatalf("after coming back: %+v", r)
	}
	l.Sync("d1", nil, tips, 4000)
	first := settleOne(t, l, 4000+grace)
	l.Sync("d1", []Conflict{c}, tips, 5000+grace)
	l.Sync("d1", nil, tips, 6000+grace)
	second := settleOne(t, l, 6000+2*grace)
	if first.Messages != 1 || second.Messages != 0 || second.Opened != 5000+grace {
		t.Fatalf("first %+v, second %+v", first, second)
	}
}

// TestLedgerMeasures: each measure lands on the conflicts it names, and only
// on Records the ledger holds; messages land on every conflict between the
// two workstreams, and on none with a third.
func TestLedgerMeasures(t *testing.T) {
	l := NewLedger(grace)
	x, y, z := local("x.go"), local("y.go"), pairConflict("z.go", "d1", "d1:c")
	l.Sync("d1", []Conflict{x, y, z}, tipsOf("d1:a", "1", "d1:b", "2", "d1:c", "3"), 1000)
	l.Message("d1:b", "d1:a")
	l.Message("d1:b", "d1:c") // no conflict between b and c
	l.Held([]string{x.ID, y.ID, x.ID, "unknown"})
	l.Retried([]string{x.ID}, false)
	l.Retried([]string{y.ID}, true)
	l.Escalated([]string{x.ID, y.ID}, EscalatedByRule)
	l.Escalated([]string{x.ID}, EscalatedByAgent)
	l.Escalated([]string{z.ID}, EscalatedRelayed)
	want := map[string]Tally{
		"x.go": {Messages: 1, Held: 1, Unchanged: 1, RuleEscalations: 1, AgentEscalations: 1},
		"y.go": {Messages: 1, Held: 1, Changed: 1, RuleEscalations: 1},
		"z.go": {Relayed: 1},
	}
	for _, c := range []Conflict{x, y, z} {
		r, _ := l.Get(c.ID)
		if r.Tally != want[c.Path] {
			t.Errorf("%s: %+v, want %+v", c.Path, r.Tally, want[c.Path])
		}
	}
	if r, _ := l.Get(x.ID); r.Escalations() != 2 {
		t.Errorf("x.go escalations %d, want 2", r.Escalations())
	}
}

// TestMergeTwoDaemons: the records two daemons sent of one conflict add up
// their own counts, keep the most rule escalations of either, drop the
// relayed copy, and commit when either side committed.
func TestMergeTwoDaemons(t *testing.T) {
	mine := Record{ID: "c", Repo: "r", Daemons: [2]DaemonID{"d1", "d2"}, Opened: 1000, Closed: 9000, Outcome: OutcomeResolved,
		Tally: Tally{Held: 1, Changed: 1, RuleEscalations: 1, AgentEscalations: 1}}
	theirs := Record{ID: "c", Repo: "r", Daemons: [2]DaemonID{"d1", "d2"}, Opened: 2000, Closed: 8000, Outcome: OutcomeResolved,
		Tally: Tally{Held: 2, Unchanged: 1, RuleEscalations: 1, Relayed: 1}}
	m := Merge([]Record{mine, theirs})
	want := Tally{Held: 3, Changed: 1, Unchanged: 1, RuleEscalations: 1, AgentEscalations: 1}
	if m.Tally != want || m.Outcome != OutcomeResolved || m.Opened != 1000 || m.Closed != 9000 {
		t.Fatalf("merged %+v, want tally %+v resolved 1000-9000", m, want)
	}
	if m.Escalations() != 2 {
		t.Fatalf("escalations %d, want 2: one rule, one agent", m.Escalations())
	}
	theirs.Outcome, theirs.Committed = OutcomeCommitted, true
	if m := Merge([]Record{mine, theirs}); m.Outcome != OutcomeCommitted || !m.Committed {
		t.Fatalf("one side committed: %+v", m)
	}
	theirs.Outcome, theirs.Committed = OutcomeGone, false
	if m := Merge([]Record{mine, theirs}); m.Outcome != OutcomeGone {
		t.Fatalf("one side gone: %+v", m)
	}
	if m := Merge([]Record{mine}); m.Tally != (Tally{Held: 1, Changed: 1, RuleEscalations: 1, AgentEscalations: 1}) || m.Outcome != OutcomeResolved {
		t.Fatalf("one record: %+v", m)
	}
}

// TestTotals, over synthetic records: the
// share resolved before a commit leaves gone conflicts out.
func TestTotals(t *testing.T) {
	var tot Totals
	if _, ok := tot.ResolvedShare(); ok {
		t.Fatal("a share with nothing ended")
	}
	for _, r := range []Record{
		{Outcome: OutcomeResolved, Tally: Tally{Messages: 2, Held: 1, Changed: 1}},
		{Outcome: OutcomeResolved, Tally: Tally{AgentEscalations: 1}},
		{Outcome: OutcomeCommitted, Tally: Tally{Committed: true, RuleEscalations: 1, Held: 1, Unchanged: 1}},
		{Outcome: OutcomeGone, Tally: Tally{Messages: 1, Relayed: 1}},
	} {
		tot.Add(r)
	}
	want := Totals{Conflicts: 4, Resolved: 2, Committed: 1, Gone: 1, Messages: 3, Escalations: 3, Held: 2, Changed: 1, Unchanged: 1}
	if tot != want {
		t.Fatalf("totals %+v, want %+v", tot, want)
	}
	if s, ok := tot.ResolvedShare(); !ok || s != 2.0/3 {
		t.Fatalf("share %v %v, want 2/3", s, ok)
	}
}

// TestRecordWire: what a Record sends is
// counts and identities, never the path, the branches or the workstreams.
func TestRecordWire(t *testing.T) {
	r := Record{ID: "c", Repo: "r", Daemons: [2]DaemonID{"d1", "d2"}, Opened: 1, Closed: 2, Outcome: OutcomeResolved,
		Tally: Tally{Messages: 1}, Path: "secret/path.go", Branches: [2]string{"feat-secret", "b"}, Workstreams: [2]WorkstreamID{"d1:w", "d2:w"}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret", "d1:w", "path"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%s carries %q", b, leak)
		}
	}
	if want := `{"id":"c","repo":"r","daemons":["d1","d2"],"opened":1,"closed":2,"outcome":"resolved","messages":1}`; string(b) != want {
		t.Errorf("wire %s, want %s", b, want)
	}
}
