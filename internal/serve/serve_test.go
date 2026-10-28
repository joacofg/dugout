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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// newServer closes the server in a cleanup, so it runs after the stream
// cleanups registered later: httptest's Close waits for open streams.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New().Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string) int {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/msg", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postState(t *testing.T, srv *httptest.Server, d core.DaemonID, ws ...core.Workstream) {
	t.Helper()
	b, err := json.Marshal(wire.Message{Type: wire.TypeFullState, FullState: core.FullState{Daemon: d, Workstreams: ws}})
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, string(b)); code != http.StatusNoContent {
		t.Fatalf("POST full_state: status %d", code)
	}
}

func dump(t *testing.T, srv *httptest.Server) wire.StateDump {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d wire.StateDump
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func w(id, file string) core.Workstream {
	return core.Workstream{ID: core.WorkstreamID(id), Repo: "r", Branch: "b-" + id, Files: []string{file}}
}

func TestFullStateCreatesWarning(t *testing.T) {
	srv := newServer(t)
	postState(t, srv, "d1", w("a", "x.go"))
	postState(t, srv, "d2", w("b", "x.go"))
	d := dump(t, srv)
	if len(d.Warnings) != 1 || d.Warnings[0].Path != "x.go" || len(d.Workstreams) != 2 {
		t.Fatalf("state = %+v", d)
	}
}

func TestUnknownTypeIgnored(t *testing.T) {
	srv := newServer(t)
	if code := post(t, srv, `{"type":"delivered","id":"x"}`); code != http.StatusNoContent {
		t.Fatalf("unknown type: status %d", code)
	}
	if d := dump(t, srv); len(d.Workstreams) != 0 || len(d.Warnings) != 0 {
		t.Fatalf("unknown type changed state: %+v", d)
	}
}

func TestBadMessage(t *testing.T) {
	srv := newServer(t)
	if code := post(t, srv, `{not json`); code != http.StatusBadRequest {
		t.Errorf("bad json: status %d", code)
	}
	if code := post(t, srv, `{"type":"full_state","workstreams":[]}`); code != http.StatusBadRequest {
		t.Errorf("full_state without daemon_id: status %d", code)
	}
}

func TestFullStateReplacesOnServer(t *testing.T) {
	srv := newServer(t)
	postState(t, srv, "d1", w("a", "x.go"))
	postState(t, srv, "d2", w("b", "x.go"))
	postState(t, srv, "d2", w("b", "other.go"))
	if d := dump(t, srv); len(d.Warnings) != 0 || len(d.Workstreams) != 2 {
		t.Fatalf("replace kept the old set: %+v", d)
	}
}

// stream subscribes as d and returns a channel of the warning sets it gets.
func stream(t *testing.T, srv *httptest.Server, d string) <-chan []core.Warning {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/stream?daemon_id="+d, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan []core.Warning, 16)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 2*maxMessage) // a warnings event can be long: see TestServeCapsIntent
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
	return out
}

func next(t *testing.T, ch <-chan []core.Warning) []core.Warning {
	t.Helper()
	select {
	case ws := <-ch:
		return ws
	case <-time.After(2 * time.Second):
		t.Fatal("no warnings event within 2s")
		return nil
	}
}

func TestStreamPushesChanges(t *testing.T) {
	srv := newServer(t)
	ch := stream(t, srv, "d1")
	if ws := next(t, ch); ws == nil || len(ws) != 0 {
		t.Fatalf("first event = %v, want []", ws)
	}
	postState(t, srv, "d1", w("a", "x.go"))
	postState(t, srv, "d2", w("b", "x.go"))
	if ws := next(t, ch); len(ws) != 1 || ws[0].Path != "x.go" {
		t.Fatalf("after overlap = %+v", ws)
	}
	postState(t, srv, "d2")
	if ws := next(t, ch); len(ws) != 0 {
		t.Fatalf("after d2 cleared = %+v", ws)
	}
}

func TestStreamNeedsDaemonID(t *testing.T) {
	srv := newServer(t)
	resp, err := http.Get(srv.URL + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stream without daemon_id: status %d", resp.StatusCode)
	}
}

const evilState = `{"type":"full_state","daemon_id":"evil","workstreams":[{"id":"evil:1","repo":"r","branch":"x","files":["x.go"]}]}`

func do(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestRejectsBrowserRequests: a web page can send a no-cors text/plain POST
// to localhost without a preflight, and DNS rebinding lets it read
// responses. None of that may reach the core, which feeds agent context.
func TestRejectsBrowserRequests(t *testing.T) {
	srv := newServer(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/msg", strings.NewReader(evilState))
	req.Header.Set("Content-Type", "text/plain")
	if code := do(t, req); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST: status %d, want 415", code)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/msg", strings.NewReader(evilState))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	if code := do(t, req); code != http.StatusForbidden {
		t.Errorf("POST with Origin: status %d, want 403", code)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/stream?daemon_id=d1", nil)
	req.Header.Set("Origin", "https://evil.example")
	if code := do(t, req); code != http.StatusForbidden {
		t.Errorf("stream with Origin: status %d, want 403", code)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/state", nil)
	req.Host = "evil.example:7457"
	if code := do(t, req); code != http.StatusForbidden {
		t.Errorf("state with a foreign Host: status %d, want 403", code)
	}

	if d := dump(t, srv); len(d.Workstreams) != 0 {
		t.Fatalf("a rejected request changed the state: %+v", d)
	}
}

// recorder is a slog.Handler that keeps every record's message.
type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, rec.Message)
	return nil
}
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }
func (r *recorder) count(msg string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// TestWarningCreatedLoggedOnce: serve logs a warning when it first appears
// and when it goes away, not on every resend of the same full state.
func TestWarningCreatedLoggedOnce(t *testing.T) {
	s := New()
	rec := &recorder{}
	s.SetLogger(slog.New(rec))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	postState(t, srv, "d1", w("a", "x.go"))
	postState(t, srv, "d2", w("b", "x.go"))
	postState(t, srv, "d2", w("b", "x.go"))
	if n := rec.count("warning created"); n != 1 {
		t.Fatalf("warning created logged %d times, want 1", n)
	}
	postState(t, srv, "d2")
	if n := rec.count("warning resolved"); n != 1 {
		t.Fatalf("warning resolved logged %d times, want 1", n)
	}
}

func TestTokenRequired(t *testing.T) {
	s := New()
	s.SetToken("t")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })

	type call struct {
		method, path, body string
		ok                 int
	}
	calls := []call{
		{http.MethodPost, "/v1/msg", evilState, http.StatusNoContent},
		{http.MethodGet, "/v1/state", "", http.StatusOK},
		{http.MethodGet, "/v1/stream?daemon_id=d1", "", http.StatusOK},
	}
	for _, c := range calls {
		for auth, want := range map[string]int{"": http.StatusUnauthorized, "Bearer x": http.StatusUnauthorized, "t": http.StatusUnauthorized, "Bearer t": c.ok} {
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, c.method, srv.URL+c.path, strings.NewReader(c.body))
			if c.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != want {
				t.Errorf("%s %s with Authorization %q: status %d, want %d", c.method, c.path, auth, resp.StatusCode, want)
			}
			resp.Body.Close()
			cancel()
		}
	}
}

