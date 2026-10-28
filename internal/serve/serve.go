// Package serve is the "office" server: it wraps core.Apply with an HTTP
// API, and streams each daemon its current warnings over SSE. Workstreams and
// warnings live in memory only, so a restart loses them until the daemons
// resend their full state. Without a state file the only auth is an
// optional shared token. With one (UseState, admin.go) each person
// has a token, and the admin API keeps the tokens, a repo allowlist and the
// team pause in that file.
package serve

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"mime"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/wire"
)

// KeepAlive is how often an idle stream gets a comment line.
const KeepAlive = wire.StreamKeepAlive

const maxMessage = 4 << 20

// What serve takes of a workstream, whose fields reach the other daemons'
// warnings and, from there, their agents and humans: its intent cut to
// maxIntent runes, as the daemons keep it, its person's name to maxPerson,
// and only with a branch and paths within maxBranch and maxPath bytes.
const (
	maxIntent = 280
	maxPerson = 100
	maxBranch = 256
	maxPath   = 1024
)

// Server holds the core state and the open streams.
type Server struct {
	mu    sync.Mutex
	st    core.State
	subs  map[core.DaemonID]map[*sub]struct{}
	hosts map[string]bool // extra allowed Host names, see AllowHost
	token string          // shared token; empty means none, see SetToken
	log   *slog.Logger

	snaps     map[core.WorkstreamID]storedBundle // see snapshot.go
	snapBytes int
	snapLimit int
	reported  map[core.DaemonID]map[string]core.Conflict
	setClocks map[core.DaemonID]core.Clock // orders each daemon's conflicts messages

	// With a state file, see UseState. Only admin changes and new daemon
	// bindings (claim) write stored, under adminMu as well as mu.
	statePath   string
	stored      Stored
	adminMu     sync.Mutex                       // serializes changes of stored with their saves
	last        map[core.DaemonID]core.FullState // each daemon's last full state taken, before the allowlist
	foreign     map[foreignKey]bool              // foreign workstream ids already warned about
	outsiders   map[outsiderKey]bool             // daemons already warned about a repo for members only
	protoWarn   map[core.DaemonID]bool           // daemons whose protocol mismatch was logged
	minProtocol int                              // the oldest daemon protocol taken; 0 is wire.MinProtocol, tests raise it
	oversized   map[core.DaemonID]bool           // daemons whose oversized workstream was logged
	counts      counters                         // see metrics.go

	// The agent metrics (outcomes.go): the records of conflicts waiting for
	// the other daemon's, the records already taken, the totals by repo, how
	// long to wait and the clock that tells.
	pending     map[string]*pendingOutcome
	outcomeSeen map[outcomeKey]time.Time
	ended       map[string]core.Totals
	outcomeWait time.Duration
	clock       func() time.Time

	// The office view (office.go): the agents' messages, the escalations and
	// the open office streams.
	office officeLog
	// viewTokens are the office view's read-only tokens, by hash (office.go).
	viewTokens map[string]viewToken
}

// foreignKey is a workstream id a daemon sent that is not its own.
type foreignKey struct {
	d  core.DaemonID
	ws core.WorkstreamID
}

// outsiderKey is a repo for members only that a daemon of someone else sent.
type outsiderKey struct {
	d    core.DaemonID
	repo string
}

// sub is one open stream. Its channel holds only the latest warning set.
// ctl is the same for the team pause. esc holds the escalations for its
// daemon not sent yet, up to maxEscalations. gone closes when the token of
// the stream's person is revoked.
type sub struct {
	ch   chan []core.Warning
	ctl  chan wire.Control
	esc  chan wire.Escalation
	user string // "" without a state file
	gone chan struct{}
}

