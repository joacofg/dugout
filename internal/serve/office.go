package serve

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/wire"
)

// The office view: what the daemons tell serve only for it, the
// agents' messages and the escalations, kept in memory like workstreams, and
// the view each person may see, built from those and serve's state.

// How much of it each repo keeps: the newest messages and escalations, and
// the conflicts an escalation names.
const (
	maxOfficeMessages    = 50
	maxOfficeEscalations = 20
	maxEscalatedIDs      = 16
)

// officeEscalation is one recorded escalation and the key it is deduplicated
// by: its trigger and its sorted conflicts. ended is set once none of its
// conflicts stands, so a recurrence is a new escalation.
type officeEscalation struct {
	key   string
	ended bool
	rec   wire.OfficeEscalation
}

// officeLog is what serve keeps for the view, by repo.
type officeLog struct {
	messages    map[string][]wire.OfficeMessage
	escalations map[string][]officeEscalation
	// viewers are the open office streams, each signalled on a change.
	viewers map[*viewer]struct{}
}

func newOfficeLog() officeLog {
	return officeLog{messages: map[string][]wire.OfficeMessage{}, escalations: map[string][]officeEscalation{}, viewers: map[*viewer]struct{}{}}
}

// viewer is one open office stream. changed holds one pending signal; gone
// closes when the viewer's person is revoked, or its view token
// evicted.
type viewer struct {
	user    string
	token   string // the hash of the view token it reads with, "" for a person's token
	changed chan struct{}
	gone    chan struct{}
}

// officeChangedLocked tells every open office stream that its view may have
// changed. Callers hold s.mu.
func (s *Server) officeChangedLocked() {
	for v := range s.office.viewers {
		select {
		case v.changed <- struct{}{}:
		default:
		}
	}
}

// workstreamLocked finds workstream id in serve's state. Callers hold s.mu.
func (s *Server) workstreamLocked(id core.WorkstreamID) (core.Workstream, bool) {
	for _, w := range s.st.Workstreams() {
		if w.ID == id {
			return w, true
		}
	}
	return core.Workstream{}, false
}

