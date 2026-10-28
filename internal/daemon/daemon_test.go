package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// shortDir is a temp dir short enough for a unix socket path (macOS
// caps it at 104 bytes, and t.TempDir() paths can be longer).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newServe(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(serve.New().Handler())
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

// closeServer drops open streams first: httptest's Close waits for every
// active request, and an SSE stream only ends when its client goes away.
func closeServer(srv *httptest.Server) {
	srv.CloseClientConnections()
	srv.Close()
}

type running struct {
	d    *Daemon
	dir  string
	sock string
}

func startDaemon(t *testing.T, server string) running {
	t.Helper()
	return startDaemonWith(t, server, nil)
}

// startDaemonWith runs set, when there is one, on the daemon before Run.
func startDaemonWith(t *testing.T, server string, set func(*Daemon)) running {
	t.Helper()
	dir := shortDir(t)
	d, err := New(Config{StateDir: dir, Server: server, Poll: 200 * time.Millisecond, Resend: time.Second, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if set != nil {
		set(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop within 5s")
		}
	})
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

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func call(t *testing.T, sock string, req wire.Request) wire.Response {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp wire.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("bad response %q: %v", line, err)
	}
	return resp
}

func hook(t *testing.T, sock, event, session, cwd, tool, file string) string {
	t.Helper()
	in := map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": event}
	if tool != "" {
		in["tool_name"] = tool
		in["tool_input"] = map[string]any{"file_path": file}
	}
	b, _ := json.Marshal(in)
	resp := call(t, sock, wire.Request{Type: wire.TypeHook, Event: event, Input: b})
	if resp.Error != "" {
		t.Fatalf("hook %s: %s", event, resp.Error)
	}
	return string(resp.Output)
}

func warnings(t *testing.T, srv *httptest.Server) wire.StateDump {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d wire.StateDump
	json.NewDecoder(resp.Body).Decode(&d)
	return d
}

// overlap leaves shared.go changed in the clone (branch feat-a) and in a
// linked worktree on feat-b.
func overlap(t *testing.T) (testgit.Fixture, string) {
	t.Helper()
	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	wt := f.AddWorktree(t, "wtb", "feat-b")
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "from a\n")
	testgit.WriteFile(t, filepath.Join(wt, "shared.go"), "from b\n")
	return f, wt
}

func TestTwoWorktreesOneDaemon(t *testing.T) {
	f, wt := overlap(t)
	r := startDaemon(t, newServe(t).URL)
	for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
		if out := hook(t, r.sock, "SessionStart", s.id, s.cwd, "", ""); strings.Contains(out, "dugout-context") ||
			!strings.Contains(out, `office_declare_intent (session_id: \"`+s.id+`\")`) {
			t.Fatalf("SessionStart %s before any scan = %s, want only the ask for the intent", s.id, out)
		}
	}
	for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
		eventually(t, 10*time.Second, s.id+" gets the warning", func() bool {
			out := hook(t, r.sock, "PostToolUse", s.id, s.cwd, "Edit", filepath.Join(s.cwd, "shared.go"))
			return strings.Contains(out, `\"shared.go\"`) && strings.Contains(out, "dugout-context")
		})
	}
	if out := hook(t, r.sock, "PostToolUse", "sA", f.Clone, "Bash", ""); out != "{}" {
		t.Fatalf("second delivery to sA = %s, want {}", out)
	}
}