// New returns an empty Server.
func New() *Server {
	return &Server{
		st: core.NewState(), subs: map[core.DaemonID]map[*sub]struct{}{}, log: slog.Default(),
		snaps: map[core.WorkstreamID]storedBundle{}, snapLimit: defaultSnapshotLimit,
		reported: map[core.DaemonID]map[string]core.Conflict{}, setClocks: map[core.DaemonID]core.Clock{},
		last: map[core.DaemonID]core.FullState{}, foreign: map[foreignKey]bool{}, outsiders: map[outsiderKey]bool{}, protoWarn: map[core.DaemonID]bool{},
		oversized: map[core.DaemonID]bool{},
		pending:   map[string]*pendingOutcome{}, outcomeSeen: map[outcomeKey]time.Time{}, ended: map[string]core.Totals{},
		outcomeWait: defaultOutcomeWait, clock: time.Now,
		office:     newOfficeLog(),
		viewTokens: map[string]viewToken{},
	}
}

// SetLogger replaces the logger, slog.Default() by default.
func (s *Server) SetLogger(log *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = log
}

func (s *Server) logger() *slog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log
}

// SetToken makes every request carry "Authorization: Bearer <token>". Required
// when serve listens beyond loopback without a state file. With a state file it
// is not taken.
func (s *Server) SetToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *Server) tokenOK(header string) bool {
	s.mu.Lock()
	token := s.token
	s.mu.Unlock()
	if token == "" {
		return true
	}
	got, ok := strings.CutPrefix(header, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// userKey keys the request's person in its context.
type userKey struct{}

// userFrom is the person whose token the guard took, "" without a state
// file or on /v1/admin.
func userFrom(ctx context.Context) string {
	name, _ := ctx.Value(userKey{}).(string)
	return name
}

// isAdminPath reports whether the mux routes p to the admin API. It cleans p
// as the mux does before routing, so "/v1/admin/../msg" is not admin.
func isAdminPath(p string) bool {
	p = path.Clean(p)
	return p == "/v1/admin" || strings.HasPrefix(p, "/v1/admin/")
}

// authorize checks the request's token. Without a state file that is the
// shared token, if any. With one, /v1/admin takes only the admin token and
// every other route only the token of a person not revoked, whose name it
// puts in the request's context (userFrom). Tokens are compared by their
// hashes, in constant time. A view token (office.go) passes, in either mode,
// only on the office view's two GETs; it is looked up by its hash.
func (s *Server) authorize(r *http.Request) (*http.Request, bool) {
	// A view token reads the office only, as its person, whatever the mode.
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.HasPrefix(tok, wire.ViewTokenPrefix) {
		s.mu.Lock()
		user, ok := s.viewUserLocked(r, tok)
		s.mu.Unlock()
		if !ok {
			return r, false
		}
		ctx := context.WithValue(r.Context(), userKey{}, user)
		return r.WithContext(context.WithValue(ctx, viewTokenKey{}, HashToken(tok))), true
	}
	s.mu.Lock()
	withState := s.statePath != ""
	s.mu.Unlock()
	if !withState {
		return r, s.tokenOK(r.Header.Get("Authorization"))
	}
	// The join route takes no token: the invite code in its body is the
	// credential (invite.go).
	if isJoinPath(r.URL.Path) {
		return r, true
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return r, false
	}
	hash := []byte(HashToken(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	if isAdminPath(r.URL.Path) {
		return r, subtle.ConstantTimeCompare(hash, []byte(s.stored.AdminHash)) == 1
	}
	name := ""
	for _, u := range s.stored.Users {
		if subtle.ConstantTimeCompare(hash, []byte(u.TokenHash)) == 1 && !u.Revoked {
			name = u.Name
		}
	}
	if name == "" {
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), userKey{}, name)), true
}

// activeLocked reports whether person name has a token not revoked. Callers
// hold s.mu.
func (s *Server) activeLocked(name string) bool {
	return slices.ContainsFunc(s.stored.Users, func(u User) bool { return u.Name == name && !u.Revoked })
}

// revokedLocked reports whether the request's person lost their token after
// the guard took it, so a request already past the guard changes nothing.
// Callers hold s.mu.
func (s *Server) revokedLocked(r *http.Request) bool {
	user := userFrom(r.Context())
	return user != "" && !s.activeLocked(user)
}

// afterAuth runs between the guard and the handler. It is a seam for tests
// to hold a request there.
var afterAuth = func(*http.Request) {}

// claim binds daemon d to the request's person the first time a route
// carries it, and answers 403 when d is bound to someone else: a person
// cannot replace another's state, bundles or conflicts, nor read their
// stream. A new binding is in the state file before the request goes on, so
// a restart keeps it; only a daemon's first request pays the write. Without
// a state file there are no persons, and every daemon passes.
func (s *Server) claim(w http.ResponseWriter, r *http.Request, d core.DaemonID) bool {
	user := userFrom(r.Context())
	if user == "" {
		return true
	}
	s.mu.Lock()
	owner, ok := s.stored.Owners[d]
	log := s.log
	s.mu.Unlock()
	if !ok {
		bound := false
		if !s.change(w, func(st *Stored) *refusal {
			// Another request may have bound d, or revoked user, since.
			if owner, ok = st.Owners[d]; ok {
				return nil
			}
			if !slices.ContainsFunc(st.Users, func(u User) bool { return u.Name == user && !u.Revoked }) {
				return &refusal{http.StatusUnauthorized, "missing or wrong token"}
			}
			if st.Owners == nil {
				st.Owners = map[core.DaemonID]string{}
			}
			st.Owners[d], owner, bound = user, user, true
			return nil
		}, nil) {
			return false
		}
		if bound {
			log.Info("daemon bound", "daemon", d, "person", user)
		}
	}
	if owner != user {
		log.Warn("daemon_id of another person", "daemon", d, "person", user, "owner", owner)
		http.Error(w, "daemon_id of another person", http.StatusForbidden)
		return false
	}
	return true
}

// AllowHost accepts Host headers naming host, besides loopback. Use it
// when serve listens on a specific non-loopback address.
func (s *Server) AllowHost(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hosts == nil {
		s.hosts = map[string]bool{}
	}
	s.hosts[normalizeHost(host)] = true
}

// normalizeHost drops a port, IPv6 brackets and a trailing dot, and lowers
// the case, for both -allow-host values and request Host headers.
func normalizeHost(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

func (s *Server) hostAllowed(hostport string) bool {
	host := normalizeHost(hostport)
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hosts[host]
}

// Handler serves POST /v1/msg, GET /v1/stream, GET /v1/state, POST|GET
// /v1/snapshot, the office view (its page under /office, POST
// /v1/office/token, GET /v1/office and its stream) and, with a state file,
// the admin API under /v1/admin, behind guard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/msg", s.handleMsg)
	mux.HandleFunc("GET /v1/stream", s.handleStream)
	mux.HandleFunc("GET /v1/state", s.handleState)
	mux.HandleFunc("POST /v1/snapshot", s.handleSnapshotPost)
	mux.HandleFunc("GET /v1/snapshot", s.handleSnapshotGet)
	mux.HandleFunc("POST /v1/office/token", s.handleViewToken)
	mux.HandleFunc("GET /v1/office", s.handleOffice)
	mux.HandleFunc("GET /v1/office/stream", s.handleOfficeStream)
	mux.Handle("GET /office", webHandler())
	mux.Handle("GET /office/", webHandler())
	s.adminRoutes(mux)
	guarded := s.guard(mux)
	// The header goes on every response, the guard's refusals included.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.HeaderProtocol, strconv.Itoa(wire.ProtocolVersion))
		guarded.ServeHTTP(w, r)
	})
}

