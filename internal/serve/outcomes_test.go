package serve

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

func (c client) outcomes(d core.DaemonID, rs ...core.Record) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeOutcomes, FullState: core.FullState{Daemon: d}, Outcomes: rs})
}

func ended(id string, a, b core.DaemonID, o core.Outcome, t core.Tally) core.Record {
	return core.Record{ID: id, Repo: "r", Daemons: [2]core.DaemonID{a, b}, Opened: 1000, Closed: 5000, Outcome: o, Tally: t}
}

// TestAgentMetrics: serve adds up, per repo,
// the conflicts the daemons say ended, merging the two daemons' records of a
// conflict between machines once both came, or with what came once the wait
// is over; it serves the share resolved before a commit, and the messages,
// escalations and held edits of those conflicts.
func TestAgentMetrics(t *testing.T) {
	s, srv, _ := stateServer(t, "r")
	var mu sync.Mutex
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	ana, beto, admin := as(t, srv, anaTok), as(t, srv, betoTok), as(t, srv, adminTok)
	if code, _ := beto.stream("d2"); code != http.StatusOK {
		t.Fatalf("beto's stream: %d", code)
	}
	send := func(c client, d core.DaemonID, rs ...core.Record) {
		t.Helper()
		if code := c.outcomes(d, rs...); code != http.StatusNoContent {
			t.Fatalf("outcomes from %s: %d", d, code)
		}
	}

	// Ana's two sessions settled one on her machine.
	send(ana, "d1", ended("c1", "d1", "d1", core.OutcomeResolved, core.Tally{Messages: 2, Held: 1, Changed: 1}))
	send(ana, "d1", ended("c1", "d1", "d1", core.OutcomeResolved, core.Tally{Messages: 2, Held: 1, Changed: 1})) // a resend
	// One between machines: Ana's side resolved it, Beto's committed.
	send(ana, "d1", ended("c2", "d1", "d2", core.OutcomeResolved, core.Tally{Held: 1, RuleEscalations: 1, AgentEscalations: 1}))
	body := scrape(t, admin)
	if got := metricValue(t, body, `dugout_conflicts_ended_total{repo="r",outcome="resolved"}`); got != 1 {
		t.Fatalf("resolved = %d before Beto's record of c2, want 1 (c1 once):\n%s", got, body)
	}
	send(beto, "d2", ended("c2", "d1", "d2", core.OutcomeCommitted, core.Tally{Committed: true, RuleEscalations: 1, Relayed: 1}))
	// Two more between machines that only Ana's daemon reports: Beto's is
	// connected for c3, and c4's other daemon is gone with its machine.
	send(ana, "d1", ended("c3", "d1", "d2", core.OutcomeResolved, core.Tally{}), ended("c4", "d1", "d9", core.OutcomeResolved, core.Tally{}))
	// What serve never counts: a conflict Ana's daemon is not a side of, a
	// repo off the allowlist, a negative count.
	send(ana, "d1", ended("x1", "d2", "d3", core.OutcomeResolved, core.Tally{Messages: 50}),
		core.Record{ID: "x2", Repo: "elsewhere", Daemons: [2]core.DaemonID{"d1", "d1"}, Outcome: core.OutcomeResolved, Tally: core.Tally{Messages: 50}},
		ended("x3", "d1", "d1", core.OutcomeResolved, core.Tally{Messages: -5}))
	mu.Lock()
	now = now.Add(defaultOutcomeWait)
	mu.Unlock()

	body = scrape(t, admin)
	for series, want := range map[string]int{
		`dugout_conflicts_ended_total{repo="r",outcome="resolved"}`:         2, // c1, c3
		`dugout_conflicts_ended_total{repo="r",outcome="committed"}`:        1, // c2
		`dugout_conflicts_ended_total{repo="r",outcome="gone"}`:             1, // c4
		`dugout_conflict_messages_total{repo="r"}`:                          2,
		`dugout_conflict_escalations_total{repo="r"}`:                       2, // c2: one rule (each daemon told its human), one agent
		`dugout_conflict_held_edits_total{repo="r",next="changed"}`:         1,
		`dugout_conflict_held_edits_total{repo="r",next="unchanged"}`:       0,
		`dugout_conflict_held_edits_total{repo="r",next="none"}`:            1,
		`dugout_conflicts_ended_total{repo="elsewhere",outcome="resolved"}`: -1,
		`dugout_messages_total{type="outcomes"}`:                            6,
	} {
		if got := metricValue(t, body, series); got != want {
			t.Errorf("%s = %d, want %d", series, got, want)
		}
	}
	if !strings.Contains(body, `dugout_conflicts_resolved_before_commit_ratio{repo="r"} 0.6667`+"\n") ||
		!strings.Contains(body, "# TYPE dugout_conflicts_resolved_before_commit_ratio gauge") {
		t.Errorf("no share of 2 resolved out of 3 resolved or committed:\n%s", body)
	}
	for _, leak := range []string{"c1", "c2", "d1", "d9"} {
		if strings.Contains(body, leak) {
			t.Errorf("the metrics name %q:\n%s", leak, body)
		}
	}
}

// TestAgentMetricsNone: with no conflict ended, the agent metrics have their
// HELP and TYPE and no sample, and a revoked token sends none.
func TestAgentMetricsNone(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	if code := as(t, srv, carlaTok).outcomes("d3", ended("c1", "d3", "d3", core.OutcomeResolved, core.Tally{})); code != http.StatusUnauthorized {
		t.Fatalf("a revoked token's outcomes: %d, want 401", code)
	}
	body := scrape(t, as(t, srv, adminTok))
	for _, name := range []string{"dugout_conflicts_ended_total", "dugout_conflicts_resolved_before_commit_ratio", "dugout_conflict_messages_total",
		"dugout_conflict_escalations_total", "dugout_conflict_held_edits_total"} {
		if !strings.Contains(body, "# HELP "+name+" ") || !strings.Contains(body, "# TYPE "+name+" ") || strings.Contains(body, "\n"+name+"{") {
			t.Errorf("%s: want HELP and TYPE, no sample:\n%s", name, body)
		}
	}
}

// TestLabelValue: a repo identity cannot break out of its label.
func TestLabelValue(t *testing.T) {
	if got := labelValue("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Fatalf("labelValue = %q", got)
	}
}
