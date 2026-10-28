// Package daemoncore is the daemon's side of the sync with serve as a pure
// state machine: Step(outbox, event) -> (outbox, sends). It decides what
// goes to serve and when, and stamps every full state with the run's epoch
// and a seq, so serve ignores a stale one. The Uplink does the I/O; the
// simulation drives the same machine over a hostile network. Like core, it
// has no I/O, clock or randomness (guard_test.go enforces it).
package daemoncore

import (
	"slices"
	"strings"

	"github.com/joacofg/dugout/internal/core"
)

// Kind is what a Send carries.
type Kind int

const (
	KindState     Kind = iota + 1 // the daemon's full state
	KindConflicts                 // the daemon's conflict set
	KindOutcomes                  // the records of conflicts that ended
)

func (k Kind) String() string {
	switch k {
	case KindState:
		return "state"
	case KindConflicts:
		return "conflicts"
	case KindOutcomes:
		return "outcomes"
	}
	return "kind?"
}

// Event is an input to Step.
type Event interface{ isEvent() }

// SetState hands over the daemon's current full state. Its Daemon, Epoch and
// Seq are the outbox's to stamp.
type SetState struct{ State core.FullState }

// SetConflicts hands over the daemon's current conflict set, in any order.
type SetConflicts struct{ Conflicts []core.Conflict }

// AddOutcomes hands over the records of conflicts that ended. Each one goes
// to serve until serve took a send that carried it.
type AddOutcomes struct{ Records []core.Record }

// Connected is the warnings stream connecting: the serve on the other end
// may be a new one that holds nothing of this daemon's, or an old report.
type Connected struct{}

// Tick is the periodic resend.
type Tick struct{}

// Sent is the result of the send with Seq: OK when serve took it. Every
// Send is owed one (see Step): a lost message is OK false, never silence.
type Sent struct {
	Kind Kind
	Seq  uint64
	OK   bool
}

func (SetState) isEvent()     {}
func (SetConflicts) isEvent() {}
func (AddOutcomes) isEvent()  {}
func (Connected) isEvent()    {}
func (Tick) isEvent()         {}
func (Sent) isEvent()         {}

// Send is one message for serve, with the run's epoch and a seq above every
// earlier send's. A state's are also stamped into State, with the daemon.
type Send struct {
	Kind      Kind
	Epoch     uint64
	Seq       uint64
	State     core.FullState  // when Kind is KindState
	Conflicts []core.Conflict // when Kind is KindConflicts, sorted by ID
	Outcomes  []core.Record   // when Kind is KindOutcomes, oldest first
}

// maxOutcomes caps the records waiting for serve; past it the oldest go, so
// a serve that is gone for days costs a bounded memory.
const maxOutcomes = 1000

// Outbox is what one run of a daemon owes serve. Step never mutates an
// Outbox it receives; the slices inside it are treated as immutable.
type Outbox struct {
	daemon core.DaemonID
	epoch  uint64
	seq    uint64 // of the last send, of either kind

	state    core.FullState
	hasState bool

	conflicts []core.Conflict // last set given, sorted by ID
	hasSet    bool            // a set was given; before, there is none to send
	setSeq    uint64          // seq of the last conflicts send
	setOwed   bool            // that send failed: the next tick sends the set again

	outcomes []core.Record // not taken by serve yet, oldest first
	outSeq   uint64        // seq of the last outcomes send
	outSent  []outcomeKey  // what it carried
}

// outcomeKey names one record of one daemon: a conflict that ended, and when.
type outcomeKey struct {
	id     string
	closed core.Millis
}

// NewOutbox returns the outbox of daemon d's run epoch. Epoch 0 stamps none,
// and serve applies every such full state.
func NewOutbox(d core.DaemonID, epoch uint64) Outbox {
	return Outbox{daemon: d, epoch: epoch}
}

