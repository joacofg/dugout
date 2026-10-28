package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	adminTok = "dgt_admin"
	anaTok   = "dgt_ana"
	betoTok  = "dgt_beto"
	carlaTok = "dgt_carla" // revoked
)

const (
	repoApp = "github.com/acme/app"
	repoAPI = "github.com/acme/api"
)

// stateServer runs serve with a state file: an admin, ana and beto, carla
// revoked, and repos allowed. It returns the file's path too.
func stateServer(t *testing.T, repos ...string) (*Server, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "serve.json")
	st := Stored{Version: 1, AdminHash: HashToken(adminTok), Repos: repos, Users: []User{
		{Name: "ana", TokenHash: HashToken(anaTok)},
		{Name: "beto", TokenHash: HashToken(betoTok)},
		{Name: "carla", TokenHash: HashToken(carlaTok), Revoked: true},
	}}
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.UseState(path, st)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return s, srv, path
}

// client makes requests to srv with token tok; an empty tok sends none.
type client struct {
	t   *testing.T
	srv *httptest.Server
	tok string
}

func as(t *testing.T, srv *httptest.Server, tok string) client { return client{t, srv, tok} }

// do sends a request with the given extra header pairs and returns the
// status and body.
func (c client) do(method, path, ctype string, body []byte, hdr ...string) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c client) msg(m wire.Message) int {
	c.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		c.t.Fatal(err)
	}
	code, _ := c.do(http.MethodPost, "/v1/msg", "application/json", b)
	return code
}

func (c client) fullState(d core.DaemonID, epoch, seq uint64, ws ...core.Workstream) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeFullState, FullState: core.FullState{Daemon: d, Epoch: epoch, Seq: seq, Workstreams: ws}})
}

func (c client) conflicts(d core.DaemonID, cs ...core.Conflict) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeConflicts, FullState: core.FullState{Daemon: d}, Conflicts: cs})
}

func (c client) state() wire.StateDump {
	c.t.Helper()
	code, b := c.do(http.MethodGet, "/v1/state", "", nil)
	if code != http.StatusOK {
		c.t.Fatalf("GET /v1/state: status %d %s", code, b)
	}
	var d wire.StateDump
	if err := json.Unmarshal(b, &d); err != nil {
		c.t.Fatal(err)
	}
	return d
}

func (c client) upload(ws, d string, body []byte) int {
	c.t.Helper()
	code, _ := c.do(http.MethodPost, "/v1/snapshot?ws="+ws+"&daemon_id="+d, wire.BundleType, body,
		wire.HeaderCommit, commitC, wire.HeaderBase, baseB)
	return code
}

func (c client) download(ws, d string) (int, []byte) {
	c.t.Helper()
	return c.do(http.MethodGet, "/v1/snapshot?ws="+ws+"&daemon_id="+d, "", nil)
}

// stream subscribes as d. With a 200 the channel gets each warning set and
// closes when the stream ends.
func (c client) stream(d string) (int, <-chan []core.Warning) {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.srv.URL+"/v1/stream?daemon_id="+d, nil)
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	out := make(chan []core.Warning, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "warnings":
				var ws []core.Warning
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ws); err == nil {
					out <- ws
				}
			}
		}
	}()
	return resp.StatusCode, out
}

func wr(id, repo, file string) core.Workstream {
	return core.Workstream{ID: core.WorkstreamID(id), Repo: repo, Branch: "b-" + id, Files: []string{file}}
}

func wsIDs(ws []core.Workstream) string {
	var ids []string
	for _, w := range ws {
		ids = append(ids, string(w.ID))
	}
	return strings.Join(ids, ",")
}

