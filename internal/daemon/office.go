package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/peers"
	"github.com/joacofg/dugout/internal/wire"
)

// officeFiles caps the files an office_who row names.
const officeFiles = 10

// sessionNames names, for each workstream of want on this machine, the live
// Claude Code session attached to its worktree, the address another session
// messages its agent at (package peers): the one started last when several
// are. A session the registry gives a PID it lacked, one the daemon
// registered after a restart, keeps that PID, so pruneSessions drops it when
// it ends.
func (d *Daemon) sessionNames(want map[core.WorkstreamID]bool) map[core.WorkstreamID]string {
	type attached struct {
		s        *Session
		id, dir  string
		pid      int
		ws       core.WorkstreamID
		knownPID bool
	}
	d.mu.Lock()
	var atts []attached
	for _, s := range d.sessions {
		if wt := d.worktrees[s.Root]; wt != nil && want[wt.ws] && s.ClaudeDir != "" {
			atts = append(atts, attached{s: s, id: s.ID, dir: s.ClaudeDir, pid: s.PID, ws: wt.ws, knownPID: s.PID > 0})
		}
	}
	d.mu.Unlock()
	// Outside d.mu: a hook does not wait on another's file reads.
	names := map[core.WorkstreamID]string{}
	started := map[core.WorkstreamID]int64{}
	for _, a := range atts {
		p, ok := peers.Lookup(a.dir, a.id, a.pid)
		if !ok {
			continue
		}
		if !a.knownPID {
			d.mu.Lock()
			if a.s.PID == 0 {
				a.s.PID = p.PID
			}
			d.mu.Unlock()
		}
		if cur, ok := names[a.ws]; !ok || p.StartedAt > started[a.ws] || (p.StartedAt == started[a.ws] && p.Name < cur) {
			names[a.ws], started[a.ws] = p.Name, p.StartedAt
		}
	}
	return names
}

// viewFor is what touches ws, as the hooks answer it: its overlaps,
// conflicts and duplicates, with the session of each side on this machine and
// each conflict's stances, and its office, the other workstreams of ws's repo
// on this machine that are none of those and have files in flight or a
// session open.
func (d *Daemon) viewFor(ws core.WorkstreamID) View {
	v := View{Overlaps: d.overlapsFor(ws), Conflicts: d.conflictsFor(ws), Duplicates: d.duplicatesFor(ws)}
	want := map[core.WorkstreamID]bool{}
	named := map[core.WorkstreamID]bool{ws: true} // ws and those its overlaps or conflicts name
	for _, w := range v.Overlaps {
		for _, s := range w.Sides {
			want[s.Workstream], named[s.Workstream] = s.Daemon == d.id && s.Workstream != ws, true
		}
	}
	for _, c := range v.Conflicts {
		for _, s := range c.Sides {
			want[s.Workstream], named[s.Workstream] = s.Daemon == d.id && s.Workstream != ws, true
		}
	}
	for _, dup := range v.Duplicates {
		for _, s := range dup.Sides {
			want[s.Workstream], named[s.Workstream] = s.Daemon == d.id && s.Workstream != ws, true
		}
	}
	var candidates []core.Side
	files := map[core.WorkstreamID]bool{}
	d.mu.Lock()
	repo := ""
	for _, wt := range d.worktrees {
		if wt.ws == ws && wt.snap != nil {
			repo = wt.snap.Repo
		}
	}
	for _, wt := range d.worktrees {
		if repo != "" && wt.snap != nil && wt.snap.Repo == repo && !named[wt.ws] {
			candidates = append(candidates, core.Side{Daemon: d.id, Workstream: wt.ws, Branch: wt.snap.Branch, Intent: wt.intent})
			want[wt.ws], files[wt.ws] = true, len(wt.snap.Files) > 0
		}
	}
	for i, c := range v.Conflicts {
		v.Conflicts[i].Stances = d.stancesLocked(c)
	}
	d.mu.Unlock()
	names := d.sessionNames(want)
	stamp := func(sides *[2]core.Side) {
		for i := range sides {
			if sides[i].Daemon == d.id {
				sides[i].Session = names[sides[i].Workstream]
			}
		}
	}
	for i := range v.Overlaps {
		stamp(&v.Overlaps[i].Sides)
	}
	for i := range v.Conflicts {
		stamp(&v.Conflicts[i].Sides)
	}
	for i := range v.Duplicates {
		stamp(&v.Duplicates[i].Sides)
	}
	slices.SortFunc(candidates, func(a, b core.Side) int { return strings.Compare(string(a.Workstream), string(b.Workstream)) })
	for _, c := range candidates {
		if c.Session = names[c.Workstream]; c.Session != "" || files[c.Workstream] {
			v.Office = append(v.Office, c)
		}
	}
	return v
}