// minProto is the oldest daemon protocol this server takes.
func (s *Server) minProto() int {
	if s.minProtocol > 0 {
		return s.minProtocol
	}
	return wire.MinProtocol
}

// noteProtocol logs once per daemon that it speaks another protocol
// version. A message without the field is an older daemon (version 0) and
// says nothing.
func (s *Server) noteProtocol(d core.DaemonID, theirs int) {
	if theirs == 0 || theirs == wire.ProtocolVersion {
		return
	}
	s.mu.Lock()
	seen := s.protoWarn[d]
	s.protoWarn[d] = true
	log := s.log
	s.mu.Unlock()
	if !seen {
		log.Warn("protocol mismatch", "daemon", d, "ours", wire.ProtocolVersion, "theirs", theirs)
	}
}

// guard keeps browsers out. Whatever reaches the core ends up in agents'
// context, and a web page can POST text/plain to localhost without
// a CORS preflight, or read responses through DNS rebinding. The daemon's
// client sends JSON, no Origin, and a loopback or configured Host. Only the
// office page's own files (web.go) take an Origin, one naming serve itself,
// which a browser sends on font loads.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !(isWebPath(r.URL.Path) && sameOrigin(o, r.Host)) {
			http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
			return
		}
		if !s.hostAllowed(r.Host) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		// The page's files carry no data: no token, and the mux answers
		// anything but a GET with 405.
		if isWebPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		r, ok := s.authorize(r)
		if !ok {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		afterAuth(r)
		if r.Method == http.MethodPost {
			want := "application/json"
			if r.URL.Path == "/v1/snapshot" {
				want = wire.BundleType
			}
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != want {
				http.Error(w, "Content-Type must be "+want, http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleMsg(w http.ResponseWriter, r *http.Request) {
	var msg wire.Message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessage)).Decode(&msg); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad message: "+err.Error(), http.StatusBadRequest)
		return
	}
	if v := msg.ProtocolVersion; v >= 1 && v < s.minProto() {
		s.counts.refusedTooOld.Add(1)
		http.Error(w, "your dugout is older than this server accepts: update it", http.StatusUpgradeRequired)
		return
	}
	if msg.Daemon != "" && !s.claim(w, r, msg.Daemon) {
		return
	}
	if msg.Daemon != "" {
		s.noteProtocol(msg.Daemon, msg.ProtocolVersion)
	}
	s.counts.countMessage(msg.Type)
	if msg.Type == wire.TypeConflicts && msg.Daemon != "" {
		if !s.recordConflicts(r, msg.Daemon, msg.Epoch, msg.Seq, msg.Conflicts, msg.Stances) {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if msg.Type == wire.TypeEscalation && msg.Daemon != "" {
		s.handleEscalation(w, r, msg.Daemon, msg.Escalation)
		return
	}
	if msg.Type == wire.TypeAgentMessage && msg.Daemon != "" {
		s.handleAgentMessage(w, r, msg.Daemon, msg.AgentMessage)
		return
	}
	if msg.Type == wire.TypeEscalated && msg.Daemon != "" {
		s.handleEscalated(w, r, msg.Daemon, msg.Escalated)
		return
	}
	if msg.Type == wire.TypeOutcomes && msg.Daemon != "" {
		if !s.recordOutcomes(r, msg.Daemon, msg.Outcomes) {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if msg.Type != wire.TypeFullState {
		s.mu.Lock()
		log := s.log
		s.mu.Unlock()
		log.Info("ignoring message type", "type", msg.Type)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if msg.Daemon == "" {
		http.Error(w, "full_state without daemon_id", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.revokedLocked(r) {
		s.mu.Unlock()
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	before := s.st.Warnings()
	have := s.st.Clock(msg.Daemon).Epoch()
	// Keep what the daemon sent, unfiltered, for refilter. Only a message
	// core will take replaces it: a stale one changes nothing.
	if _, fresh := s.st.Clock(msg.Daemon).Admit(msg.Epoch, msg.Seq); fresh && s.statePath != "" {
		if len(msg.Workstreams) == 0 {
			delete(s.last, msg.Daemon)
		} else {
			s.last[msg.Daemon] = msg.FullState
		}
	}
	fs, dropped := s.visible(msg.FullState)
	effects := s.applyLocked(fs)
	after := s.st.Warnings()
	s.dropOrphanBundles()
	gone := s.dropOrphanConflicts()
	log := s.log
	s.mu.Unlock()
	if warnStaleEpoch(log, msg.Type, msg.Daemon, msg.Epoch, have) {
		s.counts.staleEpochs.Add(1)
	}
	attrs := []any{"daemon", msg.Daemon, "workstreams", len(msg.Workstreams), "effects", len(effects)}
	if len(dropped) > 0 {
		attrs = append(attrs, "outside_allowlist", strings.Join(dropped, ","))
	}
	log.Info("full_state", attrs...)
	logWarningChanges(log, before, after)
	for _, c := range gone {
		log.Info("conflict resolved", "id", c.ID, "path", c.Path)
	}
	w.WriteHeader(http.StatusNoContent)
}

// allowedLocked reports whether repo is on the allowlist; without a state
// file every repo is. The allowlist holds normalized identities, so repo is
// normalized first: a daemon may send a remote URL, capitals or ".git".
// Callers hold s.mu.
func (s *Server) allowedLocked(repo string) bool {
	return s.statePath == "" || slices.Contains(s.stored.Repos, gitscan.NormalizeRemote(repo))
}

// seesLocked reports whether person sees repo: a repo without members is
// everyone's, one with them only its members'. Without a state file there are
// no persons, and everyone sees every repo. Callers hold s.mu.
func (s *Server) seesLocked(person, repo string) bool {
	if s.statePath == "" {
		return true
	}
	m := s.stored.Members[gitscan.NormalizeRemote(repo)]
	return len(m) == 0 || slices.Contains(m, person)
}

// visible drops from fs the workstreams of repos outside the allowlist, so
// they make no warning, take no bundle and keep no reported conflict, and
// those of repos for members only when the daemon's person is not one, with
// a warning once per daemon and repo: a non-member's work would pair with the
// members' and get their bundles. It also
// drops the workstreams whose id is not the daemon's own ("<daemon_id>:..."),
// so Owner never names a daemon that did not make the id and nobody uploads
// a bundle for another person's workstream; it warns once per daemon and id.
// Then, with a state file or not, it bounds what is left (bound). It returns
// the repos it dropped, sorted. Callers hold s.mu.
func (s *Server) visible(fs core.FullState) (core.FullState, []string) {
	if s.statePath == "" {
		return s.bound(fs), nil
	}
	kept := make([]core.Workstream, 0, len(fs.Workstreams))
	var dropped []string
	person := s.stored.Owners[fs.Daemon]
	for _, w := range fs.Workstreams {
		repo := gitscan.NormalizeRemote(w.Repo)
		switch {
		case !strings.HasPrefix(string(w.ID), string(fs.Daemon)+":"):
			if k := (foreignKey{fs.Daemon, w.ID}); !s.foreign[k] {
				s.foreign[k] = true
				s.log.Warn("workstream id of another daemon", "daemon", fs.Daemon, "ws", w.ID)
			}
		case !s.allowedLocked(repo):
			if !slices.Contains(dropped, repo) {
				dropped = append(dropped, repo)
			}
		case !s.seesLocked(person, repo):
			if k := (outsiderKey{fs.Daemon, repo}); !s.outsiders[k] {
				s.outsiders[k] = true
				s.log.Warn("workstream in a repo for members only", "daemon", fs.Daemon, "person", person, "repo", repo)
			}
		default:
			// Canonical, so a daemon that sends another spelling of the repo
			// still pairs with the ones that send the canonical one.
			w.Repo = repo
			kept = append(kept, w)
		}
	}
	slices.Sort(dropped)
	fs.Workstreams = kept
	return s.bound(fs), dropped
}

// bound cuts each workstream's intent to maxIntent runes and its person's
// name to maxPerson, and drops the
// workstreams with a branch or a path past maxBranch or maxPath bytes, the
// daemon's others staying; it warns once per daemon. It never sanitizes: the
// daemons that render the fields do. Callers hold s.mu.
func (s *Server) bound(fs core.FullState) core.FullState {
	kept := make([]core.Workstream, 0, len(fs.Workstreams))
	for _, w := range fs.Workstreams {
		if len(w.Branch) > maxBranch || slices.ContainsFunc(w.Files, func(p string) bool { return len(p) > maxPath }) {
			if !s.oversized[fs.Daemon] {
				s.oversized[fs.Daemon] = true
				s.log.Warn("dropping workstream with oversized fields", "daemon", fs.Daemon, "workstream", w.ID)
			}
			continue
		}
		w.Intent = firstRunes(w.Intent, maxIntent)
		w.Person = firstRunes(w.Person, maxPerson)
		kept = append(kept, w)
	}
	fs.Workstreams = kept
	return fs
}

// firstRunes is s's first n runes, or s when it has fewer: valid UTF-8 when s
// is. A cut is a copy, so it does not keep a long s in memory.
func firstRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return strings.Clone(s[:i])
		}
		n--
	}
	return s
}

// applyLocked applies fs and pushes each changed warning set to the streams
// of its daemon. Callers hold s.mu.
func (s *Server) applyLocked(fs core.FullState) []core.Effect {
	var effects []core.Effect
	s.st, effects = core.Apply(s.st, fs)
	s.officeChangedLocked()
	for _, e := range effects {
		if wc, ok := e.(core.WarningsChanged); ok {
			for sb := range s.subs[wc.Daemon] {
				sb.push(wc.Warnings)
			}
		}
	}
	return effects
}

// refilter re-applies each daemon's last full state through the current
// allowlist and members, so a repo, or a person's work in it, leaves the state, or joins it, at once and not at the
// daemon's next message. It applies them with epoch 0, which core always
// takes and which leaves the daemon's clock alone: the stamp they came with
// was taken already, and the daemon's next message is ordered against it as
// before. It returns the warnings before and after, and the reported
// conflicts gone with the workstreams that left. Callers hold s.mu.
func (s *Server) refilter() (before, after []core.Warning, gone []core.Conflict) {
	before = s.st.Warnings()
	for _, d := range slices.Sorted(maps.Keys(s.last)) {
		fs, _ := s.visible(s.last[d])
		fs.Epoch, fs.Seq = 0, 0
		s.applyLocked(fs)
	}
	s.officeChangedLocked() // members may have changed what a person sees
	s.dropOrphanBundles()
	return before, s.st.Warnings(), s.dropOrphanConflicts()
}

// forgetLocked drops the state of daemons ds, as if each had sent an empty
// full state: their workstreams leave with the warnings, bundles and
// reported conflicts they hold, and the streams of the others get the
// change. It goes with epoch 0, like refilter, so a daemon's clock stays
// where it was. It returns the warnings before and after, and the conflicts
// no report holds any more. Callers hold s.mu.
func (s *Server) forgetLocked(ds []core.DaemonID) (before, after []core.Warning, gone []core.Conflict) {
	before = s.st.Warnings()
	was := s.conflictUnion()
	for _, d := range ds {
		s.applyLocked(core.FullState{Daemon: d})
		delete(s.last, d)
		delete(s.reported, d)
	}
	s.dropOrphanBundles()
	s.dropOrphanConflicts()
	is := s.conflictReporters()
	for _, c := range was {
		if _, ok := is[c.ID]; !ok {
			gone = append(gone, c)
		}
	}
	return before, s.st.Warnings(), gone
}

// warnStaleEpoch warns when a message's epoch is below the last one serve
// took from its daemon: a late message of an earlier run, or a run that went
// back, which serve ignores until the daemon's epoch passes have. A
// duplicate of the current epoch is the network's doing and stays quiet. It
// reports whether the message was stale.
func warnStaleEpoch(log *slog.Logger, typ string, d core.DaemonID, got, have uint64) bool {
	if got != 0 && got < have {
		log.Warn("stale epoch", "type", typ, "daemon", d, "got", got, "have", have)
		return true
	}
	return false
}

// logWarningChanges logs each warning when it first appears and when it
// goes away. With the daemons' "warning delivered" logs, these time the
// propagation of a warning across laptops.
func logWarningChanges(log *slog.Logger, before, after []core.Warning) {
	ids := func(ws []core.Warning) map[string]bool {
		m := make(map[string]bool, len(ws))
		for _, w := range ws {
			m[w.ID] = true
		}
		return m
	}
	was, is := ids(before), ids(after)
	for _, w := range after {
		if !was[w.ID] {
			log.Info("warning created", "id", w.ID, "repo", w.Repo, "path", w.Path,
				"daemons", string(w.Sides[0].Daemon)+","+string(w.Sides[1].Daemon))
		}
	}
	for _, w := range before {
		if !is[w.ID] {
			log.Info("warning resolved", "id", w.ID, "repo", w.Repo, "path", w.Path)
		}
	}
}

// push replaces whatever the stream has not sent yet. Callers hold s.mu.
func (sb *sub) push(ws []core.Warning) {
	select {
	case <-sb.ch:
	default:
	}
	sb.ch <- ws
}

// pushControl is push for the control event. Callers hold s.mu.
func (sb *sub) pushControl(c wire.Control) {
	select {
	case <-sb.ctl:
	default:
	}
	sb.ctl <- c
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	d := core.DaemonID(r.URL.Query().Get("daemon_id"))
	if d == "" {
		http.Error(w, "daemon_id required", http.StatusBadRequest)
		return
	}
	if !s.claim(w, r, d) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sb := &sub{ch: make(chan []core.Warning, 1), ctl: make(chan wire.Control, 1), esc: make(chan wire.Escalation, maxEscalations), user: userFrom(r.Context()), gone: make(chan struct{})}
	s.mu.Lock()
	// A revoke between the guard and here would not have seen this stream.
	if sb.user != "" && !s.activeLocked(sb.user) {
		s.mu.Unlock()
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	if s.subs[d] == nil {
		s.subs[d] = map[*sub]struct{}{}
	}
	s.subs[d][sb] = struct{}{}
	sb.push(s.st.WarningsFor(d))
	// Taken with the registration, so a later change reaches sb.ctl and
	// follows this one.
	first := s.controlLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs[d], sb)
		if len(s.subs[d]) == 0 {
			delete(s.subs, d)
		}
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// The control goes before any warnings: a daemon that reconnects learns
	// the pause before its first hook can hear a warning.
	if !writeControl(w, first) {
		return
	}
	flusher.Flush()
	ping := time.NewTicker(KeepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sb.gone:
			return
		case c := <-sb.ctl:
			if !writeControl(w, c) {
				return
			}
		case e := <-sb.esc:
			b, err := json.Marshal(e)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: escalation\ndata: %s\n\n", b); err != nil {
				return
			}
		case ws := <-sb.ch:
			b, err := json.Marshal(ws)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: warnings\ndata: %s\n\n", b); err != nil {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

// controlLocked is the control event as it stands: the pause, or false
// without a state file. Callers hold s.mu.
func (s *Server) controlLocked() wire.Control {
	return wire.Control{InjectionOff: s.stored.Paused}
}

func writeControl(w http.ResponseWriter, c wire.Control) bool {
	b, err := json.Marshal(c)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "event: control\ndata: %s\n\n", b)
	return err == nil
}

// handleState dumps the state, but for the repos for members only that the
// request's person is not a member of.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	s.mu.Lock()
	d := wire.StateDump{Workstreams: s.st.Workstreams(), Warnings: s.st.Warnings(), Conflicts: s.conflictUnion()}
	d.Workstreams = slices.DeleteFunc(d.Workstreams, func(w core.Workstream) bool { return !s.seesLocked(user, w.Repo) })
	d.Warnings = slices.DeleteFunc(d.Warnings, func(w core.Warning) bool { return !s.seesLocked(user, w.Repo) })
	d.Conflicts = slices.DeleteFunc(d.Conflicts, func(c core.Conflict) bool { return !s.seesLocked(user, c.Repo) })
	s.mu.Unlock()
	if d.Workstreams == nil {
		d.Workstreams = []core.Workstream{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}