// TestDeliveryLogged: the daemon logs when a warning arrives from serve and
// when a hook hands it to the agent, so propagation can be timed.
func TestDeliveryLogged(t *testing.T) {
	f, wt := overlap(t)
	rec := &recorder{}
	dir := shortDir(t)
	d, err := New(Config{StateDir: dir, Server: newServe(t).URL, Poll: 200 * time.Millisecond, Resend: time.Second, Log: slog.New(rec)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	sock := statedir.Socket(dir)
	eventually(t, 2*time.Second, "socket", func() bool {
		c, err := net.Dial("unix", sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})

	hook(t, sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 10*time.Second, "sA gets the warning", func() bool {
		return strings.Contains(hook(t, sock, "UserPromptSubmit", "sA", f.Clone, "", ""), "shared.go")
	})
	// The local core can hand sA the overlap before serve's copy arrives.
	eventually(t, 10*time.Second, "serve's warning received", func() bool {
		_, ok := rec.find("warning received", nil)
		return ok
	})
	received, ok := rec.find("warning received", nil)
	if !ok || received["id"] == "" || received["path"] != "shared.go" {
		t.Fatalf("no warning received record: %v", received)
	}
	want := map[string]string{"id": received["id"], "session": "sA", "event": "UserPromptSubmit", "kind": "overlap"}
	if _, ok := rec.find("warning delivered", want); !ok {
		t.Fatalf("no warning delivered record for sA (records: %v)", rec.msgs)
	}
}

// TestLocalDenyWithoutServer: with no server, the daemon's own detector
// finds the conflict, tells sA after a tool call and stops sA's first edit
// of the file.
func TestLocalDenyWithoutServer(t *testing.T) {
	f, wt := overlap(t)
	r := startDaemon(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	var out string
	eventually(t, 15*time.Second, "sA's PostToolUse names the conflict", func() bool {
		out = hook(t, r.sock, "PostToolUse", "sA", f.Clone, "", "")
		return strings.Contains(out, "conflict") && strings.Contains(out, `your line 1 of \"shared.go\"`)
	})
	if !strings.Contains(out, `branch \"feat-b\"`) {
		t.Fatalf("sA's conflict should name feat-b: %s", out)
	}
	edit := filepath.Join(f.Clone, "shared.go")
	if out := hook(t, r.sock, "PreToolUse", "sA", f.Clone, "Edit", edit); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("first Edit of the conflicted file = %s, want deny", out)
	}
	if out := hook(t, r.sock, "PreToolUse", "sA", f.Clone, "Edit", edit); out != "{}" {
		t.Fatalf("retried Edit = %s, want {}", out)
	}
}

// TestSameFileDifferentRegionsNoDeny: two workstreams that edit different
// regions of one file hear about it at the next turn, but merge cleanly, so
// no edit of the file is stopped.
func TestSameFileDifferentRegionsNoDeny(t *testing.T) {
	f, wt := differentRegions(t)
	r, rec := startLogged(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 15*time.Second, "sA's UserPromptSubmit names multi.txt", func() bool {
		return strings.Contains(hook(t, r.sock, "UserPromptSubmit", "sA", f.Clone, "", ""), `\"multi.txt\"`)
	})
	edit := func(when string) {
		t.Helper()
		if out := hook(t, r.sock, "PreToolUse", "sA", f.Clone, "Edit", filepath.Join(f.Clone, "multi.txt")); out != "{}" {
			t.Fatalf("Edit of multi.txt %s = %s, want {}", when, out)
		}
	}
	edit("once the overlap is known")
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	wsB := WorkstreamIDFor(r.d.ID(), wt, "feat-b")
	eventually(t, 15*time.Second, "both sides snapshotted", func() bool {
		return countWith(rec, "snapshot", map[string]string{"ws": string(wsA)}) > 0 &&
			countWith(rec, "snapshot", map[string]string{"ws": string(wsB)}) > 0
	})
	waitRuns(t, rec, 3)
	edit("after 3 detector runs")
}

// TestRemoteOverlapNeverDenies: an overlap with another daemon's workstream
// reaches the next turn and never stops an edit.
func TestRemoteOverlapNeverDenies(t *testing.T) {
	f, _ := overlap(t)
	var mu sync.Mutex
	var sent []core.Warning
	srv := fakeServe(t, func() []core.Warning { mu.Lock(); defer mu.Unlock(); return sent })
	d, _, dir := newLogged(t, srv.URL)
	a := core.Side{Daemon: d.ID(), Workstream: WorkstreamIDFor(d.ID(), f.Clone, "feat-a"), Branch: "feat-a"}
	remote := core.Side{Daemon: "d2", Workstream: "d2:x", Branch: "x"}
	mu.Lock()
	sent = []core.Warning{between(repoOf(t, f.Clone), "remote.go", a, remote)}
	mu.Unlock()

	r := runDaemon(t, d, dir)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	eventually(t, 10*time.Second, "sA's UserPromptSubmit names remote.go", func() bool {
		return strings.Contains(hook(t, r.sock, "UserPromptSubmit", "sA", f.Clone, "", ""), `\"remote.go\"`)
	})
	if out := hook(t, r.sock, "PreToolUse", "sA", f.Clone, "Edit", filepath.Join(f.Clone, "remote.go")); out != "{}" {
		t.Fatalf("Edit of a file shared with a remote workstream = %s, want {}", out)
	}
}

// TestFirstPostIsNotEmpty: a restarted daemon must not tell serve it
// has nothing in flight before its first scan, which would clear the other
// side's warnings for the length of that scan.
func TestFirstPostIsNotEmpty(t *testing.T) {
	f := testgit.NewFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "changed\n")
	dir := shortDir(t)
	if err := statedir.SaveWorktrees(dir, []string{f.Clone}); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{StateDir: dir, Server: "", Poll: 200 * time.Millisecond, Resend: time.Hour, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}

	office := serve.New().Handler()
	var mu sync.Mutex
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var msg wire.Message
			if json.Unmarshal(body, &msg) == nil && msg.Daemon == d.ID() {
				mu.Lock()
				sizes = append(sizes, len(msg.Workstreams))
				mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		office.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	d.cfg.Server = srv.URL
	d.uplink.Server = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, 10*time.Second, "a post from the daemon", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sizes) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if sizes[0] == 0 {
		t.Fatalf("first full_state was empty (posts: %v)", sizes)
	}
}

func TestUnknownSessionRegistered(t *testing.T) {
	f, wt := overlap(t)
	r := startDaemon(t, newServe(t).URL)
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 10*time.Second, "a never-seen session gets the warning", func() bool {
		return strings.Contains(hook(t, r.sock, "PostToolUse", "ghost", f.Clone, "", ""), "shared.go")
	})
}

// TestLateSessionAskedForIntent: a session the daemon first hears of at a
// UserPromptSubmit or PostToolUse, as every live one after a daemon restart,
// is asked for its intent in that answer, once. One first heard of at a
// PreToolUse, which never carries context, is asked at its next
// UserPromptSubmit or PostToolUse. One registered at its SessionStart is asked
// there and not again.
func TestLateSessionAskedForIntent(t *testing.T) {
	f := changedClone(t)
	r := startDaemon(t, "")
	contextOf := func(event, session string) string {
		t.Helper()
		var out struct {
			HookSpecificOutput struct{ AdditionalContext string } `json:"hookSpecificOutput"`
		}
		raw := hook(t, r.sock, event, session, f.Clone, "", "")
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s for %s: %v in %s", event, session, err, raw)
		}
		return out.HookSpecificOutput.AdditionalContext
	}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse"} {
		id := "late-" + event
		if ctx := contextOf(event, id); ctx != askFor(id) {
			t.Fatalf("first %s of an unknown session = %q, want the ask", event, ctx)
		}
		if out := hook(t, r.sock, event, id, f.Clone, "", ""); out != "{}" {
			t.Fatalf("second %s = %s, want {}", event, out)
		}
		declareOK(t, r.sock, id, "fix the login")
	}
	if out := hook(t, r.sock, "PreToolUse", "late-pre", f.Clone, "Edit", filepath.Join(f.Clone, "shared.go")); out != "{}" {
		t.Fatalf("PreToolUse of an unknown session = %s, want {}", out)
	}
	if ctx := contextOf("PostToolUse", "late-pre"); ctx != askFor("late-pre") {
		t.Fatalf("PostToolUse after the PreToolUse that registered it = %q, want the ask", ctx)
	}
	if ctx := contextOf("SessionStart", "sA"); ctx != askFor("sA") {
		t.Fatalf("SessionStart = %q, want the ask", ctx)
	}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse"} {
		if out := hook(t, r.sock, event, "sA", f.Clone, "", ""); out != "{}" {
			t.Fatalf("%s after SessionStart = %s, want {}", event, out)
		}
	}
}

