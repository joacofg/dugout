package serve

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

var (
	commitC = strings.Repeat("c", 40)
	baseB   = strings.Repeat("b", 40)
)

// snapServer has d1/ws1 and d2/ws2 sharing a.go, and d3/ws3 sharing nothing.
func snapServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := New()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	postState(t, srv, "d1", w("ws1", "a.go"))
	postState(t, srv, "d2", w("ws2", "a.go"))
	postState(t, srv, "d3", w("ws3", "z.go"))
	return s, srv
}

func snapURL(srv *httptest.Server, ws, d string) string {
	return srv.URL + "/v1/snapshot?ws=" + ws + "&daemon_id=" + d
}

func upload(t *testing.T, srv *httptest.Server, ws, d, commit, base string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, snapURL(srv, ws, d), bytes.NewReader(body))
	req.Header.Set("Content-Type", wire.BundleType)
	req.Header.Set(wire.HeaderCommit, commit)
	req.Header.Set(wire.HeaderBase, base)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func download(t *testing.T, srv *httptest.Server, ws, d, ifNoneMatch string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, snapURL(srv, ws, d), nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestSnapshotRoundTrip(t *testing.T) {
	_, srv := snapServer(t)
	if code := upload(t, srv, "ws1", "d1", commitC, baseB, []byte("bundle")); code != http.StatusNoContent {
		t.Fatalf("POST: status %d", code)
	}
	resp, body := download(t, srv, "ws1", "d2", "")
	if resp.StatusCode != http.StatusOK || string(body) != "bundle" {
		t.Fatalf("GET: status %d body %q", resp.StatusCode, body)
	}
	if resp.Header.Get(wire.HeaderCommit) != commitC || resp.Header.Get(wire.HeaderBase) != baseB ||
		resp.Header.Get("ETag") != `"`+commitC+`"` {
		t.Fatalf("GET headers: %v", resp.Header)
	}
	resp, body = download(t, srv, "ws1", "d2", `"`+commitC+`"`)
	if resp.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("conditional GET: status %d body %q", resp.StatusCode, body)
	}
}

func TestSnapshotOnlyForOverlaps(t *testing.T) {
	_, srv := snapServer(t)
	upload(t, srv, "ws1", "d1", commitC, baseB, []byte("bundle"))
	if resp, _ := download(t, srv, "ws1", "d3", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-overlapping daemon: status %d, want 404", resp.StatusCode)
	}
	if resp, _ := download(t, srv, "ws2", "d1", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no bundle yet: status %d, want 404", resp.StatusCode)
	}
}

func TestSnapshotUploaderMustOwn(t *testing.T) {
	_, srv := snapServer(t)
	if code := upload(t, srv, "ws1", "d2", commitC, baseB, []byte("x")); code != http.StatusConflict {
		t.Fatalf("foreign workstream: status %d, want 409", code)
	}
	if code := upload(t, srv, "nope", "d1", commitC, baseB, []byte("x")); code != http.StatusConflict {
		t.Fatalf("unknown workstream: status %d, want 409", code)
	}
}

func TestSnapshotBadHeaders(t *testing.T) {
	_, srv := snapServer(t)
	for _, c := range []struct{ commit, base string }{
		{"xyz", baseB}, {"", baseB}, {commitC, strings.Repeat("B", 40)}, {commitC, ""},
	} {
		if code := upload(t, srv, "ws1", "d1", c.commit, c.base, []byte("x")); code != http.StatusBadRequest {
			t.Errorf("commit %q base %q: status %d, want 400", c.commit, c.base, code)
		}
	}
}

func TestSnapshotTooLarge(t *testing.T) {
	_, srv := snapServer(t)
	if code := upload(t, srv, "ws1", "d1", commitC, baseB, make([]byte, wire.MaxBundle+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", code)
	}
}

// zeros yields left zero bytes and counts how many were read.
type zeros struct {
	left int64
	read atomic.Int64
}

func (z *zeros) Read(p []byte) (int, error) {
	if z.left == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.left))
	clear(p[:n])
	z.left -= int64(n)
	z.read.Add(int64(n))
	return n, nil
}

