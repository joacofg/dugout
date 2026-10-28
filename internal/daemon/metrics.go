package daemon

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/peers"
	"github.com/joacofg/dugout/internal/wire"
)

// Agent metrics: what the agents do about each conflict, measured
// with no human action, in a core.Ledger. The detector's publish opens and
// ends the conflicts and sees the commits and pushes, in each scan's Tip; the
// hooks count the edits held and what the next edit of that file did, and
// the SendMessage calls between the two sides' sessions; the escalations
// count themselves. A conflict that ended goes, once its grace is past, to
// `dugout status`, to the log as "conflict ended" and to serve.

const (
	// DefaultMetricsGrace is how long an ended conflict may come back as
	// the same one before its record is final.
	DefaultMetricsGrace = 2 * time.Minute
	// maxEnded caps the ended conflicts `dugout status` lists.
	maxEnded = 10
)

// heldEdit is an edit a deny held: what it would have written, as the hook
// hashed it, and the conflicts the deny was about.
type heldEdit struct {
	hash      string
	conflicts []string
}

// syncMetricsLocked hands the ledger the conflicts the detector just
// published and the tip of each scanned workstream of this machine's. Callers
// hold d.mu.
func (d *Daemon) syncMetricsLocked(next map[string]core.Conflict, scans map[core.WorkstreamID]gitscan.Snapshot) {
	tips := make(map[core.WorkstreamID]string, len(scans))
	for ws, s := range scans {
		tips[ws] = s.Tip
	}
	d.metrics.Sync(d.id, slices.Collect(maps.Values(next)), tips, millis(d.clock()))
}

// heldBefore counts, at an edit's PreToolUse, what the session did after the
// last edit of the same file a deny held: this one is its retry, changed or
// not. A hook that sent no hash (an older dugout) counts nothing. Callers
// hold s.mu.
func (d *Daemon) heldBefore(s *Session, rel, hash string) {
	h, ok := s.held[rel]
	if !ok {
		return
	}
	delete(s.held, rel)
	if h.hash == "" || hash == "" {
		return
	}
	d.mu.Lock()
	d.metrics.Retried(h.conflicts, hash != h.hash)
	d.mu.Unlock()
}

// heldNow counts the edit a deny just held, of the conflicts handed, and
// keeps it for heldBefore. Callers hold s.mu.
func (d *Daemon) heldNow(s *Session, rel, hash string, handed []core.Conflict) {
	ids := make([]string, len(handed))
	for i, c := range handed {
		ids[i] = c.ID
	}
	s.held[rel] = heldEdit{hash: hash, conflicts: ids}
	d.mu.Lock()
	d.metrics.Held(ids)
	d.mu.Unlock()
}

// messaged counts a SendMessage from a session of ws to the session named
// to, by its name in Claude Code's registry or its ID, when that session is
// this machine's: every message between the two sides of a conflict counts on
// it, and serve hears of it for the office view. The message itself never
// reaches the daemon.
func (d *Daemon) messaged(ws core.WorkstreamID, to string) {
	to = strings.TrimSpace(to)
	if ws == "" || to == "" {
		return
	}
	type attached struct {
		id, dir string
		pid     int
		ws      core.WorkstreamID
	}
	d.mu.Lock()
	var atts []attached
	for _, s := range d.sessions {
		if wt := d.worktrees[s.Root]; wt != nil && wt.ws != "" && wt.ws != ws {
			atts = append(atts, attached{s.ID, s.ClaudeDir, s.PID, wt.ws})
		}
	}
	d.mu.Unlock()
	slices.SortFunc(atts, func(a, b attached) int { return strings.Compare(a.id, b.id) })
	// Outside d.mu: a hook does not wait on another's file reads.
	for _, a := range atts {
		if p, ok := peers.Lookup(a.dir, a.id, a.pid); a.id == to || (ok && p.Name == to) {
			d.mu.Lock()
			n := d.metrics.Message(ws, a.ws)
			d.mu.Unlock()
			if n > 0 {
				d.log.Info("message counted", "from", ws, "to", a.ws, "conflicts", n)
			}
			if d.cfg.Server != "" {
				d.uplink.Message(wire.AgentMessage{From: ws, To: a.ws})
			}
			return
		}
	}
}

// settleMetrics takes the conflicts whose grace is past from the ledger: it
// keeps them for `dugout status`, logs each one and hands them to serve.
func (d *Daemon) settleMetrics() {
	d.mu.Lock()
	rs := d.metrics.Settle(millis(d.clock()))
	for _, r := range rs {
		t := d.totals[r.Repo]
		t.Add(r)
		d.totals[r.Repo] = t
		d.ended = append(d.ended, r)
	}
	d.ended = d.ended[max(0, len(d.ended)-maxEnded):]
	d.mu.Unlock()
	for _, r := range rs {
		d.log.Info("conflict ended", "id", r.ID, "repo", r.Repo, "path", r.Path, "outcome", r.Outcome,
			"open_ms", int64(r.Closed-r.Opened), "messages", r.Messages, "escalations", r.Escalations(),
			"held", r.Held, "changed", r.Changed, "unchanged", r.Unchanged, "committed", r.Committed)
	}
	if len(rs) > 0 && d.cfg.Server != "" {
		d.uplink.AddOutcomes(rs)
	}
}

// endedLocked is what `dugout status` shows of the ended conflicts: the latest
// first, those still within their grace before the settled ones, and the
// totals by repo, both kinds counted. Callers hold d.mu.
func (d *Daemon) endedLocked() ([]core.Record, map[string]core.Totals) {
	totals := maps.Clone(d.totals)
	var ending []core.Record
	for _, r := range d.metrics.Records() {
		if r.Closed != 0 {
			ending = append(ending, r)
			t := totals[r.Repo]
			t.Add(r)
			totals[r.Repo] = t
		}
	}
	slices.SortStableFunc(ending, func(a, b core.Record) int { return cmp.Compare(b.Closed, a.Closed) })
	out := ending
	for i := len(d.ended) - 1; i >= 0 && len(out) < maxEnded; i-- {
		out = append(out, d.ended[i])
	}
	return out[:min(len(out), maxEnded)], totals
}