// TestPerUserToken: with a state file every route but /v1/admin takes the
// token of a person who is not revoked, and the guard hands the handlers who
// that person is.
func TestPerUserToken(t *testing.T) {
	s, srv, _ := stateServer(t, "r")
	for _, tok := range []string{anaTok, betoTok} {
		c := as(t, srv, tok)
		if code := c.fullState(core.DaemonID("d-"+tok), 0, 0, w("d-"+tok+":ws", "x.go")); code != http.StatusNoContent {
			t.Errorf("POST /v1/msg with %s: status %d", tok, code)
		}
		if code, _ := c.do(http.MethodGet, "/v1/state", "", nil); code != http.StatusOK {
			t.Errorf("GET /v1/state with %s: status %d", tok, code)
		}
		if code, _ := c.stream("d-" + tok); code != http.StatusOK {
			t.Errorf("GET /v1/stream with %s: status %d", tok, code)
		}
		if code, _ := c.download("ws-x", "d-"+tok); code != http.StatusNotFound {
			t.Errorf("GET /v1/snapshot with %s: status %d, want 404", tok, code)
		}
	}
	for _, tok := range []string{"", "dgt_nope", adminTok} {
		c := as(t, srv, tok)
		if code := c.fullState("d1", 0, 0); code != http.StatusUnauthorized {
			t.Errorf("POST /v1/msg with %q: status %d, want 401", tok, code)
		}
		if code, _ := c.do(http.MethodGet, "/v1/state", "", nil); code != http.StatusUnauthorized {
			t.Errorf("GET /v1/state with %q: status %d, want 401", tok, code)
		}
		if code, _ := c.stream("d1"); code != http.StatusUnauthorized {
			t.Errorf("GET /v1/stream with %q: status %d, want 401", tok, code)
		}
	}
	// The scheme is required, as with the shared token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/state", nil)
	req.Header.Set("Authorization", anaTok)
	if code := do(t, req); code != http.StatusUnauthorized {
		t.Errorf("token without Bearer: status %d, want 401", code)
	}

	var got string
	h := s.guard(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = userFrom(r.Context()) }))
	req = httptest.NewRequest(http.MethodGet, "http://localhost/v1/state", nil)
	req.Header.Set("Authorization", "Bearer "+betoTok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "beto" {
		t.Fatalf("userFrom = %q, want beto", got)
	}
}

// TestRevokedTokenRejected: a token revoked in the file, or through the
// admin API, opens nothing.
func TestRevokedTokenRejected(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	carla := as(t, srv, carlaTok)
	if code := carla.fullState("d3", 0, 0, w("d3:ws", "x.go")); code != http.StatusUnauthorized {
		t.Fatalf("revoked in the file: status %d, want 401", code)
	}
	ana := as(t, srv, anaTok)
	if code := ana.fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("before the revoke: status %d", code)
	}
	if code, b := as(t, srv, adminTok).do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d %s", code, b)
	}
	if code := ana.fullState("d1", 0, 0); code != http.StatusUnauthorized {
		t.Fatalf("after the revoke: status %d, want 401", code)
	}
	if code, _ := ana.download("d1:ws", "d1"); code != http.StatusUnauthorized {
		t.Fatalf("snapshot after the revoke: status %d, want 401", code)
	}
}

// TestRevokeClosesStream: revoking a person ends their open streams, and
// only theirs, at once.
func TestRevokeClosesStream(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	_, anaCh := ana.stream("d1")
	_, betoCh := beto.stream("d2")
	next(t, anaCh)
	next(t, betoCh)
	if code, b := as(t, srv, adminTok).do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d %s", code, b)
	}
	select {
	case ws, open := <-anaCh:
		if open {
			t.Fatalf("ana's stream sent %v after the revoke, want it closed", ws)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ana's stream still open 2s after the revoke")
	}
	if code := ana.fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusUnauthorized {
		t.Fatalf("POST after the revoke: status %d, want 401", code)
	}
	if code, _ := ana.stream("d1"); code != http.StatusUnauthorized {
		t.Fatalf("reconnect after the revoke: status %d, want 401", code)
	}
	// beto's stream is still there and still gets his warnings.
	beto.fullState("d2", 0, 0, w("d2:ws", "x.go"))
	beto.fullState("d4", 0, 0, w("d4:ws", "x.go"))
	if ws := next(t, betoCh); len(ws) != 1 {
		t.Fatalf("beto's stream after ana's revoke: %v", ws)
	}
}

