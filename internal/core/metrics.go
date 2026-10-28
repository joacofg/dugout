package core

import (
	"slices"
	"strings"
)

// Agent metrics: what the agents did about each conflict, counted
// with no human action. A daemon keeps a Ledger of the conflicts it sees and
// the measures of its own sessions, each conflict a Record; once a conflict
// ended, its Record goes to serve, which merges both daemons' and adds them
// up per repo (Totals). A Record holds counts only: no code, no text of a
// message and, on the wire, no path.

// Outcome is how a conflict ended.
type Outcome string

const (
	// OutcomeOpen: it still stands.
	OutcomeOpen Outcome = ""
	// OutcomeResolved: it went away before either side committed or pushed.
	OutcomeResolved Outcome = "resolved"
	// OutcomeCommitted: a side committed or pushed while it stood, so the
	// conflict reached git history.
	OutcomeCommitted Outcome = "committed"
	// OutcomeGone: a workstream of it went away while it stood, with no
	// commit: its worktree was removed or switched branches.
	OutcomeGone Outcome = "gone"
)

// Escalator is who took a conflict to the humans, as a Tally counts it.
type Escalator int

const (
	// EscalatedByRule: a fixed rule of EscalationRule, on this daemon.
	EscalatedByRule Escalator = iota + 1
	// EscalatedByAgent: an agent of this daemon, with office_escalate.
	EscalatedByAgent
	// EscalatedRelayed: another daemon's agent, relayed by serve.
	EscalatedRelayed
)

// Tally is what was measured of one conflict while it stood, and in the
// Ledger's grace after it ended.
type Tally struct {
	// Messages counts SendMessage calls between sessions of its two sides.
	Messages int `json:"messages,omitempty"`
	// Held counts edits of its file Dugout held with a deny; Changed and
	// Unchanged, those whose next edit of that file by the same session
	// differed from the held one, or was the same edit again. A held edit with
	// neither was never retried.
	Held      int `json:"held,omitempty"`
	Changed   int `json:"changed,omitempty"`
	Unchanged int `json:"unchanged,omitempty"`
	// RuleEscalations, AgentEscalations and Relayed count the times it went
	// to the humans, by Escalator.
	RuleEscalations  int `json:"rule_escalations,omitempty"`
	AgentEscalations int `json:"agent_escalations,omitempty"`
	Relayed          int `json:"relayed,omitempty"`
	// Committed: a side committed or pushed while it stood.
	Committed bool `json:"committed,omitempty"`
}

// Escalations is how many times the conflict went to the humans.
func (t Tally) Escalations() int { return t.RuleEscalations + t.AgentEscalations + t.Relayed }

// Record is one conflict as a daemon measured it: from when it first stood to
// when it ended (Closed 0 while it stands), with how it ended and its Tally.
// Daemons are its sides' daemons, in its sides' order. Path, Branches and
// Workstreams are for this machine only and never marshal.
type Record struct {
	ID      string      `json:"id"`
	Repo    string      `json:"repo"`
	Daemons [2]DaemonID `json:"daemons"`
	Opened  Millis      `json:"opened"`
	Closed  Millis      `json:"closed,omitempty"`
	Outcome Outcome     `json:"outcome,omitempty"`
	Tally

	Path        string          `json:"-"`
	Branches    [2]string       `json:"-"`
	Workstreams [2]WorkstreamID `json:"-"`
}

// Ledger is one daemon's Records of the conflicts that stand and of those that
// ended less than Grace ago. A conflict that comes back within Grace, as one
// does when a check fails once or a side's snapshot is late, is the same
// Record again; past Grace, Settle hands the Record over and forgets it.
// Measures of a conflict that ended count within Grace too: an agent often
// retries its edit, or tells the other side it aligned, just after the lines
// stopped conflicting. The zero Ledger is not usable: use NewLedger.
type Ledger struct {
	grace Millis
	recs  map[string]*Record
	tips  map[WorkstreamID]string // by local workstream, as the last Sync had them
}

// NewLedger returns an empty Ledger with grace.
func NewLedger(grace Millis) *Ledger {
	return &Ledger{grace: grace, recs: map[string]*Record{}, tips: map[WorkstreamID]string{}}
}

// Sync takes what daemon d sees at now: the conflicts standing, and the tip
// of each of its live workstreams (gitscan.Snapshot.Tip). It opens a Record
// for each new conflict and reopens one that came back within grace. A
// standing Record whose conflict is gone ends: committed if a side committed
// while it stood, gone if a side of d's is no longer live, resolved
// otherwise. A workstream whose tip moved since the last Sync committed or
// pushed, and marks each conflict of it still standing. The ends go first: a
// commit seen in the same scan as the conflict going away came after the
// lines were aligned, since a commit never changes the files.
func (l *Ledger) Sync(d DaemonID, standing []Conflict, tips map[WorkstreamID]string, now Millis) {
	in := make(map[string]bool, len(standing))
	for _, c := range standing {
		in[c.ID] = true
		r := l.recs[c.ID]
		if r == nil {
			r = &Record{ID: c.ID, Repo: c.Repo, Opened: now}
			l.recs[c.ID] = r
		}
		r.Closed, r.Outcome = 0, OutcomeOpen
		r.Path = c.Path
		for i, s := range c.Sides {
			r.Daemons[i], r.Branches[i], r.Workstreams[i] = s.Daemon, s.Branch, s.Workstream
		}
	}
	for id, r := range l.recs {
		if in[id] || r.Closed != 0 {
			continue
		}
		r.Closed, r.Outcome = now, OutcomeResolved
		for i, ws := range r.Workstreams {
			if _, live := tips[ws]; r.Daemons[i] == d && !live {
				r.Outcome = OutcomeGone
			}
		}
		if r.Committed {
			r.Outcome = OutcomeCommitted
		}
	}
	for ws, tip := range tips {
		if prev, ok := l.tips[ws]; ok && prev != "" && tip != "" && prev != tip {
			for _, r := range l.recs {
				if r.Closed == 0 && (r.Workstreams[0] == ws || r.Workstreams[1] == ws) {
					r.Committed = true
				}
			}
		}
	}
	l.tips = make(map[WorkstreamID]string, len(tips))
	for ws, tip := range tips {
		l.tips[ws] = tip
	}
}

