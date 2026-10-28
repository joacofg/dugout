package serve

import (
	"encoding/json"
	"net/http"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// maxEscalations is how many escalations one stream holds unsent; past it,
// the newest go undelivered and their sender is told so.
const maxEscalations = 8

// handleEscalation hands daemon d's escalation to the streams of the other
// side's daemon and answers whether one took it. It takes one only from the
// daemon that reported the asking workstream, to a workstream of another
// daemon, on a path serve holds a warning for between the two: so a daemon
// can reach only a human whose work already overlaps its own. The sides go on
// as serve's state has them, the reason cut to maxIntent runes.
func (s *Server) handleEscalation(w http.ResponseWriter, r *http.Request, d core.DaemonID, e *wire.Escalation) {
	if e == nil {
		http.Error(w, "escalation without escalation", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.revokedLocked(r) {
		s.mu.Unlock()
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	from, to, ok := s.pairLocked(d, *e)
	if !ok {
		s.mu.Unlock()
		http.Error(w, "no overlap between those workstreams on that path", http.StatusUnprocessableEntity)
		return
	}
	out := wire.Escalation{Conflict: firstRunes(e.Conflict, 64), Repo: e.Repo, Path: e.Path, From: from, To: to,
		Reason: firstRunes(e.Reason, maxIntent), Urgent: e.Urgent}
	delivered := false
	for sb := range s.subs[to.Daemon] {
		select {
		case sb.esc <- out:
			delivered = true
		default:
		}
	}
	log := s.log
	s.mu.Unlock()
	log.Info("escalation", "from", d, "to", to.Daemon, "path", e.Path, "delivered", delivered)
	b, _ := json.Marshal(wire.Relayed{Delivered: delivered})
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// pairLocked finds the warning on e's repo and path between e's two
// workstreams, From reported by d and To by another daemon, and returns its
// sides in that order. Callers hold s.mu.
func (s *Server) pairLocked(d core.DaemonID, e wire.Escalation) (from, to core.Side, ok bool) {
	if owner, found := s.st.Owner(e.From.Workstream); !found || owner != d || e.From.Daemon != d {
		return from, to, false
	}
	for _, w := range s.st.Warnings() {
		if w.Repo != e.Repo || w.Path != e.Path {
			continue
		}
		for i, side := range w.Sides {
			other := w.Sides[1-i]
			if side.Workstream == e.From.Workstream && other.Workstream == e.To.Workstream && other.Daemon != d {
				return side, other, true
			}
		}
	}
	return from, to, false
}
