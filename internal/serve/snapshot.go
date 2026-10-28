package serve

import (
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

const defaultSnapshotLimit = 256 << 20

// hexID matches a git object name, SHA-1 or SHA-256, lowercase.
var hexID = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// storedBundle is the last bundle a daemon uploaded for one of its workstreams.
type storedBundle struct {
	daemon core.DaemonID
	commit string
	base   string
	data   []byte
}

// SetSnapshotLimit caps the bytes of all stored bundles together, 256 MB by
// default. It exists for tests.
func (s *Server) SetSnapshotLimit(total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapLimit = total
}

func (s *Server) handleSnapshotPost(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ws, d := core.WorkstreamID(q.Get("ws")), core.DaemonID(q.Get("daemon_id"))
	commit, base := r.Header.Get(wire.HeaderCommit), r.Header.Get(wire.HeaderBase)
	if ws == "" || d == "" || !hexID.MatchString(commit) || !hexID.MatchString(base) {
		http.Error(w, "ws, daemon_id and the commit and base headers are required", http.StatusBadRequest)
		return
	}
	if !s.claim(w, r, d) {
		return
	}
	// A Content-Length over a limit is refused before a byte of the body is
	// read. The checks after reading stay: a body may come without one, or
	// with a wrong one.
	if r.ContentLength > wire.MaxBundle {
		http.Error(w, "bundle too large", http.StatusRequestEntityTooLarge)
		return
	}
	s.mu.Lock()
	revoked := s.revokedLocked(r)
	owner, ok := s.st.Owner(ws)
	full := r.ContentLength >= 0 && s.snapBytes+int(r.ContentLength)-len(s.snaps[ws].data) > s.snapLimit
	s.mu.Unlock()
	if revoked {
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	if !ok || owner != d {
		http.Error(w, "workstream does not belong to daemon", http.StatusConflict)
		return
	}
	if full {
		http.Error(w, "snapshot storage full", http.StatusInsufficientStorage)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxBundle))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "bundle too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	// The person may have been revoked, or the workstream may have gone,
	// while the body was uploading.
	if s.revokedLocked(r) {
		s.mu.Unlock()
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	if owner, ok := s.st.Owner(ws); !ok || owner != d {
		s.mu.Unlock()
		http.Error(w, "workstream does not belong to daemon", http.StatusConflict)
		return
	}
	if s.snapBytes+len(data)-len(s.snaps[ws].data) > s.snapLimit {
		s.mu.Unlock()
		http.Error(w, "snapshot storage full", http.StatusInsufficientStorage)
		return
	}
	s.snapBytes += len(data) - len(s.snaps[ws].data)
	s.snaps[ws] = storedBundle{daemon: d, commit: commit, base: base, data: data}
	log := s.log
	s.mu.Unlock()
	log.Info("snapshot stored", "ws", ws, "daemon", d, "commit", commit[:12], "bytes", len(data))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ws, d := core.WorkstreamID(q.Get("ws")), core.DaemonID(q.Get("daemon_id"))
	if ws == "" || d == "" {
		http.Error(w, "ws and daemon_id required", http.StatusBadRequest)
		return
	}
	if !s.claim(w, r, d) {
		return
	}
	user := userFrom(r.Context())
	s.mu.Lock()
	overlaps := false
	for _, wn := range s.st.WarningsFor(d) {
		a, b := wn.Sides[0], wn.Sides[1]
		// visible keeps a non-member out of such a warning; this holds even
		// if some other path ever pairs one.
		if !s.seesLocked(user, wn.Repo) {
			continue
		}
		if (a.Workstream == ws && b.Daemon == d) || (b.Workstream == ws && a.Daemon == d) {
			overlaps = true
			break
		}
	}
	b, have := s.snaps[ws]
	s.mu.Unlock()
	if !overlaps || !have {
		http.NotFound(w, r)
		return
	}
	etag := `"` + b.commit + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", wire.BundleType)
	w.Header().Set(wire.HeaderCommit, b.commit)
	w.Header().Set(wire.HeaderBase, b.base)
	w.Write(b.data)
	s.counts.bundlesRelayed.Add(1)
}

// dropOrphanBundles forgets the bundles of workstreams no daemon reports any
// more. Callers hold s.mu.
func (s *Server) dropOrphanBundles() {
	for ws, b := range s.snaps {
		if _, ok := s.st.Owner(ws); !ok {
			s.snapBytes -= len(b.data)
			delete(s.snaps, ws)
		}
	}
}

// dropOrphanConflicts forgets the reported conflicts with a side whose
// workstream no daemon reports any more, whoever reported them: a conflict
// cannot outlive its workstreams, even when the daemon that reported it never
// reports again. It returns the conflicts no report holds any more, sorted by
// ID. Callers hold s.mu.
func (s *Server) dropOrphanConflicts() []core.Conflict {
	before := s.conflictUnion()
	for d, m := range s.reported {
		maps.DeleteFunc(m, func(_ string, c core.Conflict) bool {
			_, okA := s.st.Owner(c.Sides[0].Workstream)
			_, okB := s.st.Owner(c.Sides[1].Workstream)
			return !okA || !okB
		})
		if len(m) == 0 {
			delete(s.reported, d)
		}
	}
	after := s.conflictReporters()
	var gone []core.Conflict
	for _, c := range before {
		if _, is := after[c.ID]; !is {
			gone = append(gone, c)
		}
	}
	return gone
}

// recordConflicts replaces what daemon d reports and logs how the union of
// all reports changed: each conflict when it first appears and when the last
// daemon stops reporting it. With the warning logs, these measure detection.
// A set stamped older than the last one taken from d changes nothing.
// Conflicts of repos outside the allowlist or for members only that r's
// person is not one of, and those where neither side is d's, are not kept. It returns false,
// changing nothing, when r's person was revoked after the guard.
func (s *Server) recordConflicts(r *http.Request, d core.DaemonID, epoch, seq uint64, cs []core.Conflict, stances map[string][2]core.Stance) bool {
	s.mu.Lock()
	if s.revokedLocked(r) {
		s.mu.Unlock()
		return false
	}
	clock, ok := s.setClocks[d].Admit(epoch, seq)
	if !ok {
		log := s.log
		s.mu.Unlock()
		if warnStaleEpoch(log, wire.TypeConflicts, d, epoch, clock.Epoch()) {
			s.counts.staleEpochs.Add(1)
		}
		return true
	}
	s.setClocks[d] = clock
	before := map[string]core.Conflict{}
	for _, c := range s.conflictUnion() {
		before[c.ID] = c
	}
	mine := make(map[string]core.Conflict, len(cs))
	for _, c := range cs {
		// A daemon reports the conflicts it is part of, nobody else's.
		if s.allowedLocked(c.Repo) && s.seesLocked(userFrom(r.Context()), c.Repo) && (c.Sides[0].Daemon == d || c.Sides[1].Daemon == d) {
			// Only a daemon of both sides holds stances (see the office view).
			if c.Stances = [2]core.Stance{}; c.Sides[0].Daemon == d && c.Sides[1].Daemon == d {
				c.Stances = stances[c.ID]
			}
			mine[c.ID] = c
		}
	}
	if len(mine) == 0 {
		delete(s.reported, d)
	} else {
		s.reported[d] = mine
	}
	s.endEscalationsLocked()
	s.officeChangedLocked()
	after := s.conflictReporters()
	union := s.conflictUnion()
	log := s.log
	s.mu.Unlock()

	for _, c := range union {
		if _, was := before[c.ID]; !was {
			log.Info("conflict detected", "id", c.ID, "repo", c.Repo, "path", c.Path,
				"daemons", strings.Join(after[c.ID], ","))
		}
	}
	for _, id := range sortedIDs(before) {
		if _, is := after[id]; !is {
			log.Info("conflict resolved", "id", id, "path", before[id].Path)
		}
	}
	return true
}

// conflictReporters maps each reported conflict ID to the sorted daemons
// reporting it. Callers hold s.mu.
func (s *Server) conflictReporters() map[string][]string {
	out := map[string][]string{}
	for d, m := range s.reported {
		for id := range m {
			out[id] = append(out[id], string(d))
		}
	}
	for _, ds := range out {
		slices.Sort(ds)
	}
	return out
}

// conflictUnion is every reported conflict once, sorted by ID, never nil.
// Callers hold s.mu.
func (s *Server) conflictUnion() []core.Conflict {
	seen := map[string]core.Conflict{}
	for _, m := range s.reported {
		for id, c := range m {
			seen[id] = c
		}
	}
	out := make([]core.Conflict, 0, len(seen))
	for _, id := range sortedIDs(seen) {
		out = append(out, seen[id])
	}
	return out
}

func sortedIDs[V any](m map[string]V) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