// TestAllowHostNormalized: -allow-host is typed by a person, who may add a
// port, IPv6 brackets, a trailing dot or capitals.
func TestAllowHostNormalized(t *testing.T) {
	s := New()
	for _, h := range []string{"office.vpn:7457", "[fd00::1]", "Other.VPN."} {
		s.AllowHost(h)
	}
	for _, host := range []string{"office.vpn:7457", "OFFICE.vpn.:7457", "[fd00::1]:7457", "other.vpn:7457"} {
		if !s.hostAllowed(host) {
			t.Errorf("hostAllowed(%q) = false", host)
		}
	}
}

func TestAllowedHosts(t *testing.T) {
	s := New()
	s.AllowHost("10.0.0.5")
	for host, want := range map[string]bool{
		"127.0.0.1:7457": true, "localhost:7457": true, "[::1]:7457": true, "localhost": true,
		"10.0.0.5:7457": true, "evil.example:7457": false, "10.0.0.6:7457": false,
	} {
		if got := s.hostAllowed(host); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestMessageTooLarge(t *testing.T) {
	srv := newServer(t)
	big := `{"type":"full_state","daemon_id":"d","workstreams":[{"id":"` + string(bytes.Repeat([]byte("a"), 5<<20)) + `"}]}`
	if code := post(t, srv, big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("5 MiB message: status %d", code)
	}
}

// TestServeCapsIntent: serve relays a workstream's intent in every warning the
// other daemons get, so it keeps only the first 280 runes of what was sent.
func TestServeCapsIntent(t *testing.T) {
	srv := newServer(t)
	ch := stream(t, srv, "d2")
	next(t, ch)
	a := w("a", "x.go")
	a.Intent = strings.Repeat("ë", (maxMessage-4096)/2) // about 4 MiB, within one message
	postState(t, srv, "d1", a)
	postState(t, srv, "d2", w("b", "x.go"))
	ws := next(t, ch)
	if len(ws) != 1 {
		t.Fatalf("d2's warnings = %d, want 1", len(ws))
	}
	var intent string
	for _, side := range ws[0].Sides {
		if side.Workstream == a.ID {
			intent = side.Intent
		}
	}
	if want := strings.Repeat("ë", 280); intent != want {
		t.Fatalf("d2's warning carries an intent of %d runes (valid UTF-8: %v), want the first 280",
			utf8.RuneCountInString(intent), utf8.ValidString(intent))
	}
}

// TestServeCapsPerson: a person's name reaches the other daemons' warnings
// and their humans' notifications, so serve keeps only its first 100 runes.
func TestServeCapsPerson(t *testing.T) {
	srv := newServer(t)
	a := w("a", "x.go")
	a.Person = strings.Repeat("ñ", 5000)
	postState(t, srv, "d1", a)
	postState(t, srv, "d2", w("b", "x.go"))
	d := dump(t, srv)
	if len(d.Warnings) != 1 {
		t.Fatalf("warnings = %d, want 1", len(d.Warnings))
	}
	for _, side := range d.Warnings[0].Sides {
		if side.Workstream == a.ID && side.Person != strings.Repeat("ñ", 100) {
			t.Fatalf("the warning names a person of %d runes, want the first 100", utf8.RuneCountInString(side.Person))
		}
	}
}

// TestServeDropsOversizedWorkstream: a workstream with a branch or a path
// past the caps is dropped on arrival, with one warning per daemon however
// often it comes, and the daemon's other workstreams stay. Re-applying the
// last full state, as an allowlist change does, keeps it out.
func TestServeDropsOversizedWorkstream(t *testing.T) {
	s, srv, _ := stateServer(t, repoApp)
	rec := &attrRecorder{}
	s.SetLogger(slog.New(rec))
	branch := core.Workstream{ID: "d1:branch", Repo: repoApp, Branch: strings.Repeat("b", 300), Files: []string{"x.go"}}
	path := core.Workstream{ID: "d1:path", Repo: repoApp, Branch: "path", Files: []string{"y.go", strings.Repeat("p/", 1000)}}
	ok := core.Workstream{ID: "d1:ok", Repo: repoApp, Branch: "ok", Files: []string{"z.go"}}
	ana := as(t, srv, anaTok)
	for range 2 {
		if code := ana.fullState("d1", 0, 0, branch, path, ok); code != http.StatusNoContent {
			t.Fatalf("full_state: status %d", code)
		}
	}
	if got := wsIDs(ana.state().Workstreams); got != "d1:ok" {
		t.Fatalf("workstreams = %q, want only d1:ok", got)
	}
	admin := as(t, srv, adminTok)
	if code, b := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", []byte(`{"repo":"`+repoAPI+`"}`)); code != http.StatusNoContent {
		t.Fatalf("add repo: status %d %s", code, b)
	}
	if got := wsIDs(ana.state().Workstreams); got != "d1:ok" {
		t.Fatalf("after the allowlist changed, workstreams = %q, want only d1:ok", got)
	}
	got := rec.find("dropping workstream with oversized fields")
	if len(got) != 1 {
		t.Fatalf("logged %d times, want once: %v", len(got), got)
	}
	if a := got[0]; a["level"] != "WARN" || a["daemon"] != "d1" || a["workstream"] != "d1:branch" {
		t.Fatalf("log = %v", a)
	}
}

// TestStaleEpochWarns: a full state or conflict set from an epoch below the
// last one serve took from its daemon is a run that went back, and serve
// would ignore that daemon from then on: it warns. A duplicate or an older
// seq of the current epoch is the network's doing and stays quiet.
func TestStaleEpochWarns(t *testing.T) {
	for _, typ := range []string{wire.TypeFullState, wire.TypeConflicts} {
		t.Run(typ, func(t *testing.T) {
			s := New()
			logs := &lockedBuf{}
			s.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
			srv := httptest.NewServer(s.Handler())
			t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
			send := func(epoch, seq uint64) {
				t.Helper()
				b, _ := json.Marshal(wire.Message{Type: typ, FullState: core.FullState{Daemon: "d1", Epoch: epoch, Seq: seq,
					Workstreams: []core.Workstream{w("ws1", "a.go")}}})
				if code := post(t, srv, string(b)); code != http.StatusNoContent {
					t.Fatalf("%s: status %d", typ, code)
				}
			}
			warned := func() []string {
				var out []string
				for _, l := range strings.Split(logs.String(), "\n") {
					if strings.Contains(l, `msg="stale epoch"`) {
						out = append(out, l)
					}
				}
				return out
			}
			send(5, 2)
			send(5, 2)
			send(5, 1)
			if got := warned(); len(got) != 0 {
				t.Fatalf("a duplicate or an older seq warned:\n%s", strings.Join(got, "\n"))
			}
			send(4, 9)
			got := warned()
			if len(got) != 1 {
				t.Fatalf("an older epoch warned %d times, want 1:\n%s", len(got), logs)
			}
			for _, want := range []string{"level=WARN", "daemon=d1", "got=4", "have=5"} {
				if !strings.Contains(got[0], want) {
					t.Errorf("warning lacks %q: %s", want, got[0])
				}
			}
		})
	}
}

// attrRecorder keeps the message and attrs of every record.
type attrRecorder struct {
	mu    sync.Mutex
	msgs  []string
	attrs []map[string]string
}

func (r *attrRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *attrRecorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{"level": rec.Level.String()}
	rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, rec.Message)
	r.attrs = append(r.attrs, attrs)
	return nil
}
func (r *attrRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *attrRecorder) WithGroup(string) slog.Handler      { return r }

