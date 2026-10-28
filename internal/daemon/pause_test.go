package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	pauseAdmin = "dgt_pause_admin"
	pauseUser  = "dgt_pause_user"
	pauseMsg   = `{"systemMessage":"Dugout is paused for the whole team."}`
)

// pausable is a serve with a state file, one person (ana) and an admin, and
// a daemon that is ana's. stateCalls counts serve's GET /v1/state.
type pausable struct {
	srv        *httptest.Server
	run        running
	stateCalls *atomic.Int32
}

func startPausable(t *testing.T) pausable {
	t.Helper()
	path := filepath.Join(t.TempDir(), "serve.json")
	st := serve.Stored{Version: 1, AdminHash: serve.HashToken(pauseAdmin),
		Users: []serve.User{{Name: "ana", TokenHash: serve.HashToken(pauseUser)}}}
	if err := serve.SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	s := serve.New()
	s.SetLogger(quiet)
	s.UseState(path, st)
	h, calls := s.Handler(), &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/state" {
			calls.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	dir := shortDir(t)
	d, err := New(Config{StateDir: dir, Server: srv.URL, Token: pauseUser, Poll: 200 * time.Millisecond, Resend: time.Second, Log: quiet})
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
	return pausable{srv, r, calls}
}

// setPause asks serve to pause or resume the team and waits for the daemon
// to take it.
func (p pausable) setPause(t *testing.T, paused bool) {
	t.Helper()
	body := `{"paused":false}`
	if paused {
		body = `{"paused":true}`
	}
	req, _ := http.NewRequest(http.MethodPost, p.srv.URL+"/v1/admin/pause", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+pauseAdmin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause: status %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "daemon takes the pause", func() bool { return p.run.d.paused.Load() == paused })
}

// TestPausedHooksAnswerEmpty: with a conflict live, a paused daemon hands
// out neither context nor deny; when the team resumes, the conflict that
// grew meanwhile is told at the next hook and denies the edit once.
func TestPausedHooksAnswerEmpty(t *testing.T) {
	f, wt := overlap(t)
	p := startPausable(t)
	sock := p.run.sock
	hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 15*time.Second, "the detector finds the conflict", func() bool {
		p.run.d.mu.Lock()
		defer p.run.d.mu.Unlock()
		return len(p.run.d.conflicts) > 0
	})
	p.setPause(t, true)
	edit := filepath.Join(f.Clone, "shared.go")
	for i, step := range []struct{ event, tool, file string }{
		{"PostToolUse", "", ""}, {"UserPromptSubmit", "", ""}, {"PreToolUse", "Edit", edit}, {"PreToolUse", "Edit", edit},
	} {
		out := hook(t, sock, step.event, "sA", f.Clone, step.tool, step.file)
		if i == 0 && out != pauseMsg {
			t.Fatalf("first hook of the pause = %s", out)
		}
		if i > 0 && out != "{}" {
			t.Fatalf("hook %d during the pause = %s, want {}", i, out)
		}
		if strings.Contains(out, "deny") || strings.Contains(out, "dugout-context") {
			t.Fatalf("hook %d during the pause carries more than the notice: %s", i, out)
		}
	}
	p.setPause(t, false)
	if out := hook(t, sock, "PreToolUse", "sA", f.Clone, "Edit", edit); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, `"systemMessage":"Dugout: shared.go conflicts`) {
		t.Fatalf("first Edit after the pause = %s, want deny and the conflict told", out)
	}
	if out := hook(t, sock, "PreToolUse", "sA", f.Clone, "Edit", edit); out != "{}" {
		t.Fatalf("retried Edit = %s, want {}", out)
	}
}