// TestDaemonIDBoundToUser: a daemon_id belongs to the first person who used
// it; another person using it gets 403 on every route that carries it, and
// cannot replace the first one's state or bundle.
func TestDaemonIDBoundToUser(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	if code := ana.fullState("d1", 0, 0, w("d1:ws1", "a.go")); code != http.StatusNoContent {
		t.Fatalf("ana's full_state: status %d", code)
	}
	if code := beto.fullState("d2", 0, 0, w("d2:ws2", "a.go")); code != http.StatusNoContent {
		t.Fatalf("beto's full_state: status %d", code)
	}
	if code := ana.upload("d1:ws1", "d1", []byte("ana's bundle")); code != http.StatusNoContent {
		t.Fatalf("ana's upload: status %d", code)
	}

	forbidden := func(what string, code int, body []byte) {
		t.Helper()
		if code != http.StatusForbidden || !strings.Contains(string(body), "daemon_id of another person") {
			t.Errorf("%s as ana's daemon: status %d %q, want 403 daemon_id of another person", what, code, body)
		}
	}
	b, _ := json.Marshal(wire.Message{Type: wire.TypeFullState, FullState: core.FullState{Daemon: "d1"}})
	code, body := beto.do(http.MethodPost, "/v1/msg", "application/json", b)
	forbidden("full_state", code, body)
	b, _ = json.Marshal(wire.Message{Type: wire.TypeConflicts, FullState: core.FullState{Daemon: "d1"}, Conflicts: []core.Conflict{{
		ID: "c1", Repo: "r", Path: "a.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:ws1"}, {Daemon: "d2", Workstream: "d2:ws2"}},
	}}})
	code, body = beto.do(http.MethodPost, "/v1/msg", "application/json", b)
	forbidden("conflicts", code, body)
	code, body = beto.do(http.MethodPost, "/v1/snapshot?ws=d1:ws1&daemon_id=d1", wire.BundleType, []byte("beto's bundle"),
		wire.HeaderCommit, commitC, wire.HeaderBase, baseB)
	forbidden("snapshot POST", code, body)
	code, body = beto.download("d2:ws2", "d1")
	forbidden("snapshot GET", code, body)
	if code, _ := beto.stream("d1"); code != http.StatusForbidden {
		t.Errorf("stream as ana's daemon: status %d, want 403", code)
	}

	d := ana.state()
	if wsIDs(d.Workstreams) != "d1:ws1,d2:ws2" || len(d.Warnings) != 1 || len(d.Conflicts) != 0 {
		t.Fatalf("beto changed the state through d1: %+v", d)
	}
	if code, got := beto.download("d1:ws1", "d2"); code != http.StatusOK || string(got) != "ana's bundle" {
		t.Fatalf("ana's bundle: status %d %q", code, got)
	}
	// ana's daemon id still works for ana.
	if code := ana.fullState("d1", 0, 0, w("d1:ws1", "b.go")); code != http.StatusNoContent {
		t.Fatalf("ana again: status %d", code)
	}
}

// TestAllowlistDropsOtherRepos: workstreams of repos outside the allowlist
// never reach the state, so they make no warning and take no bundle; an
// empty allowlist lets none through; a repo added later shows up at once.
func TestAllowlistDropsOtherRepos(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	ana, beto, admin := as(t, srv, anaTok), as(t, srv, betoTok), as(t, srv, adminTok)
	ana.fullState("d1", 0, 0, wr("d1:app", repoApp, "x.go"), wr("d1:api", repoAPI, "y.go"))
	beto.fullState("d2", 0, 0, wr("d2:app", repoApp, "x.go"), wr("d2:api", repoAPI, "y.go"))
	d := ana.state()
	if wsIDs(d.Workstreams) != "d1:app,d2:app" || len(d.Warnings) != 1 || d.Warnings[0].Repo != repoApp {
		t.Fatalf("state = %+v, want only %s", d, repoApp)
	}
	if code := ana.upload("d1:api", "d1", []byte("x")); code != http.StatusConflict {
		t.Fatalf("snapshot of a dropped workstream: status %d, want 409", code)
	}
	c := core.Conflict{ID: "c-api", Repo: repoAPI, Path: "y.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:api"}, {Daemon: "d2", Workstream: "d2:api"}}}
	if code := ana.conflicts("d1", c); code != http.StatusNoContent {
		t.Fatalf("conflicts: status %d", code)
	}
	if d := ana.state(); len(d.Conflicts) != 0 {
		t.Fatalf("a conflict of a repo outside the allowlist was kept: %+v", d.Conflicts)
	}

	if code, b := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", []byte(`{"repo":"https://github.com/Acme/API.git"}`)); code != http.StatusNoContent {
		t.Fatalf("add repo: status %d %s", code, b)
	}
	if d := ana.state(); wsIDs(d.Workstreams) != "d1:api,d1:app,d2:api,d2:app" || len(d.Warnings) != 2 {
		t.Fatalf("after allowing %s without a resend: %+v", repoAPI, d)
	}

	_, empty, _ := stateServer(t)
	as(t, empty, anaTok).fullState("d1", 0, 0, wr("d1:app", repoApp, "x.go"))
	if d := as(t, empty, anaTok).state(); len(d.Workstreams) != 0 {
		t.Fatalf("an empty allowlist let %v through", d.Workstreams)
	}
}

