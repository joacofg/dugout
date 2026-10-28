package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/snapshot"
	"github.com/joacofg/dugout/internal/wire"
)

func fixedState() core.FullState {
	return core.FullState{Daemon: "d1", Workstreams: []core.Workstream{{ID: "d1:a", Repo: "r", Branch: "a", Files: []string{"x.go"}}}}
}

func runUplink(t *testing.T, url string, resend time.Duration, onWarnings func([]core.Warning)) *Uplink {
	t.Helper()
	return startUplink(t, &Uplink{Server: url, Daemon: "d1", Resend: resend, Snapshot: fixedState, OnWarnings: onWarnings, Log: quiet})
}

// recorder is a slog.Handler that keeps every record's message and attrs,
// with its level under the key "level".
type recorder struct {
	mu    sync.Mutex
	msgs  []string
	attrs []map[string]string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{"level": rec.Level.String()}
	rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, rec.Message)
	r.attrs = append(r.attrs, attrs)
	return nil
}

// find returns the attrs of the first record with msg whose attrs include want.
func (r *recorder) find(msg string, want map[string]string) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
next:
	for i, m := range r.msgs {
		if m != msg {
			continue
		}
		for k, v := range want {
			if r.attrs[i][k] != v {
				continue next
			}
		}
		return r.attrs[i], true
	}
	return nil, false
}

// any reports whether a record with msg has attrs that match.
func (r *recorder) any(msg string, match func(map[string]string) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, m := range r.msgs {
		if m == msg && match(r.attrs[i]) {
			return true
		}
	}
	return false
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

func tokenServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	s := serve.New()
	s.SetToken(token)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

func workstreamsWithToken(t *testing.T, srv *httptest.Server, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/state", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d wire.StateDump
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return len(d.Workstreams)
}

func TestUplinkSendsToken(t *testing.T) {
	srv := tokenServer(t, "t")
	startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Token: "t", Resend: time.Hour, Snapshot: fixedState, OnWarnings: func([]core.Warning) {}, Log: quiet})
	eventually(t, 3*time.Second, "serve accepted the token", func() bool { return workstreamsWithToken(t, srv, "t") == 1 })
}

// TestUplinkWrongToken: a laptop with the wrong token sends nothing that
// sticks, and says so once, not on every retry.
func TestUplinkWrongToken(t *testing.T) {
	srv := tokenServer(t, "t")
	rec := &recorder{}
	startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Token: "x", Resend: 200 * time.Millisecond, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: slog.New(rec)})
	time.Sleep(2 * time.Second)
	if n := workstreamsWithToken(t, srv, "t"); n != 0 {
		t.Fatalf("serve took %d workstreams with a wrong token", n)
	}
	if n := rec.count("serve rejected the token"); n != 1 {
		t.Fatalf("logged the rejection %d times, want once (all: %v)", n, rec.msgs)
	}
}

// TestUplinkForbiddenIsNotATokenProblem: a 403 (Host not allowed) after a
// 401 must not claim the token works now, and must say once what to check.
func TestUplinkForbiddenIsNotATokenProblem(t *testing.T) {
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < 500*time.Millisecond {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(func() { closeServer(srv) })
	rec := &recorder{}
	startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Token: "t", Resend: 100 * time.Millisecond, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: slog.New(rec)})
	time.Sleep(2 * time.Second)
	if n := rec.count("serve accepted the token again"); n != 0 {
		t.Errorf("a 403 was reported as the token working again (%d times)", n)
	}
	if n := rec.count("serve rejected the token"); n != 1 {
		t.Errorf("401 logged %d times, want 1", n)
	}
	if n := rec.count("serve refused this host"); n != 1 {
		t.Errorf("403 hint logged %d times, want 1 (all: %v)", n, rec.msgs)
	}
}