// TestPauseSystemMessageOncePerSession: each session is told once per pause,
// and again in the next one.
func TestPauseSystemMessageOncePerSession(t *testing.T) {
	f, wt := overlap(t)
	p := startPausable(t)
	sock := p.run.sock
	hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, sock, "SessionStart", "sB", wt, "", "")
	for round := 1; round <= 2; round++ {
		p.setPause(t, true)
		for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
			if out := hook(t, sock, "UserPromptSubmit", s.id, s.cwd, "", ""); out != pauseMsg {
				t.Fatalf("round %d: first hook of %s = %s", round, s.id, out)
			}
			if out := hook(t, sock, "UserPromptSubmit", s.id, s.cwd, "", ""); out != "{}" {
				t.Fatalf("round %d: second hook of %s = %s, want {}", round, s.id, out)
			}
		}
		p.setPause(t, false)
	}
}

// TestPauseRepeatedControlIsNotANewPause: serve repeats the control on every
// reconnect, and that does not tell the sessions again.
func TestPauseRepeatedControlIsNotANewPause(t *testing.T) {
	f, _ := overlap(t)
	p := startPausable(t)
	sock := p.run.sock
	hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	p.setPause(t, true)
	if out := hook(t, sock, "UserPromptSubmit", "sA", f.Clone, "", ""); out != pauseMsg {
		t.Fatalf("first hook = %s", out)
	}
	p.run.d.setControl(wire.Control{InjectionOff: true})
	if out := hook(t, sock, "UserPromptSubmit", "sA", f.Clone, "", ""); out != "{}" {
		t.Fatalf("hook after a repeated control = %s, want {}", out)
	}
}

const localMsg = `{"systemMessage":"Dugout is paused on this machine."}`

// setLocalPause creates or removes the pause file, like dugout pause and
// resume do.
func setLocalPause(t *testing.T, dir string, on bool) {
	t.Helper()
	var err error
	if on {
		err = os.WriteFile(statedir.PausedPath(dir), nil, 0o600)
	} else {
		err = os.Remove(statedir.PausedPath(dir))
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestLocalPause: the pause file silences the hooks of this machine, with
// one notice per session and per pause, and no restart.
func TestLocalPause(t *testing.T) {
	f, wt := overlap(t)
	p := startPausable(t)
	sock := p.run.sock
	hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, sock, "SessionStart", "sB", wt, "", "")
	for round := 1; round <= 2; round++ {
		setLocalPause(t, p.run.dir, true)
		for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
			if out := hook(t, sock, "UserPromptSubmit", s.id, s.cwd, "", ""); out != localMsg {
				t.Fatalf("round %d: first hook of %s = %s", round, s.id, out)
			}
			if out := hook(t, sock, "PreToolUse", s.id, s.cwd, "Edit", filepath.Join(s.cwd, "shared.go")); out != "{}" {
				t.Fatalf("round %d: second hook of %s = %s, want {}", round, s.id, out)
			}
		}
		setLocalPause(t, p.run.dir, false)
		// The daemon sees the pause end at the next hook; a pause that
		// begins and ends between two hooks is not told.
		hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	}
	eventually(t, 15*time.Second, "the detector finds the conflict", func() bool {
		p.run.d.mu.Lock()
		defer p.run.d.mu.Unlock()
		return len(p.run.d.conflicts) > 0
	})
	if out := hook(t, sock, "PreToolUse", "sA", f.Clone, "Edit", filepath.Join(f.Clone, "shared.go")); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("first Edit after the local pause = %s, want deny", out)
	}
}