// uploadAnnounced uploads n bytes for ws as d with their Content-Length and
// Expect: 100-continue, so the body only leaves once serve starts reading it.
// It returns the status and how many bytes of the body left.
func uploadAnnounced(t *testing.T, srv *httptest.Server, ws, d string, n int64) (status int, sent int64) {
	t.Helper()
	body := &zeros{left: n}
	req, err := http.NewRequest(http.MethodPost, snapURL(srv, ws, d), body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", wire.BundleType)
	req.Header.Set(wire.HeaderCommit, commitC)
	req.Header.Set(wire.HeaderBase, baseB)
	req.Header.Set("Expect", "100-continue")
	tr := &http.Transport{ExpectContinueTimeout: time.Minute}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, body.read.Load()
}

// TestSnapshotRefusedBeforeReading: a Content-Length over the bundle limit,
// or one that would fill the storage, is refused before serve reads a byte.
func TestSnapshotRefusedBeforeReading(t *testing.T) {
	s, srv := snapServer(t)
	if code, sent := uploadAnnounced(t, srv, "ws1", "d1", wire.MaxBundle+1); code != http.StatusRequestEntityTooLarge || sent != 0 {
		t.Fatalf("over the bundle limit: status %d after %d body bytes, want 413 before any", code, sent)
	}
	s.SetSnapshotLimit(10)
	if code := upload(t, srv, "ws1", "d1", commitC, baseB, make([]byte, 8)); code != http.StatusNoContent {
		t.Fatalf("first: status %d", code)
	}
	if code, sent := uploadAnnounced(t, srv, "ws2", "d2", 8); code != http.StatusInsufficientStorage || sent != 0 {
		t.Fatalf("over the total limit: status %d after %d body bytes, want 507 before any", code, sent)
	}
	// The bundle it replaces does not count: ws1's 9 bytes fit.
	if code, sent := uploadAnnounced(t, srv, "ws1", "d1", 9); code != http.StatusNoContent || sent != 9 {
		t.Fatalf("replacement: status %d after %d body bytes, want 204 after 9", code, sent)
	}
}

func TestSnapshotTotalLimit(t *testing.T) {
	s, srv := snapServer(t)
	s.SetSnapshotLimit(10)
	if code := upload(t, srv, "ws1", "d1", commitC, baseB, make([]byte, 8)); code != http.StatusNoContent {
		t.Fatalf("first: status %d", code)
	}
	if code := upload(t, srv, "ws2", "d2", commitC, baseB, make([]byte, 8)); code != http.StatusInsufficientStorage {
		t.Fatalf("second: status %d, want 507", code)
	}
	if code := upload(t, srv, "ws1", "d1", commitC, baseB, make([]byte, 9)); code != http.StatusNoContent {
		t.Fatalf("replacement: status %d", code)
	}
}

func TestSnapshotContentType(t *testing.T) {
	_, srv := snapServer(t)
	resp, err := http.Post(snapURL(srv, "ws1", "d1"), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("snapshot with json: status %d, want 415", resp.StatusCode)
	}
	resp, err = http.Post(srv.URL+"/v1/msg", wire.BundleType, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("msg with bundle type: status %d, want 415", resp.StatusCode)
	}
}

func TestSnapshotDroppedWithWorkstream(t *testing.T) {
	s, srv := snapServer(t)
	upload(t, srv, "ws1", "d1", commitC, baseB, []byte("bundle"))
	postState(t, srv, "d1")
	if resp, _ := download(t, srv, "ws1", "d2", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	s.mu.Lock()
	n := s.snapBytes
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("snapBytes = %d, want 0", n)
	}
}

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestConflictsDroppedWithWorkstream: a full_state that drops a workstream
// drops every reported conflict with that workstream on a side, whoever
// reported it, and logs it resolved; conflicts between workstreams still
// there stay.
func TestConflictsDroppedWithWorkstream(t *testing.T) {
	s := New()
	logs := &lockedBuf{}
	s.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	postState(t, srv, "d1", w("ws1", "a.go"))
	postState(t, srv, "d2", w("ws2", "a.go"))
	postState(t, srv, "d3", w("ws3", "a.go"))
	conflict := func(id string, a, b core.Side) core.Conflict {
		return core.Conflict{ID: id, Repo: "r", Path: "a.go", Kind: "content", Sides: [2]core.Side{a, b}}
	}
	s1, s2, s3 := core.Side{Daemon: "d1", Workstream: "ws1"}, core.Side{Daemon: "d2", Workstream: "ws2"}, core.Side{Daemon: "d3", Workstream: "ws3"}
	c1, c2 := conflict("c1", s1, s2), conflict("c2", s2, s3)
	send := func(d core.DaemonID, cs ...core.Conflict) {
		t.Helper()
		b, _ := json.Marshal(wire.Message{Type: wire.TypeConflicts, FullState: core.FullState{Daemon: d}, Conflicts: cs})
		if code := post(t, srv, string(b)); code != http.StatusNoContent {
			t.Fatalf("conflicts: status %d", code)
		}
	}
	send("d1", c1)
	send("d2", c1, c2)
	send("d3", c2)

	postState(t, srv, "d1") // ws1 is gone; d1 never reports again
	if d := dump(t, srv); len(d.Conflicts) != 1 || d.Conflicts[0].ID != "c2" {
		t.Fatalf("state conflicts = %+v, want only c2", d.Conflicts)
	}
	if n := strings.Count(logs.String(), `msg="conflict resolved" id=c1`); n != 1 {
		t.Fatalf("c1 resolved logged %d times, want 1:\n%s", n, logs)
	}
	s.mu.Lock()
	_, kept := s.reported["d1"]
	s.mu.Unlock()
	if kept {
		t.Error("d1's report outlived the conflict it held")
	}
}

func TestConflictsMessage(t *testing.T) {
	s := New()
	logs := &lockedBuf{}
	s.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	c1 := core.Conflict{
		ID: "c1", Repo: "r", Path: "a.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "ws1"}, {Daemon: "d2", Workstream: "ws2"}},
	}
	send := func(d core.DaemonID, cs ...core.Conflict) {
		t.Helper()
		b, _ := json.Marshal(wire.Message{Type: wire.TypeConflicts, FullState: core.FullState{Daemon: d}, Conflicts: cs})
		if code := post(t, srv, string(b)); code != http.StatusNoContent {
			t.Fatalf("conflicts: status %d", code)
		}
	}
	count := func(msg string) int {
		return strings.Count(logs.String(), "msg=\""+msg+"\"") + strings.Count(logs.String(), "msg="+msg+" ")
	}

	send("d1", c1)
	if n := count("conflict detected"); n != 1 {
		t.Fatalf("detected %d times, want 1:\n%s", n, logs)
	}
	for _, want := range []string{"id=c1", "path=a.go", "daemons=d1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	send("d2", c1)
	if n := count("conflict detected"); n != 1 {
		t.Fatalf("detected %d times after d2, want 1:\n%s", n, logs)
	}
	if d := dump(t, srv); len(d.Conflicts) != 1 || d.Conflicts[0].ID != "c1" {
		t.Fatalf("state conflicts = %+v", d.Conflicts)
	}
	send("d1")
	if n := count("conflict resolved"); n != 0 {
		t.Fatalf("resolved while d2 still reports it")
	}
	send("d2")
	if n := count("conflict resolved"); n != 1 {
		t.Fatalf("resolved %d times, want 1:\n%s", n, logs)
	}
	if d := dump(t, srv); d.Conflicts == nil || len(d.Conflicts) != 0 {
		t.Fatalf("state conflicts = %#v, want empty non-nil", d.Conflicts)
	}
}

// TestConflictsStaleIgnored: a conflicts message older than the last one
// taken from its daemon, by seq or by epoch, leaves the listed conflicts as
// they were and logs no resolved or detected event.
func TestConflictsStaleIgnored(t *testing.T) {
	s := New()
	logs := &lockedBuf{}
	s.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	postState(t, srv, "d1", w("ws1", "a.go"))
	postState(t, srv, "d2", w("ws2", "a.go"))
	c1 := core.Conflict{
		ID: "c1", Repo: "r", Path: "a.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "ws1"}, {Daemon: "d2", Workstream: "ws2"}},
	}
	send := func(epoch, seq uint64, cs ...core.Conflict) {
		t.Helper()
		b, _ := json.Marshal(wire.Message{Type: wire.TypeConflicts, FullState: core.FullState{Daemon: "d1", Epoch: epoch, Seq: seq}, Conflicts: cs})
		if code := post(t, srv, string(b)); code != http.StatusNoContent {
			t.Fatalf("conflicts: status %d", code)
		}
	}
	listed := func() int { return len(dump(t, srv).Conflicts) }
	count := func(msg string) int { return strings.Count(logs.String(), `msg="`+msg+`"`) }

	send(1, 5, c1)
	send(1, 4)
	send(1, 5)
	if n := listed(); n != 1 || count("conflict resolved") != 0 {
		t.Fatalf("a stale set replaced c1: %d listed\n%s", n, logs)
	}
	send(2, 1)
	if n := listed(); n != 0 || count("conflict resolved") != 1 {
		t.Fatalf("a new epoch's empty set did not clear c1: %d listed\n%s", n, logs)
	}
	send(1, 6, c1)
	if n := listed(); n != 0 || count("conflict detected") != 1 {
		t.Fatalf("an older epoch brought c1 back: %d listed\n%s", n, logs)
	}
}