// TestAllowlistNormalizesWorkstreamRepo: a daemon that sends its repo in a
// non-canonical form (capitals, a URL, ".git") is matched against the
// allowlist by its normalized identity, and pairs with one that sends the
// canonical form.
func TestAllowlistNormalizesWorkstreamRepo(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	ana.fullState("d1", 0, 0, wr("d1:app", "github.com/Acme/App", "x.go"))
	beto.fullState("d2", 0, 0, wr("d2:app", "https://github.com/acme/app.git", "x.go"))
	d := ana.state()
	if wsIDs(d.Workstreams) != "d1:app,d2:app" || len(d.Warnings) != 1 || d.Warnings[0].Repo != repoApp {
		t.Fatalf("state = %+v, want both workstreams paired under %s", d, repoApp)
	}
}

// TestConflictsMustInvolveReporter: a daemon reports only conflicts one of
// whose sides is its own.
func TestConflictsMustInvolveReporter(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	beto := as(t, srv, betoTok)
	foreign := core.Conflict{ID: "c-foreign", Repo: repoApp, Path: "x.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:app"}, {Daemon: "d3", Workstream: "d3:app"}}}
	mine := core.Conflict{ID: "c-mine", Repo: repoApp, Path: "y.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:app"}, {Daemon: "d2", Workstream: "d2:app"}}}
	if code := beto.conflicts("d2", foreign, mine); code != http.StatusNoContent {
		t.Fatalf("conflicts: status %d", code)
	}
	d := beto.state()
	if len(d.Conflicts) != 1 || d.Conflicts[0].ID != "c-mine" {
		t.Fatalf("conflicts = %+v, want only c-mine", d.Conflicts)
	}
}