func TestSymlinkedPaths(t *testing.T) {
	f, wt := overlap(t)
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(f.Clone, link); err != nil {
		t.Fatal(err)
	}
	r := startDaemon(t, newServe(t).URL)
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 10*time.Second, "session through a symlink gets the warning", func() bool {
		return strings.Contains(hook(t, r.sock, "PostToolUse", "sL", link, "", ""), "shared.go")
	})
	out := hook(t, r.sock, "PreToolUse", "sL", link, "Edit", filepath.Join(link, "shared.go"))
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("Edit through a symlinked path = %s, want deny", out)
	}
}

func TestSocketPathTooLong(t *testing.T) {
	_, err := New(Config{StateDir: "/tmp/" + strings.Repeat("x", 120), Log: quiet})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("long socket path: err = %v", err)
	}
	_, err = New(Config{StateDir: shortDir(t), Socket: "/tmp/" + strings.Repeat("x", 120), Log: quiet})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("long Socket: err = %v", err)
	}
}

// TestSocketOutsideStateDir: with a Socket set (Linux's $XDG_RUNTIME_DIR)
// the daemon listens there and leaves none in the state dir.
func TestSocketOutsideStateDir(t *testing.T) {
	dir, sock := shortDir(t), filepath.Join(shortDir(t), "dugout.sock")
	d, err := New(Config{StateDir: dir, Socket: sock, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	eventually(t, 2*time.Second, "socket accepts", func() bool {
		c, err := net.Dial("unix", sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if _, err := os.Stat(statedir.Socket(dir)); err == nil {
		t.Error("the daemon also made a socket in the state dir")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err == nil {
		t.Error("the socket outlived the daemon")
	}
}

// TestNewCorruptEpochWarns: a corrupt epoch file does not keep the daemon
// from starting: the run's epoch comes from the clock, and the log says so.
func TestNewCorruptEpochWarns(t *testing.T) {
	dir := shortDir(t)
	if err := os.WriteFile(filepath.Join(dir, "epoch"), []byte("basura\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	floor := uint64(time.Now().UnixMilli())
	rec := &recorder{}
	d, err := New(Config{StateDir: dir, Log: slog.New(rec)})
	if err != nil {
		t.Fatalf("New with a corrupt epoch file: %v", err)
	}
	if d.uplink.Epoch < floor {
		t.Fatalf("epoch %d, want at least the clock's floor %d", d.uplink.Epoch, floor)
	}
	if _, ok := rec.find("epoch file unreadable, epoch from the clock", map[string]string{"level": "WARN"}); !ok {
		t.Fatal("no warning about the corrupt epoch file")
	}
}

func TestAlreadyRunning(t *testing.T) {
	r := startDaemon(t, newServe(t).URL)
	second, err := New(Config{StateDir: r.dir, Server: "http://127.0.0.1:1", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := second.Run(ctx); err == nil || !strings.Contains(err.Error(), "is already running") {
		t.Fatalf("second daemon: err = %v", err)
	}
	if out := hook(t, r.sock, "SessionEnd", "x", "/", "", ""); out != "{}" {
		t.Fatalf("first daemon stopped answering: %s", out)
	}
}

func TestDeletedWorktreeUntracked(t *testing.T) {
	f, wt := overlap(t)
	srv := newServe(t)
	r := startDaemon(t, srv.URL)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 10*time.Second, "server has the warning", func() bool { return len(warnings(t, srv).Warnings) == 1 })

	testgit.Git(t, f.Clone, "worktree", "remove", "--force", wt)
	eventually(t, 10*time.Second, "warning gone after the worktree is deleted", func() bool {
		return len(warnings(t, srv).Warnings) == 0
	})
	roots, err := statedir.LoadWorktrees(r.dir)
	if err != nil || slices.Contains(roots, wt) || !slices.Contains(roots, f.Clone) {
		t.Fatalf("worktrees.json = %v, %v", roots, err)
	}
}

func TestTrack(t *testing.T) {
	f := testgit.NewFixture(t)
	r := startDaemon(t, newServe(t).URL)
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: f.Clone}); resp.Error != "" {
		t.Fatalf("track clone: %s", resp.Error)
	}
	if roots, _ := statedir.LoadWorktrees(r.dir); !slices.Equal(roots, []string{f.Clone}) {
		t.Fatalf("worktrees.json = %v", roots)
	}
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: shortDir(t)}); !strings.Contains(resp.Error, "not a git repo") {
		t.Fatalf("track non-repo: %+v", resp)
	}
	noOrigin := filepath.Join(f.Base, "no-origin")
	testgit.Git(t, f.Base, "init", "-q", "-b", "main", noOrigin)
	testgit.WriteFile(t, filepath.Join(noOrigin, "x"), "x\n")
	testgit.Git(t, noOrigin, "add", "-A")
	testgit.Git(t, noOrigin, "commit", "-q", "-m", "x")
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: noOrigin}); !strings.Contains(resp.Error, "remote origin") {
		t.Fatalf("track repo without origin: %+v", resp)
	}
	if roots, _ := statedir.LoadWorktrees(r.dir); len(roots) != 1 {
		t.Fatalf("failed tracks were persisted: %v", roots)
	}
}

// TestFirstHookDoesNotWaitForTheWrite: the hook that attaches a new worktree
// answers without waiting for worktrees.json, whose two fsyncs can outlast
// the hook's 150 ms on a busy disk, and the write still lands.
func TestFirstHookDoesNotWaitForTheWrite(t *testing.T) {
	f := testgit.NewFixture(t)
	r := startDaemonWith(t, "", func(d *Daemon) {
		d.saveRoots = func(dir string, roots []string) error {
			time.Sleep(time.Second)
			return statedir.SaveWorktrees(dir, roots)
		}
	})
	t0 := time.Now()
	hook(t, r.sock, "SessionStart", "s1", f.Clone, "", "")
	if took := time.Since(t0); took > 500*time.Millisecond {
		t.Fatalf("first SessionStart took %s, want it not to wait for the 1s write", took)
	}
	eventually(t, 5*time.Second, "worktrees.json has the clone", func() bool {
		roots, _ := statedir.LoadWorktrees(r.dir)
		return slices.Equal(roots, []string{f.Clone})
	})
}

// TestUntrackableRepoNotRetracked: a session in a repo without origin is
// not registered as a workstream, and its later hooks do not track it again.
func TestUntrackableRepoNotRetracked(t *testing.T) {
	testgit.Isolate(t)
	repo := shortDir(t)
	testgit.Git(t, repo, "init", "-q", "-b", "main")
	testgit.WriteFile(t, filepath.Join(repo, "x"), "x\n")
	testgit.Git(t, repo, "add", "-A")
	testgit.Git(t, repo, "commit", "-q", "-m", "x")
	r := startDaemon(t, newServe(t).URL)

	// In memory, as a hook tracks: worktrees.json is written after the hook
	// answers, so it can hold the root for a moment.
	state := func() (tracked, rejected int) {
		r.d.mu.Lock()
		defer r.d.mu.Unlock()
		return len(r.d.worktrees), len(r.d.rejected)
	}
	hook(t, r.sock, "SessionStart", "s1", repo, "", "")
	eventually(t, 5*time.Second, "repo without origin untracked", func() bool {
		tracked, rejected := state()
		return tracked == 0 && rejected == 1
	})
	hook(t, r.sock, "PostToolUse", "s1", repo, "", "")
	if tracked, _ := state(); tracked != 0 {
		t.Fatal("a later hook tracked the untrackable repo again")
	}
	eventually(t, 5*time.Second, "worktrees.json without the repo", func() bool {
		roots, err := statedir.LoadWorktrees(r.dir)
		return err == nil && len(roots) == 0
	})
}

// TestGitFailureKeepsTracking: while git cannot run at all, polls fail, but
// the tracked worktree stays tracked, on disk and in memory.
func TestGitFailureKeepsTracking(t *testing.T) {
	f := testgit.NewFixture(t)
	r := startDaemon(t, newServe(t).URL)
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: f.Clone}); resp.Error != "" {
		t.Fatalf("track: %s", resp.Error)
	}
	t.Setenv("PATH", "")
	time.Sleep(time.Second) // several 200 ms polls with no git
	if roots, _ := statedir.LoadWorktrees(r.dir); !slices.Equal(roots, []string{f.Clone}) {
		t.Fatalf("worktrees.json after git failures = %v, want [%s]", roots, f.Clone)
	}
}

// TestScansSlowerThanPoll: once a scan outlasts the poll interval, every
// poll lands during a scan and asks for one more, so the scans never stop.
// Each scan's result still reaches the local core and the detector.
func TestScansSlowerThanPoll(t *testing.T) {
	f, wt := overlap(t)
	t.Setenv("PATH", stubGit(t, "status", "sleep 0.3")+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, rec := startLogged(t, "")
	// Tracked, not through hooks: the stub's shell can outlast a hook's
	// 100 ms budget for finding the worktree.
	for _, dir := range []string{f.Clone, wt} {
		if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: dir}); resp.Error != "" {
			t.Fatalf("track %s: %s", dir, resp.Error)
		}
	}
	// Changed after the track, so only a polled scan can see it.
	testgit.WriteFile(t, filepath.Join(f.Clone, "late.go"), "late from a\n")
	testgit.WriteFile(t, filepath.Join(wt, "late.go"), "late from b\n")
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	eventually(t, 10*time.Second, "overlap on late.go", func() bool { return hasPath(r.d.overlapsFor(wsA), "late.go") })
	eventually(t, 15*time.Second, "conflict detected on shared.go", func() bool {
		_, ok := rec.find("conflict detected", map[string]string{"path": "shared.go"})
		return ok
	})
}

