package daemon

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// handleStatus tells what this daemon sees, for `dugout status`: its tracked
// worktrees with files in flight, sorted by root, with the overlaps,
// conflicts and possible duplicates that touch each, each conflict with what
// was measured of it so far, the conflicts that ended, with their
// totals by repo, and the judge that finds the duplicates. Every text another
// workstream wrote goes through Sanitize, as it would into a hook answer. It
// answers during a pause, naming it: the person asking is a human, not an
// agent.
func (d *Daemon) handleStatus() wire.Response {
	out := wire.Status{Daemon: string(d.id), Server: d.cfg.Server, Connected: d.uplink.Connected(), Judge: d.statusJudge(), Workstreams: []wire.StatusWorkstream{},
		Ended: []wire.StatusEnded{}, Totals: []wire.StatusTotals{}}
	if out.Server == "off" {
		out.Server = ""
	}
	if paused, team := d.pausedNow(); team {
		out.Paused = "team"
	} else if paused {
		out.Paused = "local"
	}
	type tracked struct {
		root, repo, branch, intent string
		ws                         core.WorkstreamID
		files                      []string
	}
	d.mu.Lock()
	var wts []tracked
	for _, wt := range d.worktrees {
		if wt.snap != nil && len(wt.snap.Files) > 0 {
			wts = append(wts, tracked{wt.root, wt.snap.Repo, wt.snap.Branch, wt.intent, wt.ws, slices.Clone(wt.snap.Files)})
		}
	}
	// The other side's declared intent as this daemon's own core has it, which
	// is fresher than what a conflict kept from serve carries.
	fresh := map[core.WorkstreamID]string{}
	for _, ws := range d.local.Workstreams() {
		fresh[ws.ID] = ws.Intent
	}
	tallies := map[string]core.Tally{}
	for _, r := range d.metrics.Records() {
		tallies[r.ID] = r.Tally
	}
	ended, totals := d.endedLocked()
	d.mu.Unlock()
	for _, r := range ended {
		out.Ended = append(out.Ended, wire.StatusEnded{Repo: Sanitize(r.Repo), Path: Sanitize(r.Path),
			Branches: [2]string{Sanitize(r.Branches[0]), Sanitize(r.Branches[1])}, Outcome: string(r.Outcome), Tally: r.Tally})
	}
	for _, repo := range slices.Sorted(maps.Keys(totals)) {
		out.Totals = append(out.Totals, wire.StatusTotals{Repo: Sanitize(repo), Totals: totals[repo]})
	}
	slices.SortFunc(wts, func(a, b tracked) int { return strings.Compare(a.root, b.root) })
	intentOf := func(s core.Side) string {
		if in, ok := fresh[s.Workstream]; ok {
			return SanitizeIntent(in)
		}
		return SanitizeIntent(s.Intent)
	}
	for _, wt := range wts {
		w := wire.StatusWorkstream{Root: Sanitize(wt.root), Repo: Sanitize(wt.repo), Branch: Sanitize(wt.branch), Files: wt.files, Intent: SanitizeIntent(wt.intent),
			Overlaps: []wire.StatusOverlap{}, Conflicts: []wire.StatusConflict{}, Duplicates: []wire.StatusDuplicate{}}
		for _, c := range d.conflictsFor(wt.ws) {
			i := mineOf(c.Sides, wt.ws)
			other := c.Sides[1-i]
			lines := ""
			if c.Kind != "file" && len(c.Lines[i]) > 0 {
				lines, _ = spanPhrase(c.Lines[i])
			}
			w.Conflicts = append(w.Conflicts, wire.StatusConflict{Path: Sanitize(c.Path), Branch: Sanitize(other.Branch),
				Daemon: Sanitize(string(other.Daemon)), Lines: lines, Intent: intentOf(other), Tally: tallies[c.ID]})
		}
		for _, o := range d.overlapsFor(wt.ws) {
			other := o.Sides[1-mineOf(o.Sides, wt.ws)]
			w.Overlaps = append(w.Overlaps, wire.StatusOverlap{Path: Sanitize(o.Path), Branch: Sanitize(other.Branch),
				Daemon: Sanitize(string(other.Daemon)), Intent: intentOf(other)})
		}
		for _, dup := range d.duplicatesFor(wt.ws) {
			other := dup.Sides[1-mineOf(dup.Sides, wt.ws)]
			w.Duplicates = append(w.Duplicates, wire.StatusDuplicate{Branch: Sanitize(other.Branch), Daemon: Sanitize(string(other.Daemon)),
				Intent: SanitizeIntent(other.Intent), Reason: Sanitize(dup.Reason)})
		}
		byPath := func(a, b string) int { return strings.Compare(a, b) }
		slices.SortFunc(w.Conflicts, func(a, b wire.StatusConflict) int { return byPath(a.Path, b.Path) })
		slices.SortFunc(w.Overlaps, func(a, b wire.StatusOverlap) int { return byPath(a.Path, b.Path) })
		out.Workstreams = append(out.Workstreams, w)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	return wire.Response{Output: b}
}