// TestUplinkForbiddenOtherPerson: a 403 that says the daemon_id belongs to
// someone else is not a Host problem; the hint says how to get a new id.
func TestUplinkForbiddenOtherPerson(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon_id of another person", http.StatusForbidden)
	}))
	t.Cleanup(func() { closeServer(srv) })
	rec := &recorder{}
	startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Token: "t", Resend: 100 * time.Millisecond, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: slog.New(rec)})
	time.Sleep(time.Second)
	if n := rec.count("serve: this daemon_id belongs to another person"); n != 1 {
		t.Errorf("other-person 403 logged %d times, want 1 (all: %v)", n, rec.msgs)
	}
	if n := rec.count("serve refused this host"); n != 0 {
		t.Errorf("a daemon_id 403 was reported as a Host problem (%d times)", n)
	}
}

// TestUplinkWarnsWithoutTLS: an http:// server that is not loopback is
// warned about once at start; a loopback one is not.
func TestUplinkWarnsWithoutTLS(t *testing.T) {
	for server, want := range map[string]int{"http://dugout.invalid:7457": 1, "http://127.0.0.1:1": 0, "https://dugout.invalid": 0} {
		rec := &recorder{}
		startUplink(t, &Uplink{Server: server, Daemon: "d1", Token: "t", Resend: time.Hour, Snapshot: fixedState,
			OnWarnings: func([]core.Warning) {}, Log: slog.New(rec)})
		time.Sleep(300 * time.Millisecond)
		if n := rec.count("server without TLS"); n != want {
			t.Errorf("%s: warned %d times, want %d (all: %v)", server, n, want, rec.msgs)
		}
	}
}

func startUplink(t *testing.T, u *Uplink) *Uplink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return u
}

// TestUplinkResendsAfterServerRestart swaps in an empty serve behind the
// same URL, like a restart that keeps the stream open: the periodic full
// state fills the new server.
func TestUplinkResendsAfterServerRestart(t *testing.T) {
	var cur atomic.Pointer[serve.Server]
	cur.Store(serve.New())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur.Load().Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	runUplink(t, srv.URL, time.Second, func([]core.Warning) {})
	eventually(t, 3*time.Second, "first server has the workstream", func() bool { return len(warnings(t, srv).Workstreams) == 1 })

	cur.Store(serve.New())
	if len(warnings(t, srv).Workstreams) != 0 {
		t.Fatal("swapped server is not empty")
	}
	eventually(t, 3*time.Second, "restarted server has the workstream again", func() bool { return len(warnings(t, srv).Workstreams) == 1 })
}

// TestUplinkPostsOnConnect starts the server after the uplink, with a
// resend interval too long to matter: the post on connect fills it.
func TestUplinkPostsOnConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	var mu sync.Mutex
	var got []core.Warning
	runUplink(t, "http://"+addr, time.Hour, func(ws []core.Warning) { mu.Lock(); got = ws; mu.Unlock() })
	time.Sleep(300 * time.Millisecond) // the uplink is now retrying against a closed port

	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(serve.New().Handler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() { closeServer(srv) })
	eventually(t, 8*time.Second, "server has the workstream without a resend tick", func() bool {
		return len(warnings(t, srv).Workstreams) == 1
	})
	eventually(t, 2*time.Second, "stream delivered a warning set", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got != nil
	})
}

// postState sends daemon d's full state to srv, as an uplink would.
func postState(t *testing.T, srv *httptest.Server, d core.DaemonID, ws core.WorkstreamID) {
	t.Helper()
	body, _ := json.Marshal(wire.Message{Type: wire.TypeFullState, FullState: core.FullState{Daemon: d,
		Workstreams: []core.Workstream{{ID: ws, Repo: "r", Branch: string(d), Files: []string{"x.go"}}}}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/msg", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("post state: %d", resp.StatusCode)
	}
}

func TestUplinkSnapshotRoundTrip(t *testing.T) {
	srv := tokenServer(t, "t")
	postState(t, srv, "d1", "d1:a")
	postState(t, srv, "d2", "d2:b")
	u1 := &Uplink{Server: srv.URL, Daemon: "d1", Token: "t", Log: quiet}
	u2 := &Uplink{Server: srv.URL, Daemon: "d2", Token: "t", Log: quiet}
	ctx := context.Background()
	snap := snapshot.Snap{Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40)}
	bundle := []byte("bundle bytes")

	if _, _, _, err := u2.GetSnapshot(ctx, "d1:a", ""); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("before any upload: err = %v, want ErrNoSnapshot", err)
	}
	if err := u1.PutSnapshot(ctx, "d1:a", snap, bundle); err != nil {
		t.Fatal(err)
	}
	got, data, notModified, err := u2.GetSnapshot(ctx, "d1:a", "")
	if err != nil || notModified {
		t.Fatalf("get: err=%v notModified=%v", err, notModified)
	}
	if got != snap || !bytes.Equal(data, bundle) {
		t.Fatalf("got %+v %q, want %+v %q", got, data, snap, bundle)
	}
	if _, _, notModified, err := u2.GetSnapshot(ctx, "d1:a", snap.Commit); err != nil || !notModified {
		t.Fatalf("second get: err=%v notModified=%v, want notModified", err, notModified)
	}
	if _, _, _, err := u2.GetSnapshot(ctx, "d2:b", ""); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("ws without a bundle: err = %v, want ErrNoSnapshot", err)
	}
}

