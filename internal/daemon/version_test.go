package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	tooOldMsg   = "Dugout: your dugout is older than the server; update it (dugout doctor tells you how)."
	newerMsg    = "Dugout: the server is newer than your dugout; update it when you can."
	tooOldBody  = "your dugout is older than this server accepts: update it"
	notePauseMs = "Dugout is paused on this machine."
)

// versionServe is a serve stand-in: POST /v1/msg answers 204, or 426 once
// refuse is set, with the protocol header header; the stream stays open.
type versionServe struct {
	srv    *httptest.Server
	refuse atomic.Bool
	header atomic.Int32 // the protocol serve answers with
	msgs   atomic.Int32 // POST /v1/msg received
}

func startVersionServe(t *testing.T) *versionServe {
	t.Helper()
	v := &versionServe{}
	v.header.Store(wire.ProtocolVersion)
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.HeaderProtocol, strconv.Itoa(int(v.header.Load())))
		switch r.URL.Path {
		case "/v1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/v1/msg":
			v.msgs.Add(1)
			if v.refuse.Load() {
				http.Error(w, tooOldBody, http.StatusUpgradeRequired)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { closeServer(v.srv) })
	return v
}

func (v *versionServe) daemon(t *testing.T) running {
	t.Helper()
	dir := shortDir(t)
	d, err := New(Config{StateDir: dir, Server: v.srv.URL, Poll: 200 * time.Millisecond, Resend: 50 * time.Millisecond, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r := running{d: d, dir: dir, sock: statedir.Socket(dir)}
	eventually(t, 2*time.Second, "socket accepts", func() bool {
		c, err := net.Dial("unix", r.sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return r
}

func noticeOf(t *testing.T, out string) string {
	t.Helper()
	var o struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return o.SystemMessage
}

// TestDaemonTooOldNotice: a 426 stops the uplink from sending, and each
// session is told once, on its next hook.
func TestDaemonTooOldNotice(t *testing.T) {
	f, wt := overlap(t)
	v := startVersionServe(t)
	r := v.daemon(t)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 3*time.Second, "serve takes a message", func() bool { return v.msgs.Load() > 0 })
	if out := hook(t, r.sock, "UserPromptSubmit", "sA", f.Clone, "", ""); strings.Contains(out, tooOldMsg) {
		t.Fatalf("hook before the 426 = %s, want no notice", out)
	}
	v.refuse.Store(true)
	eventually(t, 3*time.Second, "the daemon sees the 426", func() bool { return r.d.uplink.TooOld() })
	seen := v.msgs.Load()
	time.Sleep(400 * time.Millisecond) // several resend ticks
	if n := v.msgs.Load(); n != seen {
		t.Fatalf("the uplink sent %d messages after the 426, want none", n-seen)
	}
	for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
		if out := hook(t, r.sock, "UserPromptSubmit", s.id, s.cwd, "", ""); !strings.Contains(noticeOf(t, out), tooOldMsg) {
			t.Fatalf("first hook of %s = %s, want the too-old notice", s.id, out)
		}
		if out := hook(t, r.sock, "UserPromptSubmit", s.id, s.cwd, "", ""); strings.Contains(out, tooOldMsg) {
			t.Fatalf("second hook of %s = %s, want no notice", s.id, out)
		}
	}
}

// TestUplinkTooOldSendsNothing: after a 426, no resend goes while the
// daemon is too old.
func TestUplinkTooOldSendsNothing(t *testing.T) {
	v := startVersionServe(t)
	v.refuse.Store(true)
	u := startUplink(t, &Uplink{Server: v.srv.URL, Daemon: "d1", Resend: 30 * time.Millisecond, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: quiet})
	eventually(t, 3*time.Second, "the uplink sees the 426", func() bool { return u.TooOld() })
	seen := v.msgs.Load()
	u.Notify()
	time.Sleep(300 * time.Millisecond)
	if n := v.msgs.Load(); n != seen {
		t.Fatalf("%d messages after the 426, want none", n-seen)
	}
}

// TestServerNewerNotice: a serve on a newer protocol is told once per
// session, never during a pause (the pause text wins).
func TestServerNewerNotice(t *testing.T) {
	f, wt := overlap(t)
	v := startVersionServe(t)
	r := v.daemon(t)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 3*time.Second, "serve takes a message", func() bool { return v.msgs.Load() > 0 })
	v.header.Store(wire.ProtocolVersion + 1)
	eventually(t, 3*time.Second, "the daemon sees the newer serve", func() bool { return r.d.uplink.ServerNewer() })
	setLocalPause(t, r.dir, true)
	if out := hook(t, r.sock, "UserPromptSubmit", "sA", f.Clone, "", ""); noticeOf(t, out) != notePauseMs {
		t.Fatalf("hook during the pause = %s, want the pause text", out)
	}
	setLocalPause(t, r.dir, false)
	for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
		if out := hook(t, r.sock, "UserPromptSubmit", s.id, s.cwd, "", ""); !strings.Contains(noticeOf(t, out), newerMsg) {
			t.Fatalf("first hook of %s = %s, want the newer-server notice", s.id, out)
		}
		if out := hook(t, r.sock, "UserPromptSubmit", s.id, s.cwd, "", ""); strings.Contains(out, newerMsg) {
			t.Fatalf("second hook of %s = %s, want no notice", s.id, out)
		}
	}
	// Everything keeps working: serve took the messages.
	if v.msgs.Load() == 0 {
		t.Fatal("the uplink sent nothing to a newer serve")
	}
}

// TestTooOldSkipsNewerNotice: a serve that refuses this dugout (426) and
// speaks a newer protocol says one thing, not two: the too-old notice only.
func TestTooOldSkipsNewerNotice(t *testing.T) {
	f, _ := overlap(t)
	v := startVersionServe(t)
	r := v.daemon(t)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	eventually(t, 3*time.Second, "serve takes a message", func() bool { return v.msgs.Load() > 0 })
	v.header.Store(wire.ProtocolVersion + 1)
	eventually(t, 3*time.Second, "the daemon sees the newer serve", func() bool { return r.d.uplink.ServerNewer() })
	v.refuse.Store(true)
	eventually(t, 3*time.Second, "the daemon sees the 426", func() bool { return r.d.uplink.TooOld() })
	out := hook(t, r.sock, "UserPromptSubmit", "sA", f.Clone, "", "")
	if msg := noticeOf(t, out); !strings.Contains(msg, tooOldMsg) || strings.Contains(msg, newerMsg) {
		t.Fatalf("notice = %q, want the too-old one only", msg)
	}
}
