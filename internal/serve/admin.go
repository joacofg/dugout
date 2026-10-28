package serve

import (
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
)

const maxAdminBody = 64 << 10

// validName is a person's name, lowercased first. It goes in URLs and logs.
var validName = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)

// UseState turns on the mode with a state file: a token per person, the
// admin API, the repo allowlist with its members and the team pause, all
// from st, and every admin change saved to path before it answers. The
// shared token of SetToken is not taken then. Call it before serving.
func (s *Server) UseState(path string, st Stored) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statePath, s.stored = path, st
}

// adminRoutes serves the admin API, behind the admin token (see authorize).
// Without a state file it is not there.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	withState := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			ok := s.statePath != ""
			s.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /v1/admin/tokens", withState(s.handleTokenIssue))
	mux.HandleFunc("GET /v1/admin/tokens", withState(s.handleTokenList))
	mux.HandleFunc("DELETE /v1/admin/tokens/{name}", withState(s.handleTokenRevoke))
	mux.HandleFunc("POST /v1/admin/invites", withState(s.handleInviteCreate))
	mux.HandleFunc("POST "+joinPath, withState(s.handleJoin))
	mux.HandleFunc("GET /v1/admin/repos", withState(s.handleRepoList))
	mux.HandleFunc("POST /v1/admin/repos", withState(s.handleRepoAdd))
	mux.HandleFunc("DELETE /v1/admin/repos/{repo...}", withState(s.handleRepoRemove))
	// The query form carries any identity intact: # ? % and a leading / do
	// not survive a URL path.
	mux.HandleFunc("DELETE /v1/admin/repos", withState(s.handleRepoRemove))
	mux.HandleFunc("POST /v1/admin/repos/grant", withState(func(w http.ResponseWriter, r *http.Request) { s.handleMembers(w, r, true) }))
	mux.HandleFunc("POST /v1/admin/repos/ungrant", withState(func(w http.ResponseWriter, r *http.Request) { s.handleMembers(w, r, false) }))
	mux.HandleFunc("POST /v1/admin/pause", withState(s.handlePause))
	mux.HandleFunc("GET /v1/admin/metrics", withState(s.handleMetrics))
}

// refusal is an admin change refused, with its status and its message.
type refusal struct {
	code int
	msg  string
}

// change edits a copy of the stored state, saves it, and only then makes it
// current and runs then, if not nil, under s.mu: a refused edit or a failed
// save changes nothing, in memory or on disk. It answers the failures itself
// and reports whether the change was made.
func (s *Server) change(w http.ResponseWriter, edit func(*Stored) *refusal, then func()) bool {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	s.mu.Lock()
	next := s.stored
	next.Users, next.Repos, next.Owners = slices.Clone(next.Users), slices.Clone(next.Repos), maps.Clone(next.Owners)
	next.Invites = slices.Clone(next.Invites)
	// Each list too, so an edit never writes into the current one.
	next.Members = maps.Clone(next.Members)
	for repo, m := range next.Members {
		next.Members[repo] = slices.Clone(m)
	}
	path, log := s.statePath, s.log
	s.mu.Unlock()
	if r := edit(&next); r != nil {
		http.Error(w, r.msg, r.code)
		return false
	}
	if err := SaveState(path, next); err != nil {
		log.Error("state not saved", "path", path, "err", err)
		http.Error(w, "could not save the state file", http.StatusInternalServerError)
		return false
	}
	s.mu.Lock()
	s.stored = next
	if then != nil {
		then()
	}
	s.mu.Unlock()
	return true
}

// decodeAdmin reads a small JSON body into v, and answers 400 if it cannot.
func decodeAdmin(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody)).Decode(v); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// personName is raw as a person's name, lowercased and trimmed, or the
// refusal of what cannot be one.
func personName(raw string) (string, *refusal) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return "", &refusal{http.StatusBadRequest, "missing name"}
	}
	if !validName.MatchString(name) {
		return "", &refusal{http.StatusBadRequest, "invalid name: use lowercase letters, numbers, dot, hyphen or underscore (up to 32)"}
	}
	return name, nil
}