func TestUnknownRequestType(t *testing.T) {
	r := startDaemon(t, newServe(t).URL)
	if resp := call(t, r.sock, wire.Request{Type: "delivered"}); !strings.Contains(resp.Error, "delivered") {
		t.Fatalf("unknown type: %+v", resp)
	}
}

func TestWorkstreamIDFor(t *testing.T) {
	a := WorkstreamIDFor("d1", "/r", "main")
	if a != WorkstreamIDFor("d1", "/r", "main") || a == WorkstreamIDFor("d1", "/r", "feat") ||
		a == WorkstreamIDFor("d2", "/r", "main") || !strings.HasPrefix(string(a), "d1:") {
		t.Fatalf("WorkstreamIDFor not stable per (daemon, root, branch): %s", a)
	}
}

const unknownSession = "Dugout does not know that session: use the session_id Dugout gave you at the start."

func declare(t *testing.T, sock, session, text string) wire.Response {
	t.Helper()
	return call(t, sock, wire.Request{Type: wire.TypeIntent, SessionID: session, Text: text})
}

func declareOK(t *testing.T, sock, session, text string) {
	t.Helper()
	if resp := declare(t, sock, session, text); resp.Error != "" || string(resp.Output) != `{"ok":true}` {
		t.Fatalf("declare %q for %s = %+v, want ok", text, session, resp)
	}
}

