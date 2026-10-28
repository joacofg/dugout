package serve

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// counters are what GET /v1/admin/metrics counts since serve started. They
// live in memory only, so a restart zeroes them.
type counters struct {
	msgFullState, msgConflicts, msgOutcomes    atomic.Int64
	msgOther                                   atomic.Int64
	bundlesRelayed, staleEpochs, refusedTooOld atomic.Int64
}

// countMessage counts one message of type typ taken past the protocol and
// daemon checks.
func (c *counters) countMessage(typ string) {
	switch typ {
	case wire.TypeFullState:
		c.msgFullState.Add(1)
	case wire.TypeConflicts:
		c.msgConflicts.Add(1)
	case wire.TypeOutcomes:
		c.msgOutcomes.Add(1)
	default:
		c.msgOther.Add(1)
	}
}

// handleMetrics answers the metrics in Prometheus text format. Its names and
// labels are fixed, but for the repo of the agent metrics: no token, no
// person, no path, no code.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	ended := s.endedTotals()
	s.mu.Lock()
	daemons := 0
	for _, set := range s.subs {
		daemons += len(set)
	}
	workstreams, warnings, conflicts := len(s.st.Workstreams()), len(s.st.Warnings()), len(s.conflictUnion())
	paused, persons := 0, 0
	if s.stored.Paused {
		paused = 1
	}
	for _, u := range s.stored.Users {
		if !u.Revoked {
			persons++
		}
	}
	s.mu.Unlock()

	var b strings.Builder
	metric := func(name, typ, help string, samples ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, sm := range samples {
			b.WriteString(sm + "\n")
		}
	}
	gauge := func(name, help string, v int) {
		metric(name, "gauge", help, fmt.Sprintf("%s %d", name, v))
	}
	const reset = " Resets when serve restarts."
	c := &s.counts
	gauge("dugout_daemons_connected", "Daemons with an open warnings stream.", daemons)
	gauge("dugout_workstreams", "Workstreams the daemons see.", workstreams)
	gauge("dugout_warnings", "Open overlap warnings.", warnings)
	gauge("dugout_conflicts", "Open reported conflicts.", conflicts)
	metric("dugout_messages_total", "counter", "Messages received on POST /v1/msg, by type."+reset,
		fmt.Sprintf(`dugout_messages_total{type="full_state"} %d`, c.msgFullState.Load()),
		fmt.Sprintf(`dugout_messages_total{type="conflicts"} %d`, c.msgConflicts.Load()),
		fmt.Sprintf(`dugout_messages_total{type="outcomes"} %d`, c.msgOutcomes.Load()),
		fmt.Sprintf(`dugout_messages_total{type="other"} %d`, c.msgOther.Load()))
	metric("dugout_messages_refused_total", "counter", "Messages refused on POST /v1/msg, by reason: too_old is a dugout below the minimum protocol (426)."+reset,
		fmt.Sprintf(`dugout_messages_refused_total{reason="too_old"} %d`, c.refusedTooOld.Load()))
	metric("dugout_bundles_relayed_total", "counter", "Bundles delivered to another daemon."+reset,
		fmt.Sprintf("dugout_bundles_relayed_total %d", c.bundlesRelayed.Load()))
	metric("dugout_stale_epochs_total", "counter", "Messages ignored for having a stale epoch."+reset,
		fmt.Sprintf("dugout_stale_epochs_total %d", c.staleEpochs.Load()))
	gauge("dugout_paused", "1 if the team is paused.", paused)
	gauge("dugout_persons", "People with an issued, unrevoked token.", persons)
	agentMetrics(metric, ended, reset)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprint(w, b.String())
}

// agentMetrics writes the agent metrics of the conflicts that ended, by repo,
// sorted; a metric with no repo yet has no sample.
func agentMetrics(metric func(name, typ, help string, samples ...string), ended map[string]core.Totals, reset string) {
	repos := slices.Sorted(maps.Keys(ended))
	samples := func(name string, f func(t core.Totals) map[string]string) []string {
		var out []string
		for _, repo := range repos {
			values := f(ended[repo])
			for _, labels := range slices.Sorted(maps.Keys(values)) {
				out = append(out, fmt.Sprintf(`%s{repo="%s"%s} %s`, name, labelValue(repo), labels, values[labels]))
			}
		}
		return out
	}
	count := func(name string, f func(t core.Totals) int) []string {
		return samples(name, func(t core.Totals) map[string]string { return map[string]string{"": strconv.Itoa(f(t))} })
	}
	metric("dugout_conflicts_ended_total", "counter", "Conflicts that ended, by repo and outcome: resolved before either side's commit or push, committed while standing, or gone with a side's worktree, branch or machine."+reset,
		samples("dugout_conflicts_ended_total", func(t core.Totals) map[string]string {
			return map[string]string{`,outcome="resolved"`: strconv.Itoa(t.Resolved), `,outcome="committed"`: strconv.Itoa(t.Committed), `,outcome="gone"`: strconv.Itoa(t.Gone)}
		})...)
	metric("dugout_conflicts_resolved_before_commit_ratio", "gauge", "Share of the conflicts that ended resolved or committed that were resolved before either side's commit or push, by repo."+reset,
		samples("dugout_conflicts_resolved_before_commit_ratio", func(t core.Totals) map[string]string {
			if share, ok := t.ResolvedShare(); ok {
				return map[string]string{"": strconv.FormatFloat(share, 'g', 4, 64)}
			}
			return nil
		})...)
	metric("dugout_conflict_messages_total", "counter", "SendMessage calls between the two sides' sessions, over the conflicts that ended, by repo."+reset,
		count("dugout_conflict_messages_total", func(t core.Totals) int { return t.Messages })...)
	metric("dugout_conflict_escalations_total", "counter", "Times the conflicts that ended went to the humans, by a fixed rule or an agent, by repo."+reset,
		count("dugout_conflict_escalations_total", func(t core.Totals) int { return t.Escalations })...)
	metric("dugout_conflict_held_edits_total", "counter", "Edits Dugout held with a deny, over the conflicts that ended, by repo and what the same session's next edit of the file did: changed, unchanged (the same edit again) or none."+reset,
		samples("dugout_conflict_held_edits_total", func(t core.Totals) map[string]string {
			return map[string]string{`,next="changed"`: strconv.Itoa(t.Changed), `,next="unchanged"`: strconv.Itoa(t.Unchanged),
				`,next="none"`: strconv.Itoa(max(0, t.Held-t.Changed-t.Unchanged))}
		})...)
}