// handleTokenIssue answers the only response that ever carries a token:
// the token goes to no log and no error.
func (s *Server) handleTokenIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeAdmin(w, r, &req) {
		return
	}
	name, bad := personName(req.Name)
	if bad != nil {
		http.Error(w, bad.msg, bad.code)
		return
	}
	token, hash, err := NewToken()
	if err != nil {
		http.Error(w, "could not generate the token", http.StatusInternalServerError)
		return
	}
	if !s.change(w, func(st *Stored) *refusal {
		if slices.ContainsFunc(st.Users, func(u User) bool { return u.Name == name && !u.Revoked }) {
			return &refusal{http.StatusConflict, "there is already a token for " + name}
		}
		st.Users = append(st.Users, User{Name: name, TokenHash: hash})
		return nil
	}, nil) {
		return
	}
	s.logger().Info("token issued", "name", name)
	writeJSON(w, http.StatusCreated, struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}{name, token})
}

func (s *Server) handleTokenList(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Name    string `json:"name"`
		Revoked bool   `json:"revoked"`
	}
	s.mu.Lock()
	out := make([]entry, 0, len(s.stored.Users))
	for _, u := range s.stored.Users {
		out = append(out, entry{u.Name, u.Revoked})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleTokenRevoke revokes the person's token and cancels their pending
// invite, if any, ends their open streams and
// drops their daemons' state at once, so the others stop getting warnings
// about work nobody reports any more. It frees their daemon ids too, in the
// same save, so none stays stuck on a person who cannot use it: a new token
// for the same name binds them again on their daemons' next request.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PathValue("name"))
	var before, after []core.Warning
	var gone []core.Conflict
	var freed []core.DaemonID
	if !s.change(w, func(st *Stored) *refusal {
		found := false
		for i, u := range st.Users {
			if u.Name == name && !u.Revoked {
				st.Users[i].Revoked, found = true, true
			}
		}
		invited := len(st.Invites)
		st.Invites = slices.DeleteFunc(st.Invites, func(i Invite) bool { return i.Name == name })
		found = found || len(st.Invites) < invited
		if !found {
			return &refusal{http.StatusNotFound, "there is no token for " + name}
		}
		for _, d := range slices.Sorted(maps.Keys(st.Owners)) {
			if st.Owners[d] == name {
				freed = append(freed, d)
				delete(st.Owners, d)
			}
		}
		return nil
	}, func() {
		s.kick(name)
		before, after, gone = s.forgetLocked(freed)
	}) {
		return
	}
	log := s.logger()
	log.Info("token revoked", "name", name)
	logWarningChanges(log, before, after)
	for _, c := range gone {
		log.Info("conflict resolved", "id", c.ID, "path", c.Path)
	}
	w.WriteHeader(http.StatusNoContent)
}

// kick ends the open streams of person name, office streams included, and
// drops their view tokens. Callers hold s.mu.
func (s *Server) kick(name string) {
	for v := range s.office.viewers {
		if v.user == name {
			select {
			case <-v.gone:
			default:
				close(v.gone)
			}
		}
	}
	maps.DeleteFunc(s.viewTokens, func(_ string, vt viewToken) bool { return vt.user == name })
	for _, set := range s.subs {
		for sb := range set {
			if sb.user == name {
				select {
				case <-sb.gone:
				default:
					close(sb.gone)
				}
			}
		}
	}
}

// repoEntry is an allowed repo and its members; none means everyone.
type repoEntry struct {
	Repo    string   `json:"repo"`
	Members []string `json:"members"`
}