// TestUplinkSnapshotSendsToken: both bundle requests carry the bearer token.
func TestUplinkSnapshotSendsToken(t *testing.T) {
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(func() { closeServer(srv) })
	u := &Uplink{Server: srv.URL, Daemon: "d1", Token: "t", Log: quiet}
	snap := snapshot.Snap{Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40)}
	u.PutSnapshot(context.Background(), "d1:a", snap, []byte("x"))
	u.GetSnapshot(context.Background(), "d1:a", "")
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 2 || auth[0] != "Bearer t" || auth[1] != "Bearer t" {
		t.Fatalf("authorization headers = %v", auth)
	}
}

func TestUplinkSnapshotRejected(t *testing.T) {
	srv := tokenServer(t, "t")
	rec := &recorder{}
	u := &Uplink{Server: srv.URL, Daemon: "d1", Token: "wrong", Log: slog.New(rec)}
	snap := snapshot.Snap{Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40)}
	for range 3 {
		if err := u.PutSnapshot(context.Background(), "d1:a", snap, []byte("x")); err == nil {
			t.Fatal("PutSnapshot succeeded with a wrong token")
		}
	}
	if n := rec.count("serve rejected the token"); n != 1 {
		t.Fatalf("logged %d times, want once", n)
	}
}

func conflictFixture() core.Conflict {
	return core.Conflict{ID: "c1", Repo: "r", Path: "x.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:a", Branch: "a"}, {Daemon: "d2", Workstream: "d2:b", Branch: "b"}},
		Lines: [2][]core.Range{{{Start: 3, Len: 2}}, {{Start: 3, Len: 2}}},
		Text:  [2][]core.Snippet{{{Lines: []string{"SECRETCODE one"}}}, {{Lines: []string{"SECRETCODE two"}}}}}
}

func TestUplinkSendsConflicts(t *testing.T) {
	s := serve.New()
	var mu sync.Mutex
	var bodies []string
	var stamps []core.FullState
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/msg" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var m wire.Message
			json.Unmarshal(b, &m)
			if m.Type == wire.TypeConflicts {
				mu.Lock()
				bodies = append(bodies, string(b))
				stamps = append(stamps, m.FullState)
				mu.Unlock()
			}
		}
		s.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(bodies) }

	u := startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Epoch: 7, Resend: time.Hour, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: quiet})
	eventually(t, 3*time.Second, "full state arrived", func() bool { return len(warnings(t, srv).Workstreams) == 1 })
	c := conflictFixture()
	u.SetConflicts([]core.Conflict{c})
	eventually(t, 3*time.Second, "conflicts posted", func() bool { return count() == 1 })
	mu.Lock()
	body := bodies[0]
	mu.Unlock()
	if strings.Contains(body, `"text"`) || strings.Contains(body, "SECRETCODE") {
		t.Fatalf("conflicts body carries code: %s", body)
	}
	eventually(t, 3*time.Second, "state lists c1", func() bool {
		d := warnings(t, srv)
		return len(d.Conflicts) == 1 && d.Conflicts[0].ID == "c1"
	})

	u.SetConflicts([]core.Conflict{c})
	u.Notify()
	time.Sleep(500 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("an equal set sent %d conflicts posts, want 1", n)
	}

	// A server that forgot the set (restart) gets it again when the stream reconnects.
	srv.CloseClientConnections()
	eventually(t, 5*time.Second, "reconnect resends the conflicts", func() bool { return count() >= 2 })

	// Each set carries the run's epoch and a growing seq, so serve ignores a
	// stale one.
	mu.Lock()
	defer mu.Unlock()
	for i, st := range stamps {
		if st.Daemon != "d1" || st.Epoch != 7 || st.Seq == 0 || (i > 0 && st.Seq <= stamps[i-1].Seq) {
			t.Fatalf("conflicts post %d stamped %+v, want daemon d1, epoch 7 and a growing seq (all: %+v)", i, st, stamps)
		}
	}
}