// TestAllowlistRemoveDropsRepo: a repo that leaves the allowlist leaves the
// state at once, with its warnings, bundles and reported conflicts, and the
// daemons' messages are ordered as before.
func TestAllowlistRemoveDropsRepo(t *testing.T) {
	s, srv, _ := stateServer(t, repoApp, repoAPI)
	ana, beto, admin := as(t, srv, anaTok), as(t, srv, betoTok), as(t, srv, adminTok)
	if code := ana.fullState("d1", 7, 3, wr("d1:app", repoApp, "x.go"), wr("d1:api", repoAPI, "y.go")); code != http.StatusNoContent {
		t.Fatalf("ana: status %d", code)
	}
	beto.fullState("d2", 0, 0, wr("d2:app", repoApp, "x.go"), wr("d2:api", repoAPI, "y.go"))
	_, ch := ana.stream("d1")
	if ws := next(t, ch); len(ws) != 2 {
		t.Fatalf("first warnings = %v, want 2", ws)
	}
	ana.upload("d1:app", "d1", []byte("app"))
	ana.upload("d1:api", "d1", []byte("api"))
	side := func(d, ws string) core.Side {
		return core.Side{Daemon: core.DaemonID(d), Workstream: core.WorkstreamID(ws)}
	}
	ana.conflicts("d1",
		core.Conflict{ID: "c-app", Repo: repoApp, Path: "x.go", Kind: "content", Sides: [2]core.Side{side("d1", "d1:app"), side("d2", "d2:app")}},
		core.Conflict{ID: "c-api", Repo: repoAPI, Path: "y.go", Kind: "content", Sides: [2]core.Side{side("d1", "d1:api"), side("d2", "d2:api")}})
	if d := ana.state(); len(d.Conflicts) != 2 {
		t.Fatalf("conflicts before = %+v", d.Conflicts)
	}

	if code, b := admin.do(http.MethodDelete, "/v1/admin/repos/"+repoAPI, "", nil); code != http.StatusNoContent {
		t.Fatalf("remove repo: status %d %s", code, b)
	}
	if ws := next(t, ch); len(ws) != 1 || ws[0].Repo != repoApp {
		t.Fatalf("stream after the removal = %v, want only %s's warning", ws, repoApp)
	}
	d := ana.state()
	if wsIDs(d.Workstreams) != "d1:app,d2:app" || len(d.Warnings) != 1 || len(d.Conflicts) != 1 || d.Conflicts[0].ID != "c-app" {
		t.Fatalf("state after the removal = %+v", d)
	}
	if code, _ := beto.download("d1:api", "d2"); code != http.StatusNotFound {
		t.Fatalf("bundle of the removed repo: status %d, want 404", code)
	}
	if code, b := beto.download("d1:app", "d2"); code != http.StatusOK || string(b) != "app" {
		t.Fatalf("bundle of the kept repo: status %d %q", code, b)
	}
	s.mu.Lock()
	n := s.snapBytes
	s.mu.Unlock()
	if n != len("app") {
		t.Fatalf("snapBytes = %d, want %d", n, len("app"))
	}

	// The re-apply left ana's clock alone: a duplicate of (7, 3) is still
	// stale, and (7, 4) goes through.
	ana.fullState("d1", 7, 3)
	if d := ana.state(); wsIDs(d.Workstreams) != "d1:app,d2:app" {
		t.Fatalf("a stale full_state was applied: %+v", d.Workstreams)
	}
	ana.fullState("d1", 7, 4, wr("d1:app", repoApp, "z.go"), wr("d1:api", repoAPI, "y.go"))
	if d := ana.state(); wsIDs(d.Workstreams) != "d1:app,d2:app" || len(d.Warnings) != 0 {
		t.Fatalf("after (7, 4): %+v", d)
	}
}

// TestLegacySharedTokenWithoutState: without a state file serve works as
// before: the shared token, no persons, every repo, and no admin API.
func TestLegacySharedTokenWithoutState(t *testing.T) {
	s := New()
	s.SetToken("t")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	c := as(t, srv, "t")
	if code := c.fullState("d1", 0, 0, wr("a", "any/repo", "x.go")); code != http.StatusNoContent {
		t.Fatalf("full_state: status %d", code)
	}
	if code := c.fullState("d2", 0, 0, wr("b", "any/repo", "x.go")); code != http.StatusNoContent {
		t.Fatalf("full_state: status %d", code)
	}
	if d := c.state(); len(d.Workstreams) != 2 || len(d.Warnings) != 1 {
		t.Fatalf("state = %+v", d)
	}
	if code := as(t, srv, "").fullState("d1", 0, 0); code != http.StatusUnauthorized {
		t.Fatalf("without the shared token: status %d, want 401", code)
	}
	if code, _ := c.do(http.MethodGet, "/v1/admin/tokens", "", nil); code != http.StatusNotFound {
		t.Fatalf("admin without a state file: status %d, want 404", code)
	}
}

// TestSharedTokenRejectedWithState: with a state file the shared token of
// DUGOUT_TOKEN opens nothing.
func TestSharedTokenRejectedWithState(t *testing.T) {
	s := New()
	s.SetToken("t")
	path := filepath.Join(t.TempDir(), "serve.json")
	st := Stored{Version: 1, AdminHash: HashToken(adminTok), Users: []User{{Name: "ana", TokenHash: HashToken(anaTok)}}}
	s.UseState(path, st)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	c := as(t, srv, "t")
	if code := c.fullState("d1", 0, 0); code != http.StatusUnauthorized {
		t.Errorf("POST /v1/msg with the shared token: status %d, want 401", code)
	}
	if code, _ := c.do(http.MethodGet, "/v1/state", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/state with the shared token: status %d, want 401", code)
	}
	if code, _ := c.do(http.MethodGet, "/v1/admin/tokens", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/admin/tokens with the shared token: status %d, want 401", code)
	}
}