// stancesLocked is what each side of c told the registry, in c's side order.
// d.mu held.
func (d *Daemon) stancesLocked(c core.Conflict) [2]core.Stance {
	by := d.stances[c.ID]
	return [2]core.Stance{by[c.Sides[0].Workstream], by[c.Sides[1].Workstream]}
}

// pruneStances drops what the registry holds for a span once a workstream of
// its pair is gone, or was never this machine's. A worktree that comes back on
// the same root and branch gets the same workstream ID, so a stance outliving
// its pair would decide a new negotiation.
func (d *Daemon) pruneStances() {
	d.mu.Lock()
	defer d.mu.Unlock()
	live := map[core.WorkstreamID]bool{}
	for _, wt := range d.worktrees {
		live[wt.ws] = true
	}
	for id := range d.stances {
		c, ok := d.spans[id]
		if !ok || !live[c.Sides[0].Workstream] || !live[c.Sides[1].Workstream] {
			delete(d.stances, id)
		}
	}
	for id, c := range d.spans {
		if !live[c.Sides[0].Workstream] || !live[c.Sides[1].Workstream] {
			delete(d.spans, id)
		}
	}
}

// handleStance records in the office registry that req.SessionID's
// workstream keeps (claim) or gives (yield) the span of the conflict whose ID
// starts with req.Conflict, and tells who keeps it now and whom to tell. The
// stance stands until that side says otherwise or a workstream of the pair is
// gone, so it holds if the conflict resolves and comes back. A span that
// already resolved takes one too: the side that yields usually aligns first
// and records it after. Only a conflict between two workstreams of this
// machine takes one: serve does not carry the registry yet, so the other
// side's daemon would not see it, and the keeper rule stands for both. While
// Dugout is paused, nothing is recorded.
func (d *Daemon) handleStance(req wire.Request, st core.Stance) wire.Response {
	if paused, team := d.pausedNow(); team {
		return wire.Response{Error: "Dugout is paused for the whole team: I recorded nothing."}
	} else if paused {
		return wire.Response{Error: "Dugout is paused on this machine: I recorded nothing."}
	}
	prefix := strings.ToLower(strings.TrimSpace(req.Conflict))
	if prefix == "" {
		return wire.Response{Error: "conflict_id is missing: the one a Dugout warning or office_who shows."}
	}
	d.mu.Lock()
	s := d.sessions[req.SessionID]
	var ws core.WorkstreamID
	if s != nil {
		if wt := d.worktrees[s.Root]; wt != nil {
			ws = wt.ws
		}
	}
	if ws == "" {
		d.mu.Unlock()
		return wire.Response{Error: "Dugout does not know that session: use the session_id Dugout gave you at the start."}
	}
	find := func(cs map[string]core.Conflict) []core.Conflict {
		var hits []core.Conflict
		for _, c := range cs {
			if touches(c.Sides, ws) && strings.HasPrefix(c.ID, prefix) {
				hits = append(hits, c)
			}
		}
		return hits
	}
	hits, resolved := find(d.conflicts), false
	if len(hits) == 0 {
		hits = find(d.spans)
		resolved = len(hits) > 0
	}
	if len(hits) != 1 {
		d.mu.Unlock()
		if len(hits) == 0 {
			return wire.Response{Error: "Dugout does not see that conflict on your branch. office_who shows yours."}
		}
		return wire.Response{Error: "that conflict_id is the start of more than one of your conflicts: pass it in full, as office_who shows it."}
	}
	c := hits[0]
	other := c.Sides[1-mineOf(c.Sides, ws)]
	if other.Daemon != d.id {
		d.mu.Unlock()
		return wire.Response{Error: "the other branch of that conflict is on another machine, and the registry is still only this machine's: Dugout's fixed rule applies. Agree on it with their agent some other way."}
	}
	by := d.stances[c.ID]
	if by == nil {
		by = map[core.WorkstreamID]core.Stance{}
		d.stances[c.ID] = by
	}
	by[ws] = st
	c.Stances = d.stancesLocked(c)
	d.mu.Unlock()
	d.reportConflicts()
	d.log.Info("stance", "id", c.ID, "workstream", ws, "stance", st, "owner", c.Owner(), "resolved", resolved)

	i := 1 - mineOf(c.Sides, ws)
	c.Sides[i].Session = d.sessionNames(map[core.WorkstreamID]bool{c.Sides[i].Workstream: true})[c.Sides[i].Workstream]
	s.mu.Lock()
	mode := s.Mode
	s.mu.Unlock()
	st2 := wire.Stance{ID: short(c.ID), Path: Sanitize(c.Path), Keeps: c.Owner() == ws, Why: whyOwner(ws, c), Resolved: resolved,
		Branch: Sanitize(c.Sides[i].Branch), Session: Sanitize(c.Sides[i].Session), Text: sideText(c, i, Sanitize(c.Sides[i].Branch))}
	if !st2.Keeps && !resolved {
		st2.Advice = contextYield(mode, true)
	}
	b, err := json.Marshal(st2)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	return wire.Response{Output: b}
}