// handleAgentMessage records that a session of m.From messaged one of m.To.
// It takes it only when both workstreams are daemon d's and in one repo: a
// daemon tells of its own sessions, and the local channel reaches no other
// machine.
func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request, d core.DaemonID, m *wire.AgentMessage) {
	if m == nil {
		http.Error(w, "message without message", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revokedLocked(r) {
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	from, okF := s.workstreamLocked(m.From)
	to, okT := s.workstreamLocked(m.To)
	ownF, _ := s.st.Owner(m.From)
	ownT, _ := s.st.Owner(m.To)
	if !okF || !okT || ownF != d || ownT != d || from.Repo != to.Repo || m.From == m.To {
		http.Error(w, "not two workstreams of this daemon in one repo", http.StatusUnprocessableEntity)
		return
	}
	list := append(s.office.messages[from.Repo], wire.OfficeMessage{At: s.clock().UTC(), From: m.From, To: m.To})
	s.office.messages[from.Repo] = slices.Clone(list[max(0, len(list)-maxOfficeMessages):])
	s.officeChangedLocked()
	w.WriteHeader(http.StatusNoContent)
}

// handleEscalated records an escalation daemon d raised, of conflicts it
// reported, with the repo, paths and sides of serve's own copy of them. The
// same trigger on the same conflicts is one record, so the two daemons of a
// pair that both run the time rule make one.
func (s *Server) handleEscalated(w http.ResponseWriter, r *http.Request, d core.DaemonID, n *wire.EscalationNotice) {
	if n == nil {
		http.Error(w, "escalated without escalated", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revokedLocked(r) {
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	ids := slices.Compact(slices.Sorted(slices.Values(n.Conflicts)))
	var cs []core.Conflict
	for _, id := range ids {
		if c, ok := s.reported[d][id]; ok {
			cs = append(cs, c)
		}
	}
	switch {
	case n.Trigger != wire.TriggerAgent && n.Trigger != wire.TriggerTime && n.Trigger != wire.TriggerDisputed:
		http.Error(w, "unknown trigger", http.StatusUnprocessableEntity)
		return
	case len(ids) == 0 || len(ids) > maxEscalatedIDs || len(cs) != len(ids):
		http.Error(w, "not conflicts this daemon reported", http.StatusUnprocessableEntity)
		return
	}
	repo, sides := cs[0].Repo, [2]core.WorkstreamID{cs[0].Sides[0].Workstream, cs[0].Sides[1].Workstream}
	// An agent's names its asker first, so the view says who asked.
	if n.Trigger == wire.TriggerAgent && n.From != "" {
		switch n.From {
		case sides[0]:
		case sides[1]:
			sides[0], sides[1] = sides[1], sides[0]
		default:
			http.Error(w, "the asker is not a side of the conflict", http.StatusUnprocessableEntity)
			return
		}
	}
	var paths []string
	for _, c := range cs {
		if c.Repo != repo {
			http.Error(w, "conflicts of more than one repo", http.StatusUnprocessableEntity)
			return
		}
		paths = append(paths, c.Path)
	}
	key := n.Trigger + "\x00" + strings.Join(ids, "\x00")
	list := s.office.escalations[repo]
	rec := wire.OfficeEscalation{At: s.clock().UTC(), Conflicts: ids, Sides: sides, Trigger: n.Trigger,
		Reason: firstRunes(n.Reason, maxIntent), Urgent: n.Urgent}
	// A repeat updates the standing record, so the two daemons of a pair
	// running the time rule make one and a re-sent escalation shows live.
	if i := slices.IndexFunc(list, func(e officeEscalation) bool { return e.key == key && !e.ended }); i >= 0 {
		rec.Paths = list[i].rec.Paths
		list = slices.Clone(list)
		list[i].rec = rec
	} else {
		slices.Sort(paths)
		rec.Paths = slices.Compact(paths)
		list = append(slices.DeleteFunc(slices.Clone(list), func(e officeEscalation) bool { return e.key == key }), officeEscalation{key: key, rec: rec})
	}
	s.office.escalations[repo] = list[max(0, len(list)-maxOfficeEscalations):]
	s.officeChangedLocked()
	w.WriteHeader(http.StatusNoContent)
}

// endEscalationsLocked marks the escalations none of whose conflicts stands
// any more. Callers hold s.mu.
func (s *Server) endEscalationsLocked() {
	standing := map[string]bool{}
	for _, c := range s.officeConflictsLocked() {
		standing[c.ID] = true
	}
	for _, es := range s.office.escalations {
		for i := range es {
			if !slices.ContainsFunc(es[i].rec.Conflicts, func(id string) bool { return standing[id] }) {
				es[i].ended = true
			}
		}
	}
}

// officeViewLocked is the office as user may see it: one entry per repo on
// the allowlist they see that holds a workstream, a conflict, a message or an
// escalation; a repo that left the allowlist takes its records with it. It
// holds no view logic beyond picking what user sees: a client lays it out.
// Callers hold s.mu.
func (s *Server) officeViewLocked(user string) wire.OfficeView {
	repos := map[string]*wire.OfficeRepo{}
	entry := func(repo string) *wire.OfficeRepo {
		repo = gitscan.NormalizeRemote(repo)
		if !s.allowedLocked(repo) || !s.seesLocked(user, repo) {
			return nil
		}
		if repos[repo] == nil {
			r := wire.NewOfficeRepo(repo)
			repos[repo] = &r
		}
		return repos[repo]
	}
	for _, w := range s.st.Workstreams() {
		if e := entry(w.Repo); e != nil {
			if w.Files == nil {
				w.Files = []string{} // the view's lists are never null
			}
			e.Workstreams = append(e.Workstreams, w)
		}
	}
	for _, w := range s.st.Warnings() {
		if e := entry(w.Repo); e != nil {
			e.Shared = append(e.Shared, wire.OfficeShared{Path: w.Path, Sides: [2]core.WorkstreamID{w.Sides[0].Workstream, w.Sides[1].Workstream}})
		}
	}
	standing := map[string]bool{}
	for _, c := range s.officeConflictsLocked() {
		standing[c.ID] = true
		if e := entry(c.Repo); e != nil {
			e.Conflicts = append(e.Conflicts, wire.OfficeSpan{ID: c.ID, Path: c.Path, Kind: c.Kind,
				Sides: [2]core.WorkstreamID{c.Sides[0].Workstream, c.Sides[1].Workstream}, Lines: c.Lines, Owner: c.Owner(), Why: c.Why()})
		}
	}
	for repo, ms := range s.office.messages {
		if e := entry(repo); e != nil {
			e.Messages = append(e.Messages, ms...)
		}
	}
	for repo, es := range s.office.escalations {
		if e := entry(repo); e != nil {
			for _, oe := range es {
				rec := oe.rec
				rec.Conflicts, rec.Paths = slices.Clone(rec.Conflicts), slices.Clone(rec.Paths)
				rec.Open = slices.ContainsFunc(rec.Conflicts, func(id string) bool { return standing[id] })
				e.Escalations = append(e.Escalations, rec)
			}
		}
	}
	v := wire.OfficeView{At: s.clock().UTC(), Repos: []wire.OfficeRepo{}}
	for _, repo := range slices.Sorted(maps.Keys(repos)) {
		v.Repos = append(v.Repos, *repos[repo])
	}
	return v
}

// officeConflictsLocked is every reported conflict once, sorted by ID, the
// copy with stances first: only the daemon of both sides holds any. Callers
// hold s.mu.
func (s *Server) officeConflictsLocked() []core.Conflict {
	seen := map[string]core.Conflict{}
	for _, m := range s.reported {
		for id, c := range m {
			if old, ok := seen[id]; !ok || old.Stances == [2]core.Stance{} {
				seen[id] = c
			}
		}
	}
	out := make([]core.Conflict, 0, len(seen))
	for _, id := range sortedIDs(seen) {
		out = append(out, seen[id])
	}
	return out
}

// View tokens: what the office page reads with, so the person's own token,
// which can write as their daemons, never enters a browser. They live in
// memory only.
const (
	viewTokenTTL  = 12 * time.Hour
	maxViewTokens = 8 // per person; a ninth evicts the oldest
)

// officeCoalesce is the least time between two events of an office stream; a
// var so tests can shorten it.
var officeCoalesce = 250 * time.Millisecond

// viewToken is one view token's person and expiry.
type viewToken struct {
	user    string
	expires time.Time
}

// isViewRead reports whether r is one of the two requests a view token may
// make: GET /v1/office or GET /v1/office/stream, its path cleaned as the mux
// cleans it.
func isViewRead(r *http.Request) bool {
	p := path.Clean(r.URL.Path)
	return r.Method == http.MethodGet && (p == "/v1/office" || p == "/v1/office/stream") && p == r.URL.Path
}

// viewUserLocked is the person of view token tok when it may make request r
// now. Callers hold s.mu.
func (s *Server) viewUserLocked(r *http.Request, tok string) (string, bool) {
	vt, ok := s.viewTokens[HashToken(tok)]
	if !ok || !isViewRead(r) || !s.clock().Before(vt.expires) {
		return "", false
	}
	if s.statePath != "" && !s.activeLocked(vt.user) {
		return "", false
	}
	return vt.user, true
}

// viewTokenKey keys the hash of the view token a request carries.
type viewTokenKey struct{}

// viewTokenFrom is the hash of the request's view token, "" without one.
func viewTokenFrom(ctx context.Context) string {
	h, _ := ctx.Value(viewTokenKey{}).(string)
	return h
}

// viewLiveLocked reports whether v may still read: always with a person's
// token, which revoke ends; with a view token, until it expires or is
// evicted. Callers hold s.mu.
func (s *Server) viewLiveLocked(v *viewer) bool {
	if v.token == "" {
		return true
	}
	vt, ok := s.viewTokens[v.token]
	return ok && s.clock().Before(vt.expires)
}

// closeOnce closes ch unless it is closed already. Callers hold s.mu.
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// handleViewToken mints a view token for the request's person, or for
// nobody in particular without a state file.
func (s *Server) handleViewToken(w http.ResponseWriter, r *http.Request) {
	tok, hash, err := newViewToken()
	if err != nil {
		http.Error(w, "could not make a token", http.StatusInternalServerError)
		return
	}
	user := userFrom(r.Context())
	s.mu.Lock()
	if s.revokedLocked(r) {
		s.mu.Unlock()
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	var mine []string
	for h, vt := range s.viewTokens {
		if vt.user == user {
			mine = append(mine, h)
		}
	}
	slices.SortFunc(mine, func(a, b string) int { return s.viewTokens[a].expires.Compare(s.viewTokens[b].expires) })
	for _, h := range mine[:max(0, len(mine)-maxViewTokens+1)] {
		delete(s.viewTokens, h)
		for v := range s.office.viewers {
			if v.token == h {
				closeOnce(v.gone)
			}
		}
	}
	expires := s.clock().Add(viewTokenTTL).UTC()
	s.viewTokens[hash] = viewToken{user: user, expires: expires}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(wire.ViewToken{Token: tok, Expires: expires})
}

// newViewToken is a fresh view token and its stored hash.
func newViewToken() (token, hash string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	token = wire.ViewTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, HashToken(token), nil
}

// handleOffice answers the office as the request's person may see it.
func (s *Server) handleOffice(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	v := s.officeViewLocked(userFrom(r.Context()))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// handleOfficeStream sends the office as an "office" event at once and after
// every change, at most one every officeCoalesce, with a ping when idle. It
// ends when the person is revoked.
func (s *Server) handleOfficeStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	v := &viewer{user: userFrom(r.Context()), token: viewTokenFrom(r.Context()), changed: make(chan struct{}, 1), gone: make(chan struct{})}
	s.mu.Lock()
	if v.user != "" && !s.activeLocked(v.user) {
		s.mu.Unlock()
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	s.office.viewers[v] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.office.viewers, v)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	send := func() bool {
		s.mu.Lock()
		if !s.viewLiveLocked(v) {
			s.mu.Unlock()
			return false
		}
		view := s.officeViewLocked(v.user)
		s.mu.Unlock()
		b, err := json.Marshal(view)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "event: office\ndata: %s\n\n", b)
		flusher.Flush()
		return err == nil
	}
	if !send() {
		return
	}
	ping := time.NewTicker(KeepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-v.gone:
			return
		case <-v.changed:
			select {
			case <-time.After(officeCoalesce):
			case <-r.Context().Done():
				return
			case <-v.gone:
				return
			}
			select {
			case <-v.changed:
			default:
			}
			if !send() {
				return
			}
		case <-ping.C:
			s.mu.Lock()
			live := s.viewLiveLocked(v)
			s.mu.Unlock()
			if !live {
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