// TestRevokeRemovesPersonState: a revoke drops the person's state at once:
// their workstreams, the warnings and reported conflicts they are in, and
// their bundles, and the others' streams get the change. A new token for the
// same name brings the person back with their daemon's next message: its
// clock was left alone.
func TestRevokeRemovesPersonState(t *testing.T) {
	s, srv, _ := stateServer(t, "r")
	ana, beto, admin := as(t, srv, anaTok), as(t, srv, betoTok), as(t, srv, adminTok)
	ana.fullState("d1", 5, 1, w("d1:a", "x.go"))
	beto.fullState("d2", 0, 0, w("d2:b", "x.go"))
	_, ch := beto.stream("d2")
	if ws := next(t, ch); len(ws) != 1 {
		t.Fatalf("beto's first warnings = %v, want 1", ws)
	}
	ana.upload("d1:a", "d1", []byte("ana's"))
	c := core.Conflict{ID: "c1", Repo: "r", Path: "x.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:a"}, {Daemon: "d2", Workstream: "d2:b"}}}
	ana.conflicts("d1", c)
	beto.conflicts("d2", c)
	if d := beto.state(); len(d.Conflicts) != 1 {
		t.Fatalf("conflicts before = %+v", d.Conflicts)
	}

	if code, b := admin.do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d %s", code, b)
	}
	if ws := next(t, ch); len(ws) != 0 {
		t.Fatalf("beto's stream after the revoke = %v, want []", ws)
	}
	if d := beto.state(); wsIDs(d.Workstreams) != "d2:b" || len(d.Warnings) != 0 || len(d.Conflicts) != 0 {
		t.Fatalf("state after the revoke = %+v", d)
	}
	if code, _ := beto.download("d1:a", "d2"); code != http.StatusNotFound {
		t.Fatalf("ana's bundle after the revoke: status %d, want 404", code)
	}
	s.mu.Lock()
	n := s.snapBytes
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("snapBytes = %d, want 0", n)
	}

	ana = as(t, srv, issue(t, admin, "ana"))
	ana.fullState("d1", 5, 1, w("d1:a", "x.go"))
	if d := beto.state(); wsIDs(d.Workstreams) != "d2:b" {
		t.Fatalf("a stale full_state came back after the revoke: %+v", d.Workstreams)
	}
	if code := ana.fullState("d1", 5, 2, w("d1:a", "x.go")); code != http.StatusNoContent {
		t.Fatalf("ana's new token on her daemon: status %d", code)
	}
	if d := beto.state(); wsIDs(d.Workstreams) != "d1:a,d2:b" || len(d.Warnings) != 1 {
		t.Fatalf("ana is not back: %+v", d)
	}
}

