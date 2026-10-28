package daemoncore

import (
	"reflect"
	"slices"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

// TestSameWorkstreamsCoversAllFields fails when core.Workstream gains a field
// that sameWorkstreams does not compare, so a change there is not missed.
func TestSameWorkstreamsCoversAllFields(t *testing.T) {
	if n := reflect.TypeFor[core.Workstream]().NumField(); n != 6 {
		t.Fatalf("core.Workstream has %d fields: update sameWorkstreams in outbox.go to compare the new one", n)
	}
}

// state is a full state of one workstream with files, as the daemon hands it
// over: its Daemon, Epoch and Seq are the outbox's to stamp.
func state(files ...string) core.FullState {
	return core.FullState{Workstreams: []core.Workstream{{ID: "d1:a", Repo: "r", Branch: "a", Files: files}}}
}

func conflict(id string, line int) core.Conflict {
	return core.Conflict{ID: id, Repo: "r", Path: "x.go", Kind: "content",
		Lines: [2][]core.Range{{{Start: line, Len: 1}}, {{Start: line, Len: 1}}}}
}

// steps feeds events to o in order and returns the outbox after them and
// every send they caused.
func steps(o Outbox, events ...Event) (Outbox, []Send) {
	var all []Send
	for _, e := range events {
		var sends []Send
		o, sends = Step(o, e)
		all = append(all, sends...)
	}
	return o, all
}

func kinds(sends []Send) []Kind {
	out := make([]Kind, len(sends))
	for i, s := range sends {
		out[i] = s.Kind
	}
	return out
}

func ids(cs []core.Conflict) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func ok(s Send) Sent     { return Sent{Kind: s.Kind, Seq: s.Seq, OK: true} }
func failed(s Send) Sent { return Sent{Kind: s.Kind, Seq: s.Seq, OK: false} }

func TestStepStateChange(t *testing.T) {
	given := state("x.go")
	given.Daemon, given.Epoch, given.Seq = "other", 1, 99
	o, sends := Step(NewOutbox("d1", 7), SetState{State: given})
	if len(sends) != 1 || sends[0].Kind != KindState {
		t.Fatalf("first state: sends = %+v, want one state send", sends)
	}
	if s := sends[0]; s.Seq != 1 || s.State.Seq != 1 || s.State.Epoch != 7 || s.State.Daemon != "d1" {
		t.Fatalf("first state stamped %+v (send seq %d), want daemon d1, epoch 7, seq 1", s.State, s.Seq)
	}
	_, sends = Step(o, SetState{State: state("x.go", "y.go")})
	if len(sends) != 1 || sends[0].Kind != KindState || sends[0].Seq != 2 || sends[0].State.Seq != 2 {
		t.Fatalf("changed state: sends = %+v, want one state send with seq 2", sends)
	}
	if got := sends[0].State.Workstreams[0].Files; !slices.Equal(got, []string{"x.go", "y.go"}) {
		t.Fatalf("sent files %v, want the new ones", got)
	}
}

func TestStepSameStateNoSend(t *testing.T) {
	same := state("x.go")
	same.Daemon, same.Seq = "d1", 99 // only the workstreams count
	if _, sends := steps(NewOutbox("d1", 7), SetState{State: state("x.go")}, SetState{State: same}); len(sends) != 1 {
		t.Fatalf("an equal state went again: sends = %+v", sends)
	}

	// The first state goes even when empty: serve must hear the daemon has
	// nothing in flight. Nil workstreams are as empty as none.
	o, sends := Step(NewOutbox("d1", 7), SetState{State: core.FullState{Workstreams: []core.Workstream{}}})
	if len(sends) != 1 {
		t.Fatalf("first, empty state: sends = %+v, want one", sends)
	}
	if _, sends := Step(o, SetState{}); len(sends) != 0 {
		t.Fatalf("a nil state after an empty one went: %+v", sends)
	}
}

// TestStepIntentChangeSends: a workstream that only changed its intent is a
// changed full state, and the tick after it sends the new one.
func TestStepIntentChangeSends(t *testing.T) {
	declared := state("x.go")
	declared.Workstreams[0].Intent = "fix the login"
	o, sends := steps(NewOutbox("d1", 7), SetState{State: state("x.go")}, SetState{State: declared})
	if len(sends) != 2 || sends[1].State.Workstreams[0].Intent != "fix the login" {
		t.Fatalf("intent declared: sends = %+v, want the state again with the intent", sends)
	}
	if _, sends := Step(o, Tick{}); len(sends) != 1 || sends[0].State.Workstreams[0].Intent != "fix the login" {
		t.Fatalf("tick after the intent: sends = %+v, want the state with the intent", sends)
	}
}

// TestStepPersonChangeSends: a workstream whose human's name changed is a
// changed full state, so the other daemons' escalations name the new one.
func TestStepPersonChangeSends(t *testing.T) {
	named := state("x.go")
	named.Workstreams[0].Person = "Ana"
	if _, sends := steps(NewOutbox("d1", 7), SetState{State: state("x.go")}, SetState{State: named}); len(sends) != 2 || sends[1].State.Workstreams[0].Person != "Ana" {
		t.Fatalf("person set: sends = %+v, want the state again with the person", sends)
	}
}

func TestStepConnectedSendsAll(t *testing.T) {
	// Nothing given yet: an empty full state would clear the daemon's
	// warnings before its first scan, and an empty conflict set would
	// say "none" before the detector ran.
	o, sends := Step(NewOutbox("d1", 7), Connected{})
	if len(sends) != 0 {
		t.Fatalf("connect before anything was given: sends = %+v", sends)
	}

	o, sends = steps(o, SetState{State: state("x.go")}, Connected{})
	if !slices.Equal(kinds(sends), []Kind{KindState, KindState}) || sends[1].Seq <= sends[0].Seq {
		t.Fatalf("connect after a state: sends = %+v, want the state again with a new seq", sends)
	}

	// From the first set on, every connect sends it, even empty: the serve on
	// the other end may still hold an older report of this daemon's.
	o, sends = Step(o, SetConflicts{})
	o, _ = Step(o, ok(sends[0]))
	o, sends = Step(o, Connected{})
	if !slices.Equal(kinds(sends), []Kind{KindState, KindConflicts}) || len(sends[1].Conflicts) != 0 {
		t.Fatalf("connect after an empty set: sends = %+v, want the state and the empty set", sends)
	}
	o, _ = steps(o, ok(sends[0]), ok(sends[1]))

	o, sends = Step(o, SetConflicts{Conflicts: []core.Conflict{conflict("c1", 3)}})
	o, _ = Step(o, ok(sends[0]))
	_, sends = Step(o, Connected{})
	if !slices.Equal(kinds(sends), []Kind{KindState, KindConflicts}) || !slices.Equal(ids(sends[1].Conflicts), []string{"c1"}) {
		t.Fatalf("connect after c1: sends = %+v, want the state and c1", sends)
	}
}

func TestStepTickResends(t *testing.T) {
	o, sends := Step(NewOutbox("d1", 7), Tick{})
	if len(sends) != 0 {
		t.Fatalf("tick before anything was given: sends = %+v", sends)
	}

	// The state goes on every tick, with a new seq.
	o, sends = Step(o, SetState{State: state("x.go")})
	o, _ = Step(o, ok(sends[0]))
	o, sends = Step(o, Tick{})
	if !slices.Equal(kinds(sends), []Kind{KindState}) || sends[0].Seq != 2 || sends[0].State.Seq != 2 {
		t.Fatalf("tick: sends = %+v, want the state with seq 2", sends)
	}

	// So does a set with conflicts, even one serve took: a serve that
	// restarted under a stream that stayed open has lost it.
	o, sends = Step(o, SetConflicts{Conflicts: []core.Conflict{conflict("c1", 3)}})
	o, _ = Step(o, ok(sends[0]))
	o, sends = Step(o, Tick{})
	if !slices.Equal(kinds(sends), []Kind{KindState, KindConflicts}) || !slices.Equal(ids(sends[1].Conflicts), []string{"c1"}) {
		t.Fatalf("tick with c1: sends = %+v, want the state and c1", sends)
	}

	// An empty set that serve took has nothing to restore.
	o, sends = Step(o, SetConflicts{})
	o, _ = Step(o, ok(sends[0]))
	if _, sends = Step(o, Tick{}); !slices.Equal(kinds(sends), []Kind{KindState}) {
		t.Fatalf("tick after an empty set arrived: sends = %+v, want the state only", sends)
	}
}

func TestStepFailedStaysPending(t *testing.T) {
	o, sends := steps(NewOutbox("d1", 7), SetState{State: state("x.go")}, SetConflicts{})
	o, _ = steps(o, failed(sends[0]), failed(sends[1]))

	// An equal set does not send it again: it waits for the next tick or connect.
	if o, sends = Step(o, SetConflicts{}); len(sends) != 0 {
		t.Fatalf("an equal set after a failure: sends = %+v, want none", sends)
	}
	o, sends = Step(o, Tick{})
	if !slices.Equal(kinds(sends), []Kind{KindState, KindConflicts}) || len(sends[1].Conflicts) != 0 {
		t.Fatalf("tick after both failed: sends = %+v, want the state and the empty set", sends)
	}
	// While that send is out, its result decides: another tick does not repeat it.
	o, again := Step(o, Tick{})
	if !slices.Equal(kinds(again), []Kind{KindState}) {
		t.Fatalf("tick with the resend out: sends = %+v, want the state only", again)
	}
	o, _ = steps(o, ok(again[0]), ok(sends[0]), ok(sends[1]))
	if _, sends = Step(o, Tick{}); !slices.Equal(kinds(sends), []Kind{KindState}) {
		t.Fatalf("tick after both arrived: sends = %+v, want the state only", sends)
	}
}

func TestStepOldAckKeepsNewerPending(t *testing.T) {
	o, sends := steps(NewOutbox("d1", 7), SetConflicts{Conflicts: []core.Conflict{conflict("c1", 3)}}, SetConflicts{})
	older, newer := sends[0], sends[1]
	// The newer, empty set fails; serve's answer to the older one comes late.
	o, _ = steps(o, failed(newer), ok(older))
	if _, sends = Step(o, Tick{}); !slices.Equal(kinds(sends), []Kind{KindConflicts}) || len(sends[0].Conflicts) != 0 {
		t.Fatalf("tick after an old ack: sends = %+v, want the empty set again", sends)
	}

	// The other way round: the newer set arrived, so the older one failing
	// says nothing about what serve holds.
	o, sends = steps(NewOutbox("d1", 7), SetConflicts{Conflicts: []core.Conflict{conflict("c1", 3)}}, SetConflicts{})
	o, _ = steps(o, ok(sends[1]), failed(sends[0]))
	if _, sends = Step(o, Tick{}); len(sends) != 0 {
		t.Fatalf("tick after an old failure: sends = %+v, want none", sends)
	}
}

func TestStepConflictsOnlyOnChange(t *testing.T) {
	c1, c2 := conflict("c1", 3), conflict("c2", 5)
	given := []core.Conflict{c2, c1}
	o, sends := Step(NewOutbox("d1", 7), SetConflicts{Conflicts: given})
	if len(sends) != 1 || sends[0].Kind != KindConflicts || !slices.Equal(ids(sends[0].Conflicts), []string{"c1", "c2"}) {
		t.Fatalf("first set: sends = %+v, want one conflicts send sorted by ID", sends)
	}
	if given[0].ID != "c2" {
		t.Fatal("Step reordered the caller's slice")
	}
	o, _ = Step(o, ok(sends[0]))

	// The same IDs and lines, in another order and with other text: nothing
	// new for serve.
	same := c1
	same.Text = [2][]core.Snippet{{{Lines: []string{"other"}}}}
	if o, sends = Step(o, SetConflicts{Conflicts: []core.Conflict{c2, same}}); len(sends) != 0 {
		t.Fatalf("an equal set: sends = %+v, want none", sends)
	}

	// Other lines are a change, and so is a conflict gone.
	if o, sends = Step(o, SetConflicts{Conflicts: []core.Conflict{conflict("c1", 9), c2}}); len(sends) != 1 || sends[0].Conflicts[0].Lines[0][0].Start != 9 {
		t.Fatalf("c1 on other lines: sends = %+v, want the new set", sends)
	}
	if o, sends = Step(o, SetConflicts{Conflicts: []core.Conflict{c2}}); len(sends) != 1 || !slices.Equal(ids(sends[0].Conflicts), []string{"c2"}) {
		t.Fatalf("c1 gone: sends = %+v, want c2 alone", sends)
	}
	o, _ = Step(o, ok(sends[0]))

	// A set equal to what serve took goes again when another one was sent
	// since: that one may still reach serve.
	o, _ = Step(o, SetConflicts{Conflicts: []core.Conflict{c1, c2}})
	if _, sends = Step(o, SetConflicts{Conflicts: []core.Conflict{c2}}); len(sends) != 1 || !slices.Equal(ids(sends[0].Conflicts), []string{"c2"}) {
		t.Fatalf("back to c2 with c1+c2 in flight: sends = %+v, want c2 alone", sends)
	}
}

// TestSeqMonotonic: whatever the events, every send gets a seq above the one
// before, and a state carries it with the outbox's daemon and epoch.
func TestSeqMonotonic(t *testing.T) {
	o := NewOutbox("d1", 7)
	var last uint64
	n := 0
	for i := range 40 {
		var cs []core.Conflict
		if i%2 == 0 {
			cs = []core.Conflict{conflict("c1", i%4)}
		}
		for _, e := range []Event{SetState{State: state(string(rune('a' + i%5)))}, SetConflicts{Conflicts: cs}, Tick{}, Connected{}} {
			var sends []Send
			o, sends = Step(o, e)
			for _, s := range sends {
				n++
				if s.Seq <= last {
					t.Fatalf("send %d: seq %d after %d", n, s.Seq, last)
				}
				last = s.Seq
				if s.Kind == KindState && (s.State.Seq != s.Seq || s.State.Epoch != 7 || s.State.Daemon != "d1") {
					t.Fatalf("send %d: state stamped %+v, want daemon d1, epoch 7, seq %d", n, s.State, s.Seq)
				}
				o, _ = Step(o, Sent{Kind: s.Kind, Seq: s.Seq, OK: n%3 != 0})
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d sends: the events did not exercise the outbox", n)
	}
}

// TestStepStampsEpoch: every send carries the run's epoch, a conflict set's
// too, so the uplink and the simulation take it from one place.
func TestStepStampsEpoch(t *testing.T) {
	_, sends := steps(NewOutbox("d1", 7), SetState{State: state("x.go")}, SetConflicts{Conflicts: []core.Conflict{conflict("c1", 3)}})
	if !slices.Equal(kinds(sends), []Kind{KindState, KindConflicts}) {
		t.Fatalf("sends = %+v, want a state and a set", sends)
	}
	for _, s := range sends {
		if s.Epoch != 7 {
			t.Fatalf("%s send carries epoch %d, want 7", s.Kind, s.Epoch)
		}
	}
	if sends[0].State.Epoch != 7 {
		t.Fatalf("state stamped epoch %d, want 7", sends[0].State.Epoch)
	}
}

func record(id string, closed core.Millis) core.Record {
	return core.Record{ID: id, Repo: "r", Daemons: [2]core.DaemonID{"d1", "d1"}, Closed: closed, Outcome: core.OutcomeResolved}
}

func recordIDs(rs []core.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// TestStepOutcomesUntilTaken: ended conflicts' records go at
// once, after the state and the set; one that failed goes again on the next
// tick and connect; once serve took a send, its records go no more, and a
// record added while that send was out stays.
func TestStepOutcomesUntilTaken(t *testing.T) {
	o := NewOutbox("d1", 7)
	o, sends := steps(o, AddOutcomes{})
	if len(sends) != 0 {
		t.Fatalf("no records sent %v", kinds(sends))
	}
	o, sends = steps(o, SetState{State: state("a.go")}, SetConflicts{}, AddOutcomes{Records: []core.Record{record("c1", 10)}})
	if want := []Kind{KindState, KindConflicts, KindOutcomes}; !slices.Equal(kinds(sends), want) {
		t.Fatalf("sends %v, want %v", kinds(sends), want)
	}
	first := sends[2]
	if first.Epoch != 7 || !slices.Equal(recordIDs(first.Outcomes), []string{"c1"}) {
		t.Fatalf("outcomes send %+v", first)
	}
	o, _ = steps(o, failed(first))
	o, sends = steps(o, Tick{})
	if want := []Kind{KindState, KindOutcomes}; !slices.Equal(kinds(sends), want) {
		t.Fatalf("tick after a failure %v, want %v", kinds(sends), want)
	}
	retry := sends[1]
	o, sends = steps(o, AddOutcomes{Records: []core.Record{record("c2", 20)}})
	newer := sends[0]
	if !slices.Equal(recordIDs(newer.Outcomes), []string{"c1", "c2"}) {
		t.Fatalf("second send carries %v", recordIDs(newer.Outcomes))
	}
	o, _ = steps(o, ok(retry)) // an older send taken: the newer one still counts
	if o.Outcomes() != 2 {
		t.Fatalf("an older ack dropped records: %d waiting", o.Outcomes())
	}
	o, sends = steps(o, AddOutcomes{Records: []core.Record{record("c3", 30)}})
	carried := sends[0]
	o, _ = steps(o, AddOutcomes{Records: []core.Record{record("c4", 40)}}) // its send is replaced before it goes
	o, sends = steps(o, Connected{})
	last := sends[len(sends)-1]
	if last.Kind != KindOutcomes || !slices.Equal(recordIDs(last.Outcomes), []string{"c1", "c2", "c3", "c4"}) {
		t.Fatalf("connect sends %v", kinds(sends))
	}
	o, _ = steps(o, ok(carried)) // not the last send: ignored
	if o.Outcomes() != 4 {
		t.Fatalf("waiting %d, want 4", o.Outcomes())
	}
	o, _ = steps(o, AddOutcomes{Records: []core.Record{record("c1", 50)}}) // the same conflict ended again
	o, sends = steps(o, Tick{})
	o, _ = steps(o, ok(sends[len(sends)-1]))
	if o.Outcomes() != 0 {
		t.Fatalf("after serve took everything, %d wait", o.Outcomes())
	}
	if _, sends = steps(o, Tick{}); !slices.Equal(kinds(sends), []Kind{KindState}) {
		t.Fatalf("tick with nothing waiting sends %v", kinds(sends))
	}
}

// TestStepOutcomesCapped: past maxOutcomes waiting, the oldest records go.
func TestStepOutcomesCapped(t *testing.T) {
	o := NewOutbox("d1", 1)
	var rs []core.Record
	for i := range maxOutcomes + 5 {
		rs = append(rs, record("c", core.Millis(i)))
	}
	o, sends := steps(o, AddOutcomes{Records: rs})
	got := sends[0].Outcomes
	if len(got) != maxOutcomes || got[0].Closed != 5 || o.Outcomes() != maxOutcomes {
		t.Fatalf("%d records, first closed at %d; want %d from 5", len(got), got[0].Closed, maxOutcomes)
	}
}

// TestSameConflictsSeesStances: a claim or a yield on a conflict whose lines
// did not move is a new set, so serve hears who keeps the span.
func TestSameConflictsSeesStances(t *testing.T) {
	c := core.Conflict{ID: "c1", Lines: [2][]core.Range{{{Start: 3, Len: 2}}, {{Start: 3, Len: 2}}}}
	claimed := c
	claimed.Stances = [2]core.Stance{core.StanceClaim, core.StanceNone}
	if _, sends := steps(NewOutbox("d1", 7), SetConflicts{Conflicts: []core.Conflict{c}}, SetConflicts{Conflicts: []core.Conflict{claimed}}); len(sends) != 2 {
		t.Fatalf("a new stance on equal lines: sends = %+v, want two conflicts sends", sends)
	}
}