// TestUplinkRestartClearsConflicts: a daemon stopped while it reported c1
// and started again, with the same id and no conflicts, clears c1 on serve.
// Both workstreams stay in serve's state, so only the new report can.
func TestUplinkRestartClearsConflicts(t *testing.T) {
	rec := &recorder{}
	s := serve.New()
	s.SetLogger(slog.New(rec))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { closeServer(srv) })
	postState(t, srv, "d2", "d2:b")
	listed := func() []core.Conflict { return warnings(t, srv).Conflicts }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	first := &Uplink{Server: srv.URL, Daemon: "d1", Resend: time.Hour, Snapshot: fixedState, OnWarnings: func([]core.Warning) {}, Log: quiet}
	go func() { first.Run(ctx); close(done) }()
	first.SetConflicts([]core.Conflict{conflictFixture()})
	eventually(t, 3*time.Second, "serve lists c1", func() bool { cs := listed(); return len(cs) == 1 && cs[0].ID == "c1" })
	cancel()
	<-done

	second := runUplink(t, srv.URL, time.Hour, func([]core.Warning) {})
	second.SetConflicts(nil)
	eventually(t, 3*time.Second, "the restarted daemon's empty set clears c1", func() bool { return len(listed()) == 0 })
	if _, ok := rec.find("conflict resolved", map[string]string{"id": "c1"}); !ok {
		t.Error("serve did not log c1 resolved")
	}
}

func TestUplinkOnConnect(t *testing.T) {
	s := serve.New()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { closeServer(srv) })
	var n atomic.Int32
	startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Resend: time.Hour, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, OnConnect: func() { n.Add(1) }, Log: quiet})
	eventually(t, 3*time.Second, "first connect", func() bool { return n.Load() == 1 })
	srv.CloseClientConnections()
	eventually(t, 5*time.Second, "connect after the cut", func() bool { return n.Load() >= 2 })
}

// TestUplinkStampsEpochAndSeq: each run of a daemon takes the next epoch of
// its state dir, and every full state it posts carries it with a growing
// seq. A restarted daemon's seq starts over, and serve still takes its state.
func TestUplinkStampsEpochAndSeq(t *testing.T) {
	dir := shortDir(t)
	floor := uint64(time.Now().UnixMilli())
	var epochs [2]uint64
	for i := range epochs {
		d, err := New(Config{StateDir: dir, Log: quiet})
		if err != nil {
			t.Fatal(err)
		}
		epochs[i] = d.uplink.Epoch
	}
	if epochs[0] < floor || epochs[1] <= epochs[0] {
		t.Fatalf("epochs of two runs = %v, want a growing pair from the clock's floor %d on", epochs, floor)
	}

	srv, posted := fullStatesPosted(t)
	postsOf := func(epoch uint64) []core.FullState {
		var out []core.FullState
		for _, p := range posted() {
			if p.Epoch == epoch {
				out = append(out, p)
			}
		}
		return out
	}

	var runs [2][]core.FullState
	for i, epoch := range epochs {
		file := []string{"x.go", "y.go"}[i]
		snap := func() core.FullState { st := fixedState(); st.Workstreams[0].Files = []string{file}; return st }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		u := &Uplink{Server: srv.URL, Daemon: "d1", Epoch: epoch, Resend: 50 * time.Millisecond, Snapshot: snap,
			OnWarnings: func([]core.Warning) {}, Log: quiet}
		go func() { u.Run(ctx); close(done) }()
		eventually(t, 3*time.Second, "serve holds the run's state, posted a few times", func() bool {
			ws := warnings(t, srv).Workstreams
			return len(ws) == 1 && slices.Equal(ws[0].Files, []string{file}) && len(postsOf(epoch)) >= 3
		})
		cancel()
		<-done
		runs[i] = postsOf(epoch)
		for j, p := range runs[i] {
			if p.Daemon != "d1" || (j > 0 && p.Seq <= runs[i][j-1].Seq) {
				t.Fatalf("run %d, post %d: daemon %q seq %d after %d, want d1 and a growing seq", i, j, p.Daemon, p.Seq, runs[i][max(j-1, 0)].Seq)
			}
		}
	}
	if first, again := runs[0][len(runs[0])-1].Seq, runs[1][0].Seq; again >= first {
		t.Fatalf("restarted run's first seq %d, want it below the first run's last %d: the seq starts over", again, first)
	}
	if n, all := len(runs[0])+len(runs[1]), len(posted()); n != all {
		t.Fatalf("%d of %d full states carry neither run's epoch", all-n, all)
	}
}