// TestLocalAndTeamPause: either pause is enough, the team's text wins, and
// the hooks come back only when both are lifted.
func TestLocalAndTeamPause(t *testing.T) {
	f, wt := overlap(t)
	p := startPausable(t)
	sock := p.run.sock
	hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 15*time.Second, "the detector finds the conflict", func() bool {
		p.run.d.mu.Lock()
		defer p.run.d.mu.Unlock()
		return len(p.run.d.conflicts) > 0
	})
	edit := filepath.Join(f.Clone, "shared.go")
	quiet := func(what string) {
		t.Helper()
		if out := hook(t, sock, "PreToolUse", "sA", f.Clone, "Edit", edit); out != "{}" {
			t.Fatalf("%s: hook = %s, want {}", what, out)
		}
	}
	setLocalPause(t, p.run.dir, true)
	if out := hook(t, sock, "UserPromptSubmit", "sB", wt, "", ""); out != localMsg {
		t.Fatalf("local pause first: sB hook = %s", out)
	}
	p.setPause(t, true) // team begins while the local one is on: the team's text wins
	if out := hook(t, sock, "UserPromptSubmit", "sB", wt, "", ""); out != pauseMsg {
		t.Fatalf("team pause over the local one: sB hook = %s", out)
	}
	if out := hook(t, sock, "UserPromptSubmit", "sA", f.Clone, "", ""); out != pauseMsg {
		t.Fatalf("both set: first hook of sA = %s, want the team text", out)
	}
	p.setPause(t, false)
	quiet("team lifted, local still on")
	setLocalPause(t, p.run.dir, false)
	if out := hook(t, sock, "PreToolUse", "sA", f.Clone, "Edit", edit); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("first Edit with both lifted = %s, want deny", out)
	}
	// And the other way: the team pause alone, with the local one set and
	// lifted inside it.
	p.setPause(t, true)
	setLocalPause(t, p.run.dir, true)
	if out := hook(t, sock, "UserPromptSubmit", "sA", f.Clone, "", ""); out != pauseMsg {
		t.Fatalf("second team pause: first hook of sA = %s", out)
	}
	setLocalPause(t, p.run.dir, false)
	quiet("local lifted, team still on")
	p.setPause(t, false)
	if out := hook(t, sock, "PreToolUse", "sB", wt, "Edit", filepath.Join(wt, "shared.go")); strings.Contains(out, "pausa") {
		t.Fatalf("hook after both lifted = %s", out)
	}
}

// TestWhoTouchesPaused: while this machine or the team is paused, who
// tells only that, the team's pause first, whatever the path, and never asks
// serve. An intent is still taken, and shows once the pause ends.
func TestWhoTouchesPaused(t *testing.T) {
	f, wt := overlap(t)
	p := startPausable(t)
	sock := p.run.sock
	hook(t, sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, sock, "SessionStart", "sB", wt, "", "")
	path := filepath.Join(f.Clone, "shared.go")
	eventually(t, 10*time.Second, "who sees feat-b", func() bool { return strings.Contains(who(t, sock, path), `"branch":"feat-b"`) })
	asked := p.stateCalls.Load()
	if asked == 0 {
		t.Fatal("who never asked serve before the pause")
	}

	const local, team = `{"tracked":true,"paused":"local"}`, `{"tracked":true,"paused":"team"}`
	setLocalPause(t, p.run.dir, true)
	for _, path := range []string{path, filepath.Join(shortDir(t), "x.go"), "shared.go"} {
		if got := who(t, sock, path); got != local {
			t.Fatalf("who %s during the local pause = %s, want %s", path, got, local)
		}
	}
	declareOK(t, sock, "sB", "fix the login")
	p.setPause(t, true)
	if got := who(t, sock, path); got != team {
		t.Fatalf("who during both pauses = %s, want the team's %s", got, team)
	}
	setLocalPause(t, p.run.dir, false)
	if got := who(t, sock, path); got != team {
		t.Fatalf("who during the team pause = %s, want %s", got, team)
	}
	if n := p.stateCalls.Load() - asked; n != 0 {
		t.Fatalf("who asked serve %d times during the pause", n)
	}
	p.setPause(t, false)
	eventually(t, 10*time.Second, "who after the pause, with the intent declared during it", func() bool {
		return strings.Contains(who(t, sock, path), `"branch":"feat-b","daemon":"`+string(p.run.d.ID())+`","intent":"fix the login"`)
	})
}
