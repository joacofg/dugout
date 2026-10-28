package serve

import (
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
)

// Agent metrics: each daemon sends the record of every conflict
// that ended on it (an "outcomes" message), counting only what happened on
// its own machine. Serve merges the records of one conflict (core.Merge) once
// every daemon of its sides sent one, or outcomeWait after the first, and adds
// the conflict up in its repo's Totals, which GET /v1/admin/metrics serves.
// Like the other counters they live in memory only.

const (
	// defaultOutcomeWait is how long serve waits for the other daemon's
	// record of a conflict between two machines: past both daemons' grace
	// and a few resends.
	defaultOutcomeWait = 5 * time.Minute
	// maxOutcomesPerMessage and maxPendingOutcomes bound what one daemon,
	// and all of them, make serve hold.
	maxOutcomesPerMessage = 1000
	maxPendingOutcomes    = 10000
	// outcomeSeenFor is how long a record already taken is remembered, so a
	// daemon's resend of one is not counted twice.
	outcomeSeenFor = time.Hour
)

// outcomeKey names one daemon's record of one conflict that ended.
type outcomeKey struct {
	d      core.DaemonID
	id     string
	closed core.Millis
}

// pendingOutcome is the records of one conflict that came so far, by daemon.
type pendingOutcome struct {
	recs  map[core.DaemonID]core.Record
	first time.Time
}

// complete reports whether every daemon of the conflict's sides sent its
// record.
func (p *pendingOutcome) complete() bool {
	for _, r := range p.recs {
		for _, d := range r.Daemons {
			if _, ok := p.recs[d]; !ok {
				return false
			}
		}
		return true
	}
	return false
}

// recordOutcomes takes the records daemon d sent: only of conflicts of a
// repo d's person sees and that d is a side of, each once. It reports false
// when the token was revoked.
func (s *Server) recordOutcomes(r *http.Request, d core.DaemonID, recs []core.Record) bool {
	s.mu.Lock()
	if s.revokedLocked(r) {
		s.mu.Unlock()
		return false
	}
	now := s.clock()
	ended := s.expireOutcomesLocked(now)
	user := userFrom(r.Context())
	for _, rec := range recs[:min(len(recs), maxOutcomesPerMessage)] {
		rec.Repo = gitscan.NormalizeRemote(rec.Repo)
		if rec.ID == "" || len(rec.ID) > maxPath || len(rec.Repo) > maxPath || (rec.Daemons[0] != d && rec.Daemons[1] != d) ||
			!s.allowedLocked(rec.Repo) || !s.seesLocked(user, rec.Repo) || !sane(rec.Tally) {
			continue
		}
		k := outcomeKey{d, rec.ID, rec.Closed}
		if _, dup := s.outcomeSeen[k]; dup {
			continue
		}
		s.outcomeSeen[k] = now
		p := s.pending[rec.ID]
		if p != nil && p.recs[d].ID != "" {
			// The same conflict ended again on d before the other side's
			// record of the first time came: the first one is complete as is.
			ended = append(ended, s.endOutcomeLocked(rec.ID))
			p = nil
		}
		if p == nil {
			if len(s.pending) >= maxPendingOutcomes {
				ended = append(ended, s.endOutcomeLocked(s.oldestPendingLocked()))
			}
			p = &pendingOutcome{recs: map[core.DaemonID]core.Record{}, first: now}
			s.pending[rec.ID] = p
		}
		p.recs[d] = rec
		if p.complete() {
			ended = append(ended, s.endOutcomeLocked(rec.ID))
		}
	}
	log := s.log
	s.mu.Unlock()
	logEnded(log, ended)
	return true
}

// sane reports whether t has no negative count.
func sane(t core.Tally) bool {
	return t.Messages >= 0 && t.Held >= 0 && t.Changed >= 0 && t.Unchanged >= 0 && t.RuleEscalations >= 0 && t.AgentEscalations >= 0 && t.Relayed >= 0
}

// expireOutcomesLocked ends the conflicts whose other record did not come
// within outcomeWait and forgets the records seen past outcomeSeenFor. It
// returns the conflicts it ended. Callers hold s.mu.
func (s *Server) expireOutcomesLocked(now time.Time) []core.Record {
	var ended []core.Record
	for _, id := range slices.Sorted(maps.Keys(s.pending)) {
		if now.Sub(s.pending[id].first) >= s.outcomeWait {
			ended = append(ended, s.endOutcomeLocked(id))
		}
	}
	maps.DeleteFunc(s.outcomeSeen, func(_ outcomeKey, at time.Time) bool { return now.Sub(at) >= outcomeSeenFor })
	return ended
}

// endOutcomeLocked merges the records of conflict id that came, adds the
// conflict up in its repo and returns it. A side whose daemon sent no record
// and has no stream open went away with its machine: a conflict it did not
// commit counts as gone, not resolved. Callers hold s.mu.
func (s *Server) endOutcomeLocked(id string) core.Record {
	p := s.pending[id]
	delete(s.pending, id)
	var recs []core.Record
	for _, d := range slices.Sorted(maps.Keys(p.recs)) {
		recs = append(recs, p.recs[d])
	}
	m := core.Merge(recs)
	for _, d := range m.Daemons {
		if _, sent := p.recs[d]; !sent && len(s.subs[d]) == 0 && m.Outcome == core.OutcomeResolved {
			m.Outcome = core.OutcomeGone
		}
	}
	t := s.ended[m.Repo]
	t.Add(m)
	s.ended[m.Repo] = t
	return m
}

// oldestPendingLocked is the conflict that has waited longest. Callers hold
// s.mu, with s.pending not empty.
func (s *Server) oldestPendingLocked() string {
	oldest := ""
	for id, p := range s.pending {
		if oldest == "" || p.first.Before(s.pending[oldest].first) || (p.first.Equal(s.pending[oldest].first) && id < oldest) {
			oldest = id
		}
	}
	return oldest
}

// endedTotals ends what waited too long and returns the totals by repo.
func (s *Server) endedTotals() map[string]core.Totals {
	s.mu.Lock()
	ended := s.expireOutcomesLocked(s.clock())
	out := maps.Clone(s.ended)
	log := s.log
	s.mu.Unlock()
	logEnded(log, ended)
	return out
}

// logEnded logs each conflict serve added up: counts and identities, no path.
func logEnded(log *slog.Logger, ended []core.Record) {
	for _, m := range ended {
		log.Info("conflict ended", "id", m.ID, "repo", m.Repo, "outcome", m.Outcome, "daemons", string(m.Daemons[0])+","+string(m.Daemons[1]),
			"messages", m.Messages, "escalations", m.Escalations(), "held", m.Held, "changed", m.Changed, "unchanged", m.Unchanged)
	}
}

// labelValue escapes v for a Prometheus label value.
func labelValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}