// Step returns the outbox after e and what to send, the state before the
// conflicts, the conflicts before the outcomes. Nothing goes before it was given: an empty full state would
// clear the daemon's warnings before its first scan, and an empty set
// would say "no conflicts" before the detector ran.
//
// The caller owes the outbox two things. Every Send gets its Sent, a lost
// message too, as OK false: an empty set whose send vanished in silence
// would wait for the next Connected. Only a send that a newer one of its
// kind replaced before it went out may be skipped without one, since its
// result would not count. And the caller never mutates what it handed over:
// the outbox keeps the slices inside.
func Step(o Outbox, e Event) (Outbox, []Send) {
	var sends []Send
	switch e := e.(type) {
	case SetState:
		if o.hasState && sameWorkstreams(o.state.Workstreams, e.State.Workstreams) {
			return o, nil
		}
		o.state, o.hasState = e.State, true
		sends = append(sends, o.sendState())
	case SetConflicts:
		cs := slices.Clone(e.Conflicts)
		slices.SortFunc(cs, func(a, b core.Conflict) int { return strings.Compare(a.ID, b.ID) })
		// Compared with the last set given, not the last serve took: one sent
		// since may still land, and only a newer send replaces it.
		if o.hasSet && sameConflicts(o.conflicts, cs) {
			return o, nil
		}
		o.conflicts, o.hasSet = cs, true
		sends = append(sends, o.sendConflicts())
	case AddOutcomes:
		if len(e.Records) == 0 {
			return o, nil
		}
		all := append(slices.Clone(o.outcomes), e.Records...)
		o.outcomes = all[max(0, len(all)-maxOutcomes):]
		sends = append(sends, o.sendOutcomes())
	case Connected:
		// The set goes even when empty: the serve may hold what this daemon
		// reported before it restarted.
		if o.hasState {
			sends = append(sends, o.sendState())
		}
		if o.hasSet {
			sends = append(sends, o.sendConflicts())
		}
		if len(o.outcomes) > 0 {
			sends = append(sends, o.sendOutcomes())
		}
	case Tick:
		// A set with conflicts goes on every tick, since a serve that restarted
		// under a stream that stayed open has lost it; an empty one only when
		// it failed, since one that arrived has nothing to restore.
		if o.hasState {
			sends = append(sends, o.sendState())
		}
		if o.hasSet && (o.setOwed || len(o.conflicts) > 0) {
			sends = append(sends, o.sendConflicts())
		}
		// Records wait until serve takes them, so a failed send goes again.
		if len(o.outcomes) > 0 {
			sends = append(sends, o.sendOutcomes())
		}
	case Sent:
		// Only the last send of the set counts: it carries the whole set, so
		// an older one's result says nothing about what serve ends up with.
		// The state goes on every tick and connect, whatever its results.
		if e.Kind == KindConflicts && e.Seq == o.setSeq {
			o.setOwed = !e.OK
		}
		// Only the last send of the outcomes counts, as for the set: it
		// carried every record waiting then. Those records go; any added
		// since stay for the next send. An older send that serve took too
		// makes serve see a record twice, which it ignores.
		if e.Kind == KindOutcomes && e.Seq == o.outSeq && e.OK {
			taken := map[outcomeKey]bool{}
			for _, k := range o.outSent {
				taken[k] = true
			}
			o.outcomes = slices.DeleteFunc(slices.Clone(o.outcomes), func(r core.Record) bool { return taken[outcomeKey{r.ID, r.Closed}] })
			o.outSent = nil
		}
	}
	return o, sends
}

func (o *Outbox) sendState() Send {
	o.seq++
	st := o.state
	st.Daemon, st.Epoch, st.Seq = o.daemon, o.epoch, o.seq
	return Send{Kind: KindState, Epoch: o.epoch, Seq: o.seq, State: st}
}

func (o *Outbox) sendConflicts() Send {
	o.seq++
	o.setSeq, o.setOwed = o.seq, false
	return Send{Kind: KindConflicts, Epoch: o.epoch, Seq: o.seq, Conflicts: o.conflicts}
}

func (o *Outbox) sendOutcomes() Send {
	o.seq++
	o.outSeq = o.seq
	o.outSent = make([]outcomeKey, len(o.outcomes))
	for i, r := range o.outcomes {
		o.outSent[i] = outcomeKey{r.ID, r.Closed}
	}
	return Send{Kind: KindOutcomes, Epoch: o.epoch, Seq: o.seq, Outcomes: o.outcomes}
}

// Outcomes is how many records wait for serve.
func (o Outbox) Outcomes() int { return len(o.outcomes) }

func sameWorkstreams(a, b []core.Workstream) bool {
	return slices.EqualFunc(a, b, func(x, y core.Workstream) bool {
		return x.ID == y.ID && x.Repo == y.Repo && x.Branch == y.Branch && slices.Equal(x.Files, y.Files) && x.Intent == y.Intent && x.Person == y.Person
	})
}

// sameConflicts compares two sets sorted by ID by IDs, line ranges and
// stances: an ID already fixes the repo, the path and the sides.
func sameConflicts(a, b []core.Conflict) bool {
	return slices.EqualFunc(a, b, func(x, y core.Conflict) bool {
		return x.ID == y.ID && slices.Equal(x.Lines[0], y.Lines[0]) && slices.Equal(x.Lines[1], y.Lines[1]) && x.Stances == y.Stances
	})
}