// changedClone is a fixture whose clone has shared.go in flight.
func changedClone(t *testing.T) testgit.Fixture {
	t.Helper()
	f := testgit.NewFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "changed\n")
	return f
}

// onlyWorkstream waits for d's full state to hold one workstream and
// returns it.
func onlyWorkstream(t *testing.T, d *Daemon) core.Workstream {
	t.Helper()
	var ws []core.Workstream
	eventually(t, 10*time.Second, "the worktree's workstream", func() bool {
		ws = d.fullState().Workstreams
		return len(ws) == 1
	})
	return ws[0]
}

// TestIntentInFullState: an intent declared for a session lands, sanitized,
// on its worktree's workstream, and goes to serve in the full state. The log
// names the session and the workstream, never the text.
func TestIntentInFullState(t *testing.T) {
	f := changedClone(t)
	srv := newServe(t)
	d, rec, dir := newLogged(t, srv.URL)
	r := runDaemon(t, d, dir)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	eventually(t, 10*time.Second, "serve has the workstream", func() bool { return len(warnings(t, srv).Workstreams) == 1 })

	declareOK(t, r.sock, "sA", "fix the \"login\"\n")
	const want = "fix the 'login'"
	ws := onlyWorkstream(t, d)
	if ws.Intent != want {
		t.Fatalf("full state intent = %q, want %q", ws.Intent, want)
	}
	eventually(t, 10*time.Second, "serve has the intent", func() bool {
		got := warnings(t, srv).Workstreams
		return len(got) == 1 && got[0].Intent == want
	})
	if _, ok := rec.find("intent declared", map[string]string{"level": "INFO", "session": "sA", "workstream": string(ws.ID)}); !ok {
		t.Fatalf("no intent declared record for sA and %s (records: %v)", ws.ID, rec.msgs)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, attrs := range rec.attrs {
		for k, v := range attrs {
			if strings.Contains(v, "login") {
				t.Fatalf("the log carries the intent: %s %s=%q", rec.msgs[i], k, v)
			}
		}
	}
}

// TestIntentUnknownSession: the daemon takes an intent only for a
// session it registered.
func TestIntentUnknownSession(t *testing.T) {
	r := startDaemon(t, "")
	for _, id := range []string{"ghost", ""} {
		if resp := declare(t, r.sock, id, "fix the login"); resp.Error != unknownSession || len(resp.Output) != 0 {
			t.Fatalf("declare for session %q = %+v, want %q", id, resp, unknownSession)
		}
	}
}

// TestIntentEmpty: an intent with nothing left after sanitizing is refused,
// and the one declared before stays.
func TestIntentEmpty(t *testing.T) {
	f := changedClone(t)
	r := startDaemon(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	declareOK(t, r.sock, "sA", "fix the login")
	for _, text := range []string{"", "\n‮", "  "} {
		if resp := declare(t, r.sock, "sA", text); resp.Error != "the intent is empty" || len(resp.Output) != 0 {
			t.Fatalf("declare %q = %+v, want the empty intent error", text, resp)
		}
	}
	if got := onlyWorkstream(t, r.d).Intent; got != "fix the login" {
		t.Fatalf("intent after the refused ones = %q", got)
	}
}

// TestSubagentOverwritesParentIntent: a subagent runs with its
// parent's session_id, so its declaration replaces the session's intent.
func TestSubagentOverwritesParentIntent(t *testing.T) {
	f := changedClone(t)
	r := startDaemon(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	declareOK(t, r.sock, "sA", "migrate payments to the new API")
	declareOK(t, r.sock, "sA", "find where the amount is validated")
	if got := onlyWorkstream(t, r.d).Intent; got != "find where the amount is validated" {
		t.Fatalf("intent after the second declaration = %q, want the subagent's", got)
	}
}

// TestIntentOutlivesSession: the intent stays with the worktree when the
// session that declared it ends, and goes when the worktree is untracked.
func TestIntentOutlivesSession(t *testing.T) {
	f := changedClone(t)
	r := startDaemon(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	declareOK(t, r.sock, "sA", "fix the login")
	hook(t, r.sock, "SessionEnd", "sA", f.Clone, "", "")
	if got := onlyWorkstream(t, r.d).Intent; got != "fix the login" {
		t.Fatalf("intent after SessionEnd = %q, want it kept", got)
	}
	if resp := declare(t, r.sock, "sA", "something else"); resp.Error != unknownSession {
		t.Fatalf("declare for an ended session = %+v, want %q", resp, unknownSession)
	}
	r.d.untrack(f.Clone)
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: f.Clone}); resp.Error != "" {
		t.Fatalf("track: %s", resp.Error)
	}
	if got := onlyWorkstream(t, r.d).Intent; got != "" {
		t.Fatalf("intent after the worktree was untracked = %q, want none", got)
	}
}

func who(t *testing.T, sock, path string) string {
	t.Helper()
	resp := call(t, sock, wire.Request{Type: wire.TypeWho, Path: path})
	if resp.Error != "" {
		t.Fatalf("who %s: %s", path, resp.Error)
	}
	return string(resp.Output)
}

// TestWhoOutput: who answers from the tracked worktree that holds the path,
// the deepest when one sits inside another, with the repo's other workstreams
// that have the file in flight, sorted by ID, never the asking one. Without a
// server, serve is off. A path outside every tracked worktree, or not
// absolute, is not tracked.
func TestWhoOutput(t *testing.T) {
	f, wt := overlap(t)
	nested := filepath.Join(f.Clone, "nested")
	testgit.WriteFile(t, filepath.Join(f.Clone, ".git", "info", "exclude"), "nested/\n")
	testgit.Git(t, f.Clone, "worktree", "add", "-q", "-b", "feat-n", nested)
	testgit.WriteFile(t, filepath.Join(nested, "shared.go"), "from n\n")
	r := startDaemon(t, "")
	for _, s := range []struct{ id, dir string }{{"sA", f.Clone}, {"sB", wt}, {"sN", nested}} {
		hook(t, r.sock, "SessionStart", s.id, s.dir, "", "")
	}
	declareOK(t, r.sock, "sB", "fix the login")

	for _, p := range []string{filepath.Join(shortDir(t), "x.go"), "shared.go"} {
		if got := who(t, r.sock, p); got != `{"tracked":false}` {
			t.Fatalf("who %s = %s, want not tracked", p, got)
		}
	}
	type touch struct {
		id     core.WorkstreamID
		branch string
		intent string
	}
	answer := func(ts ...touch) string {
		slices.SortFunc(ts, func(a, b touch) int { return strings.Compare(string(a.id), string(b.id)) })
		out := wire.Who{Tracked: true, Server: "off", Touches: []wire.Touch{}}
		for _, x := range ts {
			out.Touches = append(out.Touches, wire.Touch{Branch: x.branch, Daemon: r.d.ID(), Intent: x.intent})
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	id := func(root, branch string) core.WorkstreamID { return WorkstreamIDFor(r.d.ID(), root, branch) }
	a, b, n := touch{id(f.Clone, "feat-a"), "feat-a", ""}, touch{id(wt, "feat-b"), "feat-b", "fix the login"}, touch{id(nested, "feat-n"), "feat-n", ""}
	for _, c := range []struct{ path, want string }{
		{filepath.Join(f.Clone, "shared.go"), answer(b, n)},
		{filepath.Join(nested, "shared.go"), answer(a, b)},
		{filepath.Join(wt, "shared.go"), answer(a, n)},
		{filepath.Join(f.Clone, "a.txt"), answer()},
	} {
		eventually(t, 10*time.Second, "who "+c.path, func() bool { return who(t, r.sock, c.path) == c.want })
	}
}

// TestNewRefusesInsecureServer: an http:// server outside this machine, where
// the token travels in the clear, needs Insecure; loopback and https do not.
func TestNewRefusesInsecureServer(t *testing.T) {
	for _, tc := range []struct {
		server   string
		insecure bool
		refused  bool
	}{
		{"http://10.0.0.5:7457", false, true},
		{"HTTP://10.0.0.5:7457", false, true},
		{"http://10.0.0.5:7457", true, false},
		{"http://127.0.0.1:7457", false, false},
		{"http://localhost:7457", false, false},
		{"https://10.0.0.5:7457", false, false},
		{"", false, false},
	} {
		_, err := New(Config{StateDir: t.TempDir(), Server: tc.server, Insecure: tc.insecure})
		if got := errors.Is(err, wire.ErrInsecureServer); got != tc.refused || (err != nil && !tc.refused) {
			t.Errorf("New(Server %q, Insecure %v) = %v, want refused=%v", tc.server, tc.insecure, err, tc.refused)
		}
	}
}