func (s *Server) handleRepoList(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]repoEntry, 0, len(s.stored.Repos))
	for _, repo := range s.stored.Repos {
		out = append(out, repoEntry{repo, append([]string{}, s.stored.Members[repo]...)})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleRepoAdd allows a repo. Members, if any, join those it has: adding
// a repo again never opens it to everyone.
func (s *Server) handleRepoAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo    string   `json:"repo"`
		Members []string `json:"members"`
	}
	if !decodeAdmin(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Repo) == "" {
		http.Error(w, "missing repo", http.StatusBadRequest)
		return
	}
	repo := gitscan.NormalizeRemote(req.Repo)
	if repo == "" {
		http.Error(w, "invalid repo", http.StatusBadRequest)
		return
	}
	var names []string
	for _, raw := range req.Members {
		name, bad := personName(raw)
		if bad != nil {
			http.Error(w, bad.msg, bad.code)
			return
		}
		names = append(names, name)
	}
	attrs := []any{"repo", repo}
	if len(names) > 0 {
		attrs = append(attrs, "members", strings.Join(names, ","))
	}
	if s.changeRepos(w, "repo allowed", attrs, func(st *Stored) *refusal {
		if !slices.Contains(st.Repos, repo) {
			st.Repos = append(st.Repos, repo)
			slices.Sort(st.Repos)
		}
		if len(names) > 0 {
			if st.Members == nil {
				st.Members = map[string][]string{}
			}
			m := append(st.Members[repo], names...)
			slices.Sort(m)
			st.Members[repo] = slices.Compact(m)
		}
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleRepoRemove(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("repo")
	if raw == "" {
		raw = r.URL.Query().Get("repo")
	}
	repo := gitscan.NormalizeRemote(raw)
	if repo == "" {
		http.Error(w, "invalid repo", http.StatusBadRequest)
		return
	}
	// Its members go with it: added again, it starts as everyone's.
	if s.changeRepos(w, "repo removed", []any{"repo", repo}, func(st *Stored) *refusal {
		st.Repos = slices.DeleteFunc(st.Repos, func(x string) bool { return x == repo })
		delete(st.Members, repo)
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleMembers grants or ungrants a person a repo and answers its members.
// It refuses to ungrant the last one: a repo without members is everyone's,
// and closing it to one person must not open it to all.
func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request, grant bool) {
	var req struct {
		Repo string `json:"repo"`
		Name string `json:"name"`
	}
	if !decodeAdmin(w, r, &req) {
		return
	}
	repo := gitscan.NormalizeRemote(req.Repo)
	if repo == "" {
		http.Error(w, "invalid repo", http.StatusBadRequest)
		return
	}
	name, bad := personName(req.Name)
	if bad != nil {
		http.Error(w, bad.msg, bad.code)
		return
	}
	msg := "repo member granted"
	if !grant {
		msg = "repo member ungranted"
	}
	var members []string
	if !s.changeRepos(w, msg, []any{"repo", repo, "person", name}, func(st *Stored) *refusal {
		if !slices.Contains(st.Repos, repo) {
			return &refusal{http.StatusNotFound, repo + " is not allowed: add it with dugout admin repo add"}
		}
		m := st.Members[repo]
		i, found := slices.BinarySearch(m, name)
		switch {
		case grant && !found:
			m = slices.Insert(m, i, name)
		case !grant && !found:
			return &refusal{http.StatusNotFound, name + " is not a member of " + repo}
		case !grant && len(m) == 1:
			return &refusal{http.StatusConflict, name + " is the only member of " + repo +
				": with no members everyone would see it. To hide it from everyone, remove the repo"}
		case !grant:
			m = slices.Delete(m, i, i+1)
		}
		if st.Members == nil {
			st.Members = map[string][]string{}
		}
		st.Members[repo], members = m, m
		return nil
	}) {
		return
	}
	writeJSON(w, http.StatusOK, repoEntry{repo, members})
}

// changeRepos saves the allowlist or members edit makes, then re-applies
// every daemon's last full state through them, so the workstreams of a repo
// removed, or of a person no longer in it, leave at once with their warnings,
// bundles and reported conflicts, and those of a repo added, or a person
// granted it, show up without waiting for a resend. The caller answers when
// it reports true.
func (s *Server) changeRepos(w http.ResponseWriter, msg string, attrs []any, edit func(*Stored) *refusal) bool {
	var before, after []core.Warning
	var gone []core.Conflict
	if !s.change(w, edit, func() { before, after, gone = s.refilter() }) {
		return false
	}
	log := s.logger()
	log.Info(msg, attrs...)
	logWarningChanges(log, before, after)
	for _, c := range gone {
		log.Info("conflict resolved", "id", c.ID, "path", c.Path)
	}
	return true
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused *bool `json:"paused"`
	}
	if !decodeAdmin(w, r, &req) {
		return
	}
	if req.Paused == nil {
		http.Error(w, "missing paused", http.StatusBadRequest)
		return
	}
	// A change of the pause reaches every open stream, latest wins; setting
	// the value it already has says nothing.
	changed := false
	if !s.change(w, func(st *Stored) *refusal { changed = st.Paused != *req.Paused; st.Paused = *req.Paused; return nil }, func() {
		if !changed {
			return
		}
		for _, set := range s.subs {
			for sb := range set {
				sb.pushControl(s.controlLocked())
			}
		}
	}) {
		return
	}
	s.logger().Info("team pause", "paused", *req.Paused)
	w.WriteHeader(http.StatusNoContent)
}