// find returns the attrs of every record with msg.
func (r *attrRecorder) find(msg string) []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]string
	for i, m := range r.msgs {
		if m == msg {
			out = append(out, r.attrs[i])
		}
	}
	return out
}

// TestServeSendsProtocolHeader: every response carries the version, the
// guard's refusals and the stream included.
func TestServeSendsProtocolHeader(t *testing.T) {
	s := New()
	s.SetToken("t")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	want := strconv.Itoa(wire.ProtocolVersion)
	for _, c := range []struct {
		name, method, path, token, origin string
		status                            int
	}{
		{"state", "GET", "/v1/state", "t", "", http.StatusOK},
		{"no token", "GET", "/v1/state", "", "", http.StatusUnauthorized},
		{"origin", "GET", "/v1/state", "t", "http://evil", http.StatusForbidden},
		{"unknown route", "GET", "/v1/nope", "t", "", http.StatusNotFound},
		{"stream", "GET", "/v1/stream?daemon_id=d1", "t", "", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
			if c.token != "" {
				req.Header.Set("Authorization", "Bearer "+c.token)
			}
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.status)
			}
			if got := resp.Header.Get(wire.HeaderProtocol); got != want {
				t.Fatalf("%s = %q, want %q", wire.HeaderProtocol, got, want)
			}
		})
	}
}