// TestRevokeRacesInFlightPost: a POST whose guard ran before a revoke, and
// whose handler runs after it, changes nothing.
func TestRevokeRacesInFlightPost(t *testing.T) {
	for _, c := range []struct {
		name, path, ctype, body string
		hdr                     []string
	}{
		{"full_state", "/v1/msg", "application/json",
			`{"type":"full_state","daemon_id":"d1","workstreams":[{"id":"d1:c","repo":"r","branch":"c","files":["x.go"]}]}`, nil},
		{"conflicts", "/v1/msg", "application/json",
			`{"type":"conflicts","daemon_id":"d1","conflicts":[{"id":"c1","repo":"r","path":"x.go","kind":"content",` +
				`"sides":[{"daemon_id":"d1","workstream_id":"d1:a"},{"daemon_id":"d2","workstream_id":"d2:b"}]}]}`, nil},
		{"message", "/v1/msg", "application/json",
			`{"type":"message","daemon_id":"d1","message":{"from":"d1:a","to":"d1:a"}}`, nil},
		{"escalated", "/v1/msg", "application/json",
			`{"type":"escalated","daemon_id":"d1","escalated":{"conflicts":["c1"],"trigger":"time"}}`, nil},
		{"snapshot", "/v1/snapshot?ws=d1:a&daemon_id=d1", wire.BundleType, "bundle",
			[]string{wire.HeaderCommit, commitC, wire.HeaderBase, baseB}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, srv, _ := stateServer(t, "r")
			as(t, srv, anaTok).fullState("d1", 0, 0, w("d1:a", "x.go"))
			as(t, srv, betoTok).fullState("d2", 0, 0, w("d2:b", "x.go"))
			reached, release := make(chan struct{}), make(chan struct{})
			afterAuth = func(r *http.Request) {
				if userFrom(r.Context()) == "ana" {
					close(reached)
					<-release
				}
			}
			t.Cleanup(func() { afterAuth = func(*http.Request) {} })
			req, _ := http.NewRequest(http.MethodPost, srv.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", c.ctype)
			req.Header.Set("Authorization", "Bearer "+anaTok)
			for i := 0; i+1 < len(c.hdr); i += 2 {
				req.Header.Set(c.hdr[i], c.hdr[i+1])
			}
			done := make(chan int, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- 0
					return
				}
				resp.Body.Close()
				done <- resp.StatusCode
			}()
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("ana's POST never got past the guard")
			}
			code, b := as(t, srv, adminTok).do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil)
			close(release)
			if code != http.StatusNoContent {
				t.Fatalf("revoke: status %d %s", code, b)
			}
			if code := <-done; code != http.StatusUnauthorized {
				t.Fatalf("POST past the guard before the revoke: status %d, want 401", code)
			}
			if d := as(t, srv, betoTok).state(); wsIDs(d.Workstreams) != "d2:b" || len(d.Conflicts) != 0 {
				t.Fatalf("the POST changed the state after the revoke: %+v", d)
			}
			s.mu.Lock()
			n := len(s.snaps)
			s.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d bundles stored after the revoke", n)
			}
		})
	}
}

// TestForeignWorkstreamIDDropped: a daemon cannot report a workstream id
// that is not its own ("<daemon_id>:..."): serve drops it, so it never lists
// it under that daemon, never makes it its owner, and takes no bundle for it.
func TestForeignWorkstreamIDDropped(t *testing.T) {
	s, srv, _ := stateServer(t, "r")
	logs := &lockedBuf{}
	s.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	// beto's daemon claims ana's id before ana's daemon reports it, twice.
	for range 2 {
		beto.fullState("d2", 0, 0, w("d1:a", "y.go"), w("d2:b", "x.go"))
	}
	if d := beto.state(); wsIDs(d.Workstreams) != "d2:b" {
		t.Fatalf("state = %+v, want only d2:b", d.Workstreams)
	}
	if code := beto.upload("d1:a", "d2", []byte("beto's")); code != http.StatusConflict {
		t.Fatalf("beto's upload for d1:a: status %d, want 409", code)
	}
	ana.fullState("d1", 0, 0, w("d1:a", "x.go"))
	if code := ana.upload("d1:a", "d1", []byte("ana's")); code != http.StatusNoContent {
		t.Fatalf("ana's upload: status %d", code)
	}
	beto.fullState("d2", 0, 0, w("d1:a", "y.go"), w("d2:b", "x.go"))
	if code := beto.upload("d1:a", "d2", []byte("beto's")); code != http.StatusConflict {
		t.Fatalf("beto's upload for d1:a after ana's: status %d, want 409", code)
	}
	if code, b := beto.download("d1:a", "d2"); code != http.StatusOK || string(b) != "ana's" {
		t.Fatalf("d1:a's bundle: status %d %q, want ana's", code, b)
	}
	d := beto.state()
	if wsIDs(d.Workstreams) != "d1:a,d2:b" || len(d.Workstreams[0].Files) != 1 || d.Workstreams[0].Files[0] != "x.go" {
		t.Fatalf("state = %+v, want ana's d1:a and beto's d2:b", d.Workstreams)
	}
	if n := strings.Count(logs.String(), `msg="workstream id of another daemon"`); n != 1 {
		t.Fatalf("foreign id warned %d times, want once:\n%s", n, logs)
	}
}