// TestUplinkOnePostPerWake: the first connect hands the outbox the state and
// the connect, and each sends it; serve gets it once.
func TestUplinkOnePostPerWake(t *testing.T) {
	srv, posted := fullStatesPosted(t)
	runUplink(t, srv.URL, time.Hour, func([]core.Warning) {})
	eventually(t, 3*time.Second, "full state arrived", func() bool { return len(warnings(t, srv).Workstreams) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(posted()); n != 1 {
		t.Fatalf("%d full states posted on the first connect, want 1", n)
	}
}

// fullStatesPosted is a real serve that records every full state posted to
// it, in order.
func fullStatesPosted(t *testing.T) (*httptest.Server, func() []core.FullState) {
	t.Helper()
	s := serve.New()
	var mu sync.Mutex
	var posted []core.FullState
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/msg" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			var m wire.Message
			if json.Unmarshal(b, &m) == nil && m.Type == wire.TypeFullState {
				mu.Lock()
				posted = append(posted, m.FullState)
				mu.Unlock()
			}
		}
		s.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv, func() []core.FullState {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(posted)
	}
}

// TestUplinkOnControl: the uplink hands each control event of the stream to
// OnControl, the current one first, and the changes serve pushes after.
func TestUplinkOnControl(t *testing.T) {
	dir := t.TempDir()
	st := serve.Stored{Version: 1, AdminHash: serve.HashToken("dgt_adm"), Users: []serve.User{{Name: "ana", TokenHash: serve.HashToken("dgt_ana")}}}
	path := dir + "/serve.json"
	if err := serve.SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	s := serve.New()
	s.UseState(path, st)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { closeServer(srv) })
	var mu sync.Mutex
	var got []bool
	startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Token: "dgt_ana", Resend: time.Hour, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, OnControl: func(c wire.Control) { mu.Lock(); got = append(got, c.InjectionOff); mu.Unlock() }, Log: quiet})
	seen := func(want ...bool) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			return slices.Equal(got, want)
		}
	}
	eventually(t, 3*time.Second, "control on connect", seen(false))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/admin/pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("Authorization", "Bearer dgt_adm")
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause: %v %v", resp, err)
	}
	eventually(t, 3*time.Second, "control after the pause", seen(false, true))
}