// TestProtocolMismatchLoggedOnce (serve side): a daemon on another version is
// logged once per daemon id, however many messages it sends.
func TestProtocolMismatchLoggedOnce(t *testing.T) {
	s := New()
	rec := &attrRecorder{}
	s.SetLogger(slog.New(rec))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	send := func(d string, v int) {
		body := `{"type":"full_state","daemon_id":"` + d + `","protocol_version":` + strconv.Itoa(v) + `}`
		if code := post(t, srv, body); code != http.StatusNoContent {
			t.Fatalf("status %d", code)
		}
	}
	for range 3 {
		send("d1", wire.ProtocolVersion+1)
	}
	send("d2", wire.ProtocolVersion+2)
	got := rec.find("protocol mismatch")
	if len(got) != 2 {
		t.Fatalf("logged %d mismatches, want one per daemon: %v", len(got), got)
	}
	if a := got[0]; a["level"] != "WARN" || a["ours"] != strconv.Itoa(wire.ProtocolVersion) || a["theirs"] != strconv.Itoa(wire.ProtocolVersion+1) || a["daemon"] != "d1" {
		t.Fatalf("first mismatch = %v", a)
	}
}

// TestProtocolAbsentNotLogged: a message without the field is an older daemon,
// and one on our version is no mismatch; neither is logged.
func TestProtocolAbsentNotLogged(t *testing.T) {
	s := New()
	rec := &attrRecorder{}
	s.SetLogger(slog.New(rec))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	for _, body := range []string{
		`{"type":"full_state","daemon_id":"d1"}`,
		`{"type":"full_state","daemon_id":"d1","protocol_version":0}`,
		`{"type":"full_state","daemon_id":"d1","protocol_version":` + strconv.Itoa(wire.ProtocolVersion) + `}`,
	} {
		if code := post(t, srv, body); code != http.StatusNoContent {
			t.Fatalf("POST %s: status %d", body, code)
		}
	}
	if got := rec.find("protocol mismatch"); len(got) != 0 {
		t.Fatalf("logged %v, want nothing", got)
	}
}

// TestServeRejectsTooOldDaemon: a daemon on a protocol below the minimum gets
// 426 and the text that tells the person to update; no protocol_version (a
// pre-PR3b daemon) and one at the minimum are still accepted.
func TestServeRejectsTooOldDaemon(t *testing.T) {
	s := New()
	s.minProtocol = 3
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	do := func(body string) (int, string) {
		resp, err := http.Post(srv.URL+"/v1/msg", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	code, body := do(`{"type":"full_state","daemon_id":"d1","protocol_version":2}`)
	if code != http.StatusUpgradeRequired || body != "your dugout is older than this server accepts: update it" {
		t.Fatalf("protocol 2 under a minimum of 3: %d %q, want 426 and the update text", code, body)
	}
	for _, ok := range []string{
		`{"type":"full_state","daemon_id":"d1"}`,
		`{"type":"full_state","daemon_id":"d1","protocol_version":0}`,
		`{"type":"full_state","daemon_id":"d1","protocol_version":3}`,
	} {
		if code, _ := do(ok); code != http.StatusNoContent {
			t.Fatalf("POST %s: status %d, want 204", ok, code)
		}
	}
}