// handleOffice tells who works in the repo of the tracked worktree that holds
// req.Path, the deepest one when worktrees nest: every other workstream of
// this machine with files in flight or a session open, with the session to
// message on it, and, with the uplink on, the other daemons' as serve has
// them, asked within whoTimeout; each with its conflicts with the asking
// worktree and who keeps them, and whether the judge found it a duplicate of
// the asking worktree's task. While paused, it tells only that.
func (d *Daemon) handleOffice(req wire.Request) wire.Response {
	if paused, team := d.pausedNow(); team {
		return wire.Response{Output: json.RawMessage(`{"tracked":true,"paused":"team"}`)}
	} else if paused {
		return wire.Response{Output: json.RawMessage(`{"tracked":true,"paused":"local"}`)}
	}
	notTracked := wire.Response{Output: json.RawMessage(`{"tracked":false}`)}
	root, _ := d.deepestRoot(req.Path)
	if root == "" {
		return notTracked
	}
	d.mu.Lock()
	var own core.WorkstreamID
	var repo, branch string
	if wt := d.worktrees[root]; wt != nil && wt.snap != nil {
		own, repo, branch = wt.ws, wt.snap.Repo, wt.snap.Branch
	}
	d.mu.Unlock()
	out := wire.Office{Tracked: true, Server: "off", Branch: Sanitize(branch), Workstreams: []wire.OfficeWorkstream{}}
	if own == "" {
		b, _ := json.Marshal(out)
		return wire.Response{Output: b}
	}
	var remote []core.Workstream
	if d.cfg.Server != "" {
		ctx, cancel := context.WithTimeout(d.ctx, whoTimeout)
		dump, err := d.uplink.State(ctx)
		cancel()
		if err != nil {
			out.Server = "down"
			d.log.Warn("office without serve", "err", err)
		} else {
			out.Server, remote = "ok", dump.Workstreams
		}
	}
	v := d.viewFor(own)
	type row struct {
		ws core.WorkstreamID
		wire.OfficeWorkstream
	}
	rows := map[core.WorkstreamID]*row{}
	add := func(s core.Side, files []string, remote bool) {
		if s.Workstream == own || rows[s.Workstream] != nil {
			return
		}
		r := &row{ws: s.Workstream, OfficeWorkstream: wire.OfficeWorkstream{Branch: Sanitize(s.Branch), Daemon: Sanitize(string(s.Daemon)),
			Intent: SanitizeIntent(s.Intent), Session: Sanitize(s.Session), Remote: remote, Files: []string{}, Conflicts: []wire.OfficeConflict{}}}
		for i, f := range files {
			if i == officeFiles {
				r.FilesMore = len(files) - officeFiles
				break
			}
			r.Files = append(r.Files, Sanitize(f))
		}
		rows[s.Workstream] = r
	}
	// Files in flight as serve has them, and this machine's as its own core
	// does, fresher.
	files := map[core.WorkstreamID][]string{}
	for _, ws := range remote {
		files[ws.ID] = ws.Files
	}
	d.mu.Lock()
	for _, ws := range d.local.Workstreams() {
		files[ws.ID] = ws.Files
	}
	d.mu.Unlock()
	for _, w := range v.Overlaps {
		s := w.Sides[1-mineOf(w.Sides, own)]
		add(s, files[s.Workstream], s.Daemon != d.id)
	}
	for _, c := range v.Conflicts {
		s := c.Sides[1-mineOf(c.Sides, own)]
		add(s, files[s.Workstream], s.Daemon != d.id)
	}
	for _, dup := range v.Duplicates {
		s := dup.Sides[1-mineOf(dup.Sides, own)]
		add(s, files[s.Workstream], s.Daemon != d.id)
	}
	for _, s := range v.Office {
		add(s, files[s.Workstream], false)
	}
	for _, ws := range remote {
		if owner, _, _ := strings.Cut(string(ws.ID), ":"); core.DaemonID(owner) != d.id && ws.Repo == repo {
			add(core.Side{Daemon: core.DaemonID(owner), Workstream: ws.ID, Branch: ws.Branch, Intent: ws.Intent}, ws.Files, true)
		}
	}
	for _, c := range v.Conflicts {
		if r := rows[c.Sides[1-mineOf(c.Sides, own)].Workstream]; r != nil {
			r.Conflicts = append(r.Conflicts, wire.OfficeConflict{ID: short(c.ID), Path: Sanitize(c.Path), Keeps: c.Owner() == own, Why: whyOwner(own, c)})
		}
	}
	for _, dup := range v.Duplicates {
		if r := rows[dup.Sides[1-mineOf(dup.Sides, own)].Workstream]; r != nil {
			r.Duplicate = &wire.OfficeDuplicate{ID: short(dup.ID), Reason: Sanitize(dup.Reason)}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		out.Workstreams = append(out.Workstreams, rows[id].OfficeWorkstream)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	return wire.Response{Output: b}
}

// deepestRoot is the tracked worktree that holds path, absolute, the deepest
// one when worktrees nest, and path relative to it; "" when none does.
func (d *Daemon) deepestRoot(path string) (string, string) {
	if !filepath.IsAbs(path) {
		return "", "" // relPath would read it against every root
	}
	d.mu.Lock()
	roots := make([]string, 0, len(d.worktrees))
	for r := range d.worktrees {
		roots = append(roots, r)
	}
	d.mu.Unlock()
	root, rel := "", ""
	for _, r := range roots {
		if p := relPath(r, path); p != "" && len(r) > len(root) {
			root, rel = r, p
		}
	}
	return root, rel
}

// reportConflicts hands serve the current conflicts with what each side told
// the registry, so the office view shows who keeps each span; the outbox
// sends them only when they changed. Without a server it does nothing.
func (d *Daemon) reportConflicts() {
	if d.cfg.Server == "" {
		return
	}
	d.mu.Lock()
	cs := make([]core.Conflict, 0, len(d.conflicts))
	for _, c := range d.conflicts {
		c.Stances = d.stancesLocked(c)
		cs = append(cs, c)
	}
	d.mu.Unlock()
	d.uplink.SetConflicts(cs)
}