// Message counts a SendMessage from a session of workstream from to one of
// to, on every conflict between the two, and returns on how many.
func (l *Ledger) Message(from, to WorkstreamID) int {
	n := 0
	for _, r := range l.recs {
		if from != to && ((r.Workstreams[0] == from && r.Workstreams[1] == to) || (r.Workstreams[0] == to && r.Workstreams[1] == from)) {
			r.Messages++
			n++
		}
	}
	return n
}

// Held counts an edit held with a deny on each conflict of ids.
func (l *Ledger) Held(ids []string) {
	l.each(ids, func(r *Record) { r.Held++ })
}

// Retried counts the next edit after a held one, on each conflict the deny
// was about: changed when it differed from the held edit.
func (l *Ledger) Retried(ids []string, changed bool) {
	l.each(ids, func(r *Record) {
		if changed {
			r.Changed++
		} else {
			r.Unchanged++
		}
	})
}

// Escalated counts one escalation of each conflict of ids, by e.
func (l *Ledger) Escalated(ids []string, e Escalator) {
	l.each(ids, func(r *Record) {
		switch e {
		case EscalatedByRule:
			r.RuleEscalations++
		case EscalatedByAgent:
			r.AgentEscalations++
		case EscalatedRelayed:
			r.Relayed++
		}
	})
}

func (l *Ledger) each(ids []string, f func(*Record)) {
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		if r := l.recs[id]; r != nil {
			f(r)
		}
	}
}

// Get is the Record of conflict id, standing or ended within grace.
func (l *Ledger) Get(id string) (Record, bool) {
	r := l.recs[id]
	if r == nil {
		return Record{}, false
	}
	return *r, true
}

// Records is every Record the ledger holds, standing or ended within grace,
// sorted by ID.
func (l *Ledger) Records() []Record {
	out := make([]Record, 0, len(l.recs))
	for _, r := range l.recs {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Settle forgets the Records of the conflicts that ended grace or more before
// now and returns them, sorted by ID.
func (l *Ledger) Settle(now Millis) []Record {
	var out []Record
	for id, r := range l.recs {
		if r.Closed != 0 && now-r.Closed >= l.grace {
			out = append(out, *r)
			delete(l.recs, id)
		}
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Merge is one conflict as the Records its daemons sent of it make it: each
// daemon counts only what happened on its own machine, so the counts add up,
// but each one's rules escalate to its own human, so the rule escalations
// are the most any daemon made, and a relayed escalation is the asking
// daemon's own. It was committed when any daemon saw a commit, and gone when
// any saw its side go; else resolved. rs must be of one conflict, not empty.
func Merge(rs []Record) Record {
	m := rs[0]
	m.Tally = Tally{}
	for _, r := range rs {
		m.Opened = min(m.Opened, r.Opened)
		m.Closed = max(m.Closed, r.Closed)
		m.Messages += r.Messages
		m.Held += r.Held
		m.Changed += r.Changed
		m.Unchanged += r.Unchanged
		m.AgentEscalations += r.AgentEscalations
		m.RuleEscalations = max(m.RuleEscalations, r.RuleEscalations)
		m.Committed = m.Committed || r.Committed || r.Outcome == OutcomeCommitted
	}
	m.Outcome = OutcomeResolved
	if slices.ContainsFunc(rs, func(r Record) bool { return r.Outcome == OutcomeGone }) {
		m.Outcome = OutcomeGone
	}
	if m.Committed {
		m.Outcome = OutcomeCommitted
	}
	return m
}

// Totals adds up ended conflicts: how many ended each way, and their tallies.
type Totals struct {
	Conflicts   int `json:"conflicts"`
	Resolved    int `json:"resolved"`
	Committed   int `json:"committed"`
	Gone        int `json:"gone"`
	Messages    int `json:"messages"`
	Escalations int `json:"escalations"`
	Held        int `json:"held"`
	Changed     int `json:"changed"`
	Unchanged   int `json:"unchanged"`
}

// Add counts r, an ended conflict.
func (t *Totals) Add(r Record) {
	t.Conflicts++
	switch r.Outcome {
	case OutcomeCommitted:
		t.Committed++
	case OutcomeGone:
		t.Gone++
	default:
		t.Resolved++
	}
	t.Messages += r.Messages
	t.Escalations += r.Escalations()
	t.Held += r.Held
	t.Changed += r.Changed
	t.Unchanged += r.Unchanged
}

// ResolvedShare is the share of the conflicts that ended either way that
// were resolved before a commit or push, and false when none did: a gone
// conflict says nothing about it.
func (t Totals) ResolvedShare() (float64, bool) {
	n := t.Resolved + t.Committed
	if n == 0 {
		return 0, false
	}
	return float64(t.Resolved) / float64(n), true
}