// protoServer is a serve stand-in that answers every request with the
// protocol header theirs (none when empty) and keeps the protocol_version of
// each message it takes.
func protoServer(t *testing.T, theirs string) (srv *httptest.Server, versions func() []int) {
	t.Helper()
	var mu sync.Mutex
	var got []int
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if theirs != "" {
			w.Header().Set(wire.HeaderProtocol, theirs)
		}
		switch {
		case r.URL.Path == "/v1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.URL.Path == "/v1/msg":
			var m wire.Message
			json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			got = append(got, m.ProtocolVersion)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// TestProtocolMismatchLoggedOnce (daemon side): every message carries our
// version, and a serve on another one is logged once, however many POSTs,
// snapshot calls and stream connects see it.
func TestProtocolMismatchLoggedOnce(t *testing.T) {
	srv, versions := protoServer(t, strconv.Itoa(wire.ProtocolVersion+1))
	rec := &recorder{}
	u := startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Resend: 30 * time.Millisecond, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: slog.New(rec)})
	eventually(t, 3*time.Second, "several posts", func() bool { return len(versions()) >= 3 })
	snap := snapshot.Snap{Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40)}
	u.PutSnapshot(context.Background(), "d1:a", snap, []byte("x"))
	u.GetSnapshot(context.Background(), "d1:a", "")
	for _, v := range versions() {
		if v != wire.ProtocolVersion {
			t.Fatalf("a message went with protocol_version %d, want %d", v, wire.ProtocolVersion)
		}
	}
	_, ok := rec.find("protocol mismatch", map[string]string{"ours": strconv.Itoa(wire.ProtocolVersion), "theirs": strconv.Itoa(wire.ProtocolVersion + 1), "level": "WARN"})
	if !ok {
		t.Fatalf("no protocol mismatch warning, got %v", rec.msgs)
	}
	if n := rec.count("protocol mismatch"); n != 1 {
		t.Fatalf("logged %d times, want once", n)
	}
}

// TestProtocolAbsentNotLogged: an older serve sends no header, and a 0 header
// means the same; neither is a mismatch. The match is silent too.
func TestProtocolAbsentNotLogged(t *testing.T) {
	for _, theirs := range []string{"", "0", strconv.Itoa(wire.ProtocolVersion)} {
		t.Run("header="+theirs, func(t *testing.T) {
			srv, versions := protoServer(t, theirs)
			rec := &recorder{}
			u := startUplink(t, &Uplink{Server: srv.URL, Daemon: "d1", Resend: 30 * time.Millisecond, Snapshot: fixedState,
				OnWarnings: func([]core.Warning) {}, Log: slog.New(rec)})
			eventually(t, 3*time.Second, "several posts", func() bool { return len(versions()) >= 3 })
			u.GetSnapshot(context.Background(), "d1:a", "")
			if n := rec.count("protocol mismatch"); n != 0 {
				t.Fatalf("logged %d mismatches, want none", n)
			}
		})
	}
}

// TestUplinkState: State gets serve's whole state with the token, under the
// caller's context; a status other than 200 is a StatusError.
func TestUplinkState(t *testing.T) {
	office := serve.New()
	office.SetLogger(quiet)
	office.SetToken("t")
	srv := httptest.NewServer(office.Handler())
	t.Cleanup(func() { closeServer(srv) })
	postState(t, srv, "d1", "d1:a")
	u := &Uplink{Server: srv.URL, Daemon: "d0", Token: "t", Log: quiet}
	dump, err := u.State(context.Background())
	if err != nil || len(dump.Workstreams) != 1 || dump.Workstreams[0].ID != "d1:a" {
		t.Fatalf("State = %+v, %v, want d1:a", dump, err)
	}
	u = &Uplink{Server: srv.URL, Daemon: "d0", Token: "wrong", Log: quiet}
	var se *StatusError
	if _, err := u.State(context.Background()); !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("State with a wrong token = %v, want a 401 StatusError", err)
	}

	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(func() { closeServer(hung) })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (&Uplink{Server: hung.URL, Daemon: "d0", Log: quiet}).State(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("State from a hung serve = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("State from a hung serve took %s, want about the context's 200ms", elapsed)
	}
}

// TestUplinkConnected: true while the warnings stream is established, false
// before it and after serve goes away.
func TestUplinkConnected(t *testing.T) {
	srv, _ := protoServer(t, "")
	u := runUplink(t, srv.URL, time.Hour, func([]core.Warning) {})
	eventually(t, 5*time.Second, "connected", u.Connected)
	srv.CloseClientConnections()
	srv.Close()
	eventually(t, 5*time.Second, "disconnected", func() bool { return !u.Connected() })
}

func TestResendJitter(t *testing.T) {
	const resend = 10 * time.Second
	for _, tc := range []struct {
		f    float64
		want time.Duration
	}{
		{0, 8 * time.Second},
		{0.5, 10 * time.Second},
		{0.999999999, 12 * time.Second},
	} {
		u := &Uplink{jitter: func() float64 { return tc.f }}
		got := jittered(resend, u.jitterFactor())
		if diff := got - tc.want; diff < -time.Millisecond || diff > time.Millisecond {
			t.Errorf("f=%v: wait %v, want ~%v", tc.f, got, tc.want)
		}
	}
	if got := jittered(0, 0.7); got != 0 {
		t.Errorf("zero Resend jittered to %v", got)
	}
	if got := jittered(-time.Second, 0.7); got != 0 {
		t.Errorf("negative duration jittered to %v", got)
	}
	// Nil source: the default stays inside the range.
	for range 100 {
		if f := (&Uplink{}).jitterFactor(); f < 0 || f >= 1 {
			t.Fatalf("default source returned %v", f)
		}
	}
}

func TestBackoffJitterCapped(t *testing.T) {
	u := &Uplink{jitter: func() float64 { return 0.999999999 }}
	backoff := minBackoff
	for range 20 {
		if w := u.backoffWait(backoff); w > maxBackoff {
			t.Fatalf("backoff %v waited %v, above maxBackoff", backoff, w)
		}
		backoff = min(2*backoff, maxBackoff)
	}
	if w := (&Uplink{jitter: func() float64 { return 0 }}).backoffWait(minBackoff); w != minBackoff*8/10 {
		t.Errorf("low end waited %v, want %v", w, minBackoff*8/10)
	}
}

// TestUplinkEscalation: an escalation one uplink sends reaches the other
// daemon's OnEscalation through serve, and the sender learns it got there; to
// a daemon not connected it says it did not.
func TestUplinkEscalation(t *testing.T) {
	srv := tokenServer(t, "t")
	postState(t, srv, "d1", "d1:a")
	postState(t, srv, "d2", "d2:b")
	e := wire.Escalation{Conflict: "c1", Repo: "r", Path: "x.go",
		From: core.Side{Daemon: "d1", Workstream: "d1:a"}, To: core.Side{Daemon: "d2", Workstream: "d2:b"}, Reason: "which cap stays?"}
	u1 := &Uplink{Server: srv.URL, Daemon: "d1", Token: "t", Log: quiet}
	ctx := context.Background()
	if delivered, err := u1.Escalate(ctx, e); err != nil || delivered {
		t.Fatalf("to a daemon not connected: delivered %v, err %v; want false, nil", delivered, err)
	}
	got := make(chan wire.Escalation, 1)
	d2State := func() core.FullState {
		return core.FullState{Daemon: "d2", Workstreams: []core.Workstream{{ID: "d2:b", Repo: "r", Branch: "d2", Files: []string{"x.go"}}}}
	}
	u2 := startUplink(t, &Uplink{Server: srv.URL, Daemon: "d2", Token: "t", Resend: time.Hour, Snapshot: d2State,
		OnWarnings: func([]core.Warning) {}, OnEscalation: func(e wire.Escalation) { got <- e }, Log: quiet})
	eventually(t, 3*time.Second, "d2's stream connected", u2.Connected)
	if delivered, err := u1.Escalate(ctx, e); err != nil || !delivered {
		t.Fatalf("to d2: delivered %v, err %v; want true, nil", delivered, err)
	}
	select {
	case r := <-got:
		if r.Reason != "which cap stays?" || r.From.Workstream != "d1:a" {
			t.Fatalf("d2 got %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("d2 got no escalation within 3s")
	}
}

// TestUplinkEscalationOldServe: a serve that predates escalations answers
// 204, as for any type it does not know: the sender reads that as not
// relayed, never as delivered.
func TestUplinkEscalationOldServe(t *testing.T) {
	srv, _ := protoServer(t, "")
	u := &Uplink{Server: srv.URL, Daemon: "d1", Log: quiet}
	if delivered, err := u.Escalate(context.Background(), wire.Escalation{}); !errors.Is(err, ErrNotRelayed) || delivered {
		t.Fatalf("old serve: delivered %v, err %v; want false, ErrNotRelayed", delivered, err)
	}
}
