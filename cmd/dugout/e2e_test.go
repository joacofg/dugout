package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

var bin string

// e2eToken is the shared token every e2e serve and daemon uses.
const e2eToken = "e2e-token"

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dugout-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "dugout")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build dugout: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// proc is a background dugout process whose log is printed if the test fails.
type proc struct {
	cmd *exec.Cmd
	log *syncBuffer
}

// syncBuffer is a log buffer os/exec can write from its goroutine while a
// test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func start(t *testing.T, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(bin, args...), log: &syncBuffer{}}
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("dugout %s log:\n%s", strings.Join(args, " "), p.log.String())
		}
	})
	return p
}

func (p *proc) stop() {
	if p.cmd.ProcessState != nil {
		return
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		<-done
	}
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", within, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func state(addr string) (wire.StateDump, error) {
	var d wire.StateDump
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/state", nil)
	req.Header.Set("Authorization", "Bearer "+e2eToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	return d, json.NewDecoder(resp.Body).Decode(&d)
}

func startServe(t *testing.T, addr string) *proc {
	t.Helper()
	p := start(t, []string{wire.EnvToken + "=" + e2eToken}, "serve", "-addr", addr)
	eventually(t, 5*time.Second, "serve answers", func() bool { _, err := state(addr); return err == nil })
	return p
}

func waitDaemonSocket(t *testing.T, home string) {
	t.Helper()
	eventually(t, 5*time.Second, "daemon socket", func() bool {
		c, err := net.Dial("unix", statedir.Socket(home))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

// startLocalDaemon runs a serverless daemon (stage 2 only, no uplink).
func startLocalDaemon(t *testing.T, home string) *proc {
	t.Helper()
	p := start(t, []string{statedir.EnvHome + "=" + home}, "daemon", "-server", "off", "-poll", "300ms")
	waitDaemonSocket(t, home)
	return p
}

func startDaemon(t *testing.T, home, addr string) *proc {
	t.Helper()
	p := start(t, []string{statedir.EnvHome + "=" + home, wire.EnvToken + "=" + e2eToken}, "daemon", "-server", "http://"+addr, "-poll", "300ms", "-resend", "2s")
	waitDaemonSocket(t, home)
	return p
}

// runHook runs `dugout hook <event>` like Claude Code would and returns stdout.
func runHook(t *testing.T, home, event string, input map[string]any) string {
	t.Helper()
	input["hook_event_name"] = event
	b, _ := json.Marshal(input)
	cmd := exec.Command(bin, "hook", event)
	// A temp HOME has no LaunchAgent or unit, so a degraded hook never
	// kickstarts the real one; nor does an inherited XDG_CONFIG_HOME.
	cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home, "HOME="+home, "XDG_CONFIG_HOME=")
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("dugout hook %s: %v", event, err)
	}
	return strings.TrimSpace(string(out))
}

type world struct {
	f            testgit.Fixture
	a, b         string // worktrees: the clone on feat-a and a linked worktree on feat-b
	addr         string
	homeA, homeB string
	serve        *proc
	daemonA      *proc
}

// overlapWorld runs serve and two daemons (one per worktree, like two
// machines), opens a session in each worktree and edits shared.go and
// package-lock.json on both sides.
func overlapWorld(t *testing.T) *world {
	t.Helper()
	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	w := &world{f: f, a: f.Clone, b: f.AddWorktree(t, "wtb", "feat-b"), addr: freeAddr(t), homeA: shortDir(t), homeB: shortDir(t)}
	w.serve = startServe(t, w.addr)
	w.daemonA = startDaemon(t, w.homeA, w.addr)
	startDaemon(t, w.homeB, w.addr)

	// Nothing to hand over yet: only the ask for the intent.
	if out := runHook(t, w.homeA, "SessionStart", map[string]any{"session_id": "sA", "cwd": w.a}); strings.Contains(out, "dugout-context") || !strings.Contains(out, "office_declare_intent") {
		t.Fatalf("SessionStart sA = %s", out)
	}
	if out := runHook(t, w.homeB, "SessionStart", map[string]any{"session_id": "sB", "cwd": w.b}); strings.Contains(out, "dugout-context") || !strings.Contains(out, "office_declare_intent") {
		t.Fatalf("SessionStart sB = %s", out)
	}
	for _, dir := range []string{w.a, w.b} {
		testgit.WriteFile(t, filepath.Join(dir, "shared.go"), "edited in "+filepath.Base(dir)+"\n")
		testgit.WriteFile(t, filepath.Join(dir, "package-lock.json"), "edited in "+filepath.Base(dir)+"\n")
	}
	edit := func(session, dir string) map[string]any {
		return map[string]any{"session_id": session, "cwd": dir, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(dir, "shared.go")}}
	}
	runHook(t, w.homeA, "PostToolUse", edit("sA", w.a))
	runHook(t, w.homeB, "PostToolUse", edit("sB", w.b))
	eventually(t, 10*time.Second, "server warns about shared.go", func() bool {
		d, err := state(w.addr)
		return err == nil && len(d.Warnings) == 1 && d.Warnings[0].Path == "shared.go"
	})
	return w
}

func TestE2ETwoWorktrees(t *testing.T) {
	w := overlapWorld(t)
	d, _ := state(w.addr)
	for _, warn := range d.Warnings {
		if warn.Path == "package-lock.json" {
			t.Fatalf("ignored lockfile produced a warning: %+v", d.Warnings)
		}
	}
	for _, s := range []struct{ home, id, dir string }{{w.homeA, "sA", w.a}, {w.homeB, "sB", w.b}} {
		var out string
		eventually(t, 10*time.Second, s.id+" gets the warning on its next hook", func() bool {
			out = runHook(t, s.home, "UserPromptSubmit", map[string]any{"session_id": s.id, "cwd": s.dir, "prompt": "carry on"})
			return strings.Contains(out, "shared.go")
		})
		var parsed struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("%s: hook printed invalid JSON %s", s.id, out)
		}
		ctx := parsed.HookSpecificOutput.AdditionalContext
		if parsed.HookSpecificOutput.HookEventName != "UserPromptSubmit" ||
			!strings.HasPrefix(ctx, "<dugout-context>") || !strings.Contains(ctx, `"shared.go"`) {
			t.Fatalf("%s context = %s", s.id, out)
		}
		if again := runHook(t, s.home, "UserPromptSubmit", map[string]any{"session_id": s.id, "cwd": s.dir}); again != "{}" {
			t.Fatalf("%s got the same warning twice: %s", s.id, again)
		}
	}

	// Two daemons are two machines: an overlap never stops an edit. Both
	// worktrees changed line 1 of shared.go, so the speculative merge also
	// finds a conflict, and whether it lands before this edit is a race: an
	// edit may be held, but only by that conflict, never by the overlap alone
	// (the conflict's own deny is TestE2EStage2Remote).
	pre := map[string]any{"session_id": "sA", "cwd": w.a, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(w.a, "shared.go")}}
	if out := runHook(t, w.homeA, "PreToolUse", pre); out != "{}" &&
		!(strings.Contains(out, `"permissionDecision":"deny"`) && strings.Contains(out, `conflicts with in-flight changes on branch \"feat-b\"`)) {
		t.Fatalf("Edit of the shared file = %s, want {} or the conflict's deny", out)
	}
}

func TestE2EStage2Local(t *testing.T) {
	f := testgit.NewFixture(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	multi := func(edits map[int]string) string {
		out := slices.Clone(lines)
		for n, v := range edits {
			out[n-1] = v
		}
		return strings.Join(out, "\n") + "\n"
	}
	testgit.WriteFile(t, filepath.Join(f.Seed, "multi.txt"), multi(nil))
	testgit.Git(t, f.Seed, "add", "-A")
	testgit.Git(t, f.Seed, "commit", "-q", "-m", "multi")
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, f.Clone, "fetch", "-q", "origin")
	testgit.Git(t, f.Clone, "merge", "-q", "--ff-only", "origin/main")
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	a, b := f.Clone, f.AddWorktree(t, "wtb", "feat-b")
	home := shortDir(t)
	startLocalDaemon(t, home)

	for _, s := range []struct{ id, dir string }{{"sA", a}, {"sB", b}} {
		runHook(t, home, "SessionStart", map[string]any{"session_id": s.id, "cwd": s.dir})
	}
	write := func(dir string, edits map[int]string) {
		testgit.WriteFile(t, filepath.Join(dir, "multi.txt"), multi(edits))
	}
	post := func(session, dir string) string {
		return runHook(t, home, "PostToolUse", map[string]any{"session_id": session, "cwd": dir, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(dir, "multi.txt")}})
	}
	pre := func(session string) string {
		return runHook(t, home, "PreToolUse", map[string]any{"session_id": session, "cwd": a, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(a, "multi.txt")}})
	}

	// 1. Different lines of the same file: context only, never a deny.
	write(a, map[int]string{2: "A two"})
	write(b, map[int]string{9: "B nine"})
	post("sA", a)
	post("sB", b)
	eventually(t, 10*time.Second, "sA gets the overlap as context", func() bool {
		return strings.Contains(runHook(t, home, "UserPromptSubmit", map[string]any{"session_id": "sA", "cwd": a, "prompt": "carry on"}), `\"multi.txt\"`) // raw hook JSON escapes the quotes
	})
	if out := pre("sA"); out != "{}" {
		t.Fatalf("Edit with only an overlap = %s, want {}", out)
	}

	// 2. B touches line 2 too: a real textual conflict.
	write(b, map[int]string{2: "B two", 9: "B nine"})
	eventually(t, 10*time.Second, "sA gets the conflict after an edit", func() bool {
		return strings.Contains(post("sA", a), `your line 2 of \"multi.txt\"`)
	})
	out := pre("sA")
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "your line 2") || !strings.Contains(out, "retry the edit") || !strings.Contains(out, "  | B two") {
		t.Fatalf("Edit into a textual conflict = %s, want deny", out)
	}
	if out := pre("sA"); out != "{}" {
		t.Fatalf("retry = %s, want {}", out)
	}
	// B's agent edits no more: it hears of the conflict when it finishes, and
	// so does its human.
	var stop string
	eventually(t, 10*time.Second, "sB gets the conflict at Stop", func() bool {
		stop = runHook(t, home, "Stop", map[string]any{"session_id": "sB", "cwd": b, "stop_hook_active": false})
		return stop != "{}"
	})
	if !strings.Contains(stop, `"hookEventName":"Stop"`) || !strings.Contains(stop, `of \"multi.txt\"`) || !strings.Contains(stop, `"systemMessage":"Dugout: multi.txt conflicts`) {
		t.Fatalf("Stop of sB = %s, want the conflict as context and a systemMessage", stop)
	}
	if out := runHook(t, home, "Stop", map[string]any{"session_id": "sB", "cwd": b, "stop_hook_active": true}); out != "{}" {
		t.Fatalf("second Stop of sB = %s, want {}", out)
	}

	// 3. B goes back to the original line 2: the conflict is gone.
	write(b, map[int]string{9: "B nine"})
	// A fresh session per attempt: a deny is delivered once per session, so
	// {} can only mean the conflict is gone, not that an earlier poll consumed it.
	n := 0
	eventually(t, 10*time.Second, "a new session is not denied once the conflict is gone", func() bool {
		n++
		id := fmt.Sprintf("sA2-%d", n)
		runHook(t, home, "SessionStart", map[string]any{"session_id": id, "cwd": a})
		return pre(id) == "{}"
	})
}

func TestE2EStage2Remote(t *testing.T) {
	f := testgit.NewFixture(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	testgit.WriteFile(t, filepath.Join(f.Seed, "multi.txt"), strings.Join(lines, "\n")+"\n")
	testgit.Git(t, f.Seed, "add", "-A")
	testgit.Git(t, f.Seed, "commit", "-q", "-m", "multi")
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, f.Clone, "fetch", "-q", "origin")
	testgit.Git(t, f.Clone, "merge", "-q", "--ff-only", "origin/main")
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	a := f.Clone
	b := filepath.Join(f.Base, "clone-b")
	testgit.Git(t, f.Base, "clone", "-q", f.Origin, b)
	testgit.Git(t, b, "switch", "-q", "-c", "feat-b")

	addr, homeA, homeB := freeAddr(t), shortDir(t), shortDir(t)
	serve1 := startServe(t, addr)
	daemonA := startDaemon(t, homeA, addr)
	daemonB := startDaemon(t, homeB, addr)
	runHook(t, homeA, "SessionStart", map[string]any{"session_id": "sA", "cwd": a})
	runHook(t, homeB, "SessionStart", map[string]any{"session_id": "sB", "cwd": b})

	multi := func(line2 string) string {
		out := slices.Clone(lines)
		out[1] = line2
		return strings.Join(out, "\n") + "\n"
	}
	post := func(home, session, dir string) string {
		return runHook(t, home, "PostToolUse", map[string]any{"session_id": session, "cwd": dir, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(dir, "multi.txt")}})
	}
	pre := func(home, session, dir string) string {
		return runHook(t, home, "PreToolUse", map[string]any{"session_id": session, "cwd": dir, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(dir, "multi.txt")}})
	}
	oneConflict := func() bool {
		d, err := state(addr)
		return err == nil && len(d.Conflicts) == 1
	}

	// 1. Both change line 2: each daemon sees the other side's lines.
	testgit.WriteFile(t, filepath.Join(a, "multi.txt"), multi("A two"))
	testgit.WriteFile(t, filepath.Join(b, "multi.txt"), multi("B two"))
	eventually(t, 20*time.Second, "sA gets the remote conflict", func() bool {
		out := post(homeA, "sA", a)
		return strings.Contains(out, `your line 2 of \"multi.txt\"`) && strings.Contains(out, "  | B two")
	})
	eventually(t, 20*time.Second, "sB gets the remote conflict", func() bool {
		out := post(homeB, "sB", b)
		return strings.Contains(out, `your line 2 of \"multi.txt\"`) && strings.Contains(out, "  | A two")
	})
	out := pre(homeA, "sA", a)
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "retry the edit") {
		t.Fatalf("Edit into a remote conflict = %s, want deny", out)
	}
	if out := pre(homeA, "sA", a); out != "{}" {
		t.Fatalf("retry = %s, want {}", out)
	}
	eventually(t, 20*time.Second, "serve holds the conflict", oneConflict)
	// state never carries code: neither side's lines may show up in the raw body.
	rawState := func() string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/state", nil)
		req.Header.Set("Authorization", "Bearer "+e2eToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/state: %v", err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			t.Fatalf("read /v1/state: %v", err)
		}
		return buf.String()
	}
	if raw := rawState(); strings.Contains(raw, "A two") || strings.Contains(raw, "B two") {
		t.Fatalf("/v1/state leaks code: %s", raw)
	}

	// 2. serve restarts empty: the daemons re-upload and report again.
	serve1.stop()
	serve2 := startServe(t, addr)
	eventually(t, 20*time.Second, "restarted serve has the conflict again", oneConflict)

	// 3. A moves onto a new origin commit B's clone does not have: B fetches the base.
	testgit.WriteFile(t, filepath.Join(f.Seed, "newfile.txt"), "new\n")
	testgit.Git(t, f.Seed, "add", "-A")
	testgit.Git(t, f.Seed, "commit", "-q", "-m", "new")
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, a, "fetch", "-q", "origin")
	testgit.Git(t, a, "rebase", "-q", "--autostash", "origin/main")
	eventually(t, 20*time.Second, "daemon B fetches the missing base", func() bool {
		return strings.Contains(daemonB.log.String(), "base fetched")
	})
	// The conflict was already live before the move, so wait for the log of a
	// check that ran after the base fetch: import of A's new snapshot, then a
	// detection that still finds the conflict.
	after := func(l, marker string) (string, bool) {
		i := strings.Index(l, marker)
		if i < 0 {
			return "", false
		}
		return l[i+len(marker):], true
	}
	eventually(t, 20*time.Second, "B re-checks the new base and keeps the conflict", func() bool {
		rest, ok := after(daemonB.log.String(), "base fetched")
		if !ok {
			return false
		}
		rest, ok = after(rest, "snapshot downloaded")
		return ok && strings.Contains(rest, "conflicts=1")
	})
	// A fresh session gets the conflict naming A's lines (SessionStart delivers it).
	// The rebase passes through a detached HEAD, so a conflict seen in the log
	// can still be replaced by the settled one: poll with a fresh session each time.
	attempt := 0
	eventually(t, 20*time.Second, "a fresh session after the base move gets the conflict", func() bool {
		attempt++
		out := runHook(t, homeB, "SessionStart", map[string]any{"session_id": fmt.Sprintf("sB2-%d", attempt), "cwd": b})
		return strings.Contains(out, "  | A two")
	})
	eventually(t, 20*time.Second, "serve still holds the conflict", oneConflict)

	// 4. A secret is never uploaded, and never logged. Only the pair's file
	// goes up (creds.txt stays home), so the key goes in it.
	secret := "AKIA" + strings.Repeat("Q", 16)
	testgit.WriteFile(t, filepath.Join(a, "creds.txt"), "key = "+secret+"\n")
	testgit.WriteFile(t, filepath.Join(a, "multi.txt"), multi("A two")+"key = "+secret+"\n")
	eventually(t, 20*time.Second, "daemon A refuses to upload the secret", func() bool {
		l := daemonA.log.String()
		return strings.Contains(l, "snapshot not uploaded") && strings.Contains(l, "path=multi.txt") && strings.Contains(l, "rule=aws-access-key")
	})
	for name, p := range map[string]*proc{"A": daemonA, "B": daemonB, "serve1": serve1, "serve2": serve2} {
		if strings.Contains(p.log.String(), secret) {
			t.Fatalf("%s log holds the secret value", name)
		}
	}
	if strings.Contains(rawState(), secret) {
		t.Fatal("/v1/state holds the secret value")
	}
}

func TestE2EServeRestart(t *testing.T) {
	w := overlapWorld(t)
	w.serve.stop()
	if _, err := state(w.addr); err == nil {
		t.Fatal("serve still answering after stop")
	}
	startServe(t, w.addr)
	eventually(t, 10*time.Second, "restarted serve has the warning again", func() bool {
		d, err := state(w.addr)
		return err == nil && len(d.Warnings) == 1
	})
	eventually(t, 10*time.Second, "a new session gets the warning on SessionStart", func() bool {
		return strings.Contains(runHook(t, w.homeA, "SessionStart", map[string]any{"session_id": "sA2", "cwd": w.a}), "shared.go")
	})
}

func TestE2EDaemonRestartKeepsID(t *testing.T) {
	w := overlapWorld(t)
	ids := func() []string {
		d, _ := state(w.addr)
		var out []string
		for _, ws := range d.Workstreams {
			out = append(out, string(ws.ID))
		}
		sort.Strings(out)
		return out
	}
	before := ids()
	idFile, _ := os.ReadFile(filepath.Join(w.homeA, "daemon_id"))

	w.daemonA.stop()
	startDaemon(t, w.homeA, w.addr)
	time.Sleep(4 * time.Second) // two resend periods of the restarted daemon
	if after := ids(); !slices.Equal(after, before) || len(after) != 2 {
		t.Fatalf("workstreams after daemon restart = %v, before = %v (ghost set?)", after, before)
	}
	if again, _ := os.ReadFile(filepath.Join(w.homeA, "daemon_id")); !bytes.Equal(again, idFile) {
		t.Fatalf("daemon_id changed across restart: %q -> %q", idFile, again)
	}
}

func TestHookDegradedLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("latency benchmark")
	}
	home, tmp := shortDir(t), t.TempDir()
	var ms []float64
	for i := range 50 {
		cmd := exec.Command(bin, "hook", "PreToolUse")
		cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home, "HOME="+home, "TMPDIR="+tmp)
		cmd.Stdin = strings.NewReader(`{"session_id":"lat","tool_name":"Bash"}`)
		t0 := time.Now()
		out, err := cmd.Output()
		ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 0; strings.Contains(string(out), "systemMessage") != want {
			t.Fatalf("run %d output %s (systemMessage expected only on the first run)", i, out)
		}
	}
	sort.Float64s(ms)
	p95 := ms[len(ms)*95/100]
	// Shared hosted runners are slower and noisier; the 20 ms product target is checked locally.
	budget := 20.0
	if os.Getenv("CI") != "" {
		budget = 60
	}
	t.Logf("degraded hook: p50 %.1f ms, p95 %.1f ms, budget %.0f ms", ms[len(ms)/2], p95, budget)
	if p95 > budget {
		t.Fatalf("degraded hook p95 = %.1f ms, budget %.0f ms", p95, budget)
	}
}

// TestHookKickstartsLaunchAgent: when the daemon does not answer, the
// first hook of a session points to doctor and asks launchd to start the
// LaunchAgent (systemd its unit, on Linux), once, without waiting for
// launchctl or systemctl and without touching its own stdout. With nothing
// installed it asks nothing.
func TestHookKickstartsLaunchAgent(t *testing.T) {
	skipUnlessService(t)
	const message = "Dugout: the local daemon is not responding, so this session goes on without shared awareness. Run dugout doctor to see what is missing."
	tool, want := "launchctl", "kickstart gui/"+fmt.Sprint(os.Getuid())+"/com.dugout.daemon\n"
	if runtime.GOOS == "linux" {
		tool, want = "systemctl", "--user --no-block start dugout.service\n"
	}
	for _, installed := range []bool{true, false} {
		home := shortDir(t)
		lc, log := fakeTool(t, tool, "sleep 2\n") // a slow launchctl must not hold the hook
		if installed {
			writeService(t, home, bin, "http://x", "/usr/bin:/bin")
		}
		env := []string{"HOME=" + home, statedir.EnvHome + "=" + home, "TMPDIR=" + t.TempDir(), "PATH=" + lc + ":/bin:/usr/bin"}
		hook := func() (map[string]any, time.Duration) {
			t.Helper()
			cmd := exec.Command(bin, "hook", "SessionStart")
			cmd.Env = env
			cmd.Stdin = strings.NewReader(`{"session_id":"sK","cwd":"/x"}`)
			t0 := time.Now()
			out, err := cmd.Output()
			took := time.Since(t0)
			var m map[string]any
			if err != nil || json.Unmarshal(out, &m) != nil {
				t.Fatalf("installed=%v: hook: %v, stdout %q", installed, err, out)
			}
			return m, took
		}
		first, took := hook()
		if first["systemMessage"] != message || len(first) != 1 {
			t.Fatalf("installed=%v: first degraded hook = %v", installed, first)
		}
		if took > time.Second {
			t.Fatalf("installed=%v: the hook waited %s for %s", installed, took, tool)
		}
		if again, _ := hook(); len(again) != 0 {
			t.Fatalf("installed=%v: second hook of the session = %v, want {}", installed, again)
		}
		calls := ""
		if installed {
			calls = want
			eventually(t, 5*time.Second, tool+" kickstart", func() bool { _, err := os.Stat(log); return err == nil })
		}
		time.Sleep(300 * time.Millisecond) // time enough for a wrong second call to show
		if b, _ := os.ReadFile(log); string(b) != calls {
			t.Fatalf("installed=%v: %s calls = %q, want %q", installed, tool, b, calls)
		}
	}
}

func TestServeNonLoopbackNeedsToken(t *testing.T) {
	t.Setenv(wire.EnvToken, "")
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"serve", "-addr", "0.0.0.0:0"}, nil, &bytes.Buffer{}, &stderr) }()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), wire.EnvToken) {
			t.Fatalf("serve on 0.0.0.0 without a token: code %d, stderr %q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve started on 0.0.0.0 without a token")
	}
}

func TestE2EAllowHost(t *testing.T) {
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	start(t, []string{wire.EnvToken + "=s3cret"}, "serve", "-addr", addr, "-allow-host", "office.vpn")
	get := func(host, auth string) int {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/state", nil)
		req.Host = host
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	eventually(t, 5*time.Second, "serve answers", func() bool { return get(addr, "s3cret") == http.StatusOK })
	for _, c := range []struct {
		host, auth string
		want       int
	}{
		{"office.vpn:" + port, "s3cret", http.StatusOK},
		{"other.vpn:" + port, "s3cret", http.StatusForbidden},
		{"office.vpn:" + port, "", http.StatusUnauthorized},
	} {
		if got := get(c.host, c.auth); got != c.want {
			t.Errorf("Host %s token %q: status %d, want %d", c.host, c.auth, got, c.want)
		}
	}
}

func TestUsage(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"bogus"}, nil, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("unknown subcommand: code %d stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-server URL|off") {
		t.Fatalf("usage does not name -server URL|off: %q", stderr.String())
	}
	if code := run(nil, nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("no subcommand: code %d", code)
	}
}

func TestTrackCommand(t *testing.T) {
	f := testgit.NewFixture(t)
	home := shortDir(t)
	startDaemon(t, home, freeAddr(t))
	cmd := exec.Command(bin, "track", f.Clone)
	cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "tracked "+f.Clone) {
		t.Fatalf("dugout track: %v %s", err, out)
	}
	cmd = exec.Command(bin, "track", shortDir(t))
	cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home)
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not a git repo") {
		t.Fatalf("dugout track on a non-repo: %v %s", err, out)
	}
}

func runAdmin(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestAdminInit(t *testing.T) {
	path := filepath.Join(shortDir(t), "serve.json")
	out, errOut, code := runAdmin(t, "admin", "init", "-state", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	m := regexp.MustCompile(`^admin token \(shown only once\): (dgt_[A-Za-z0-9_-]{43})\n$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("stdout = %q", out)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, %v", fi, err)
	}
	s, err := serve.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 1 || s.AdminHash != serve.HashToken(m[1]) {
		t.Fatalf("state = %+v", s)
	}
	if strings.Contains(errOut, m[1]) {
		t.Fatal("token leaked to stderr")
	}
}

func TestAdminInitRefusesExisting(t *testing.T) {
	path := filepath.Join(shortDir(t), "serve.json")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runAdmin(t, "admin", "init", "-state", path)
	if code != 1 || out != "" || !strings.Contains(errOut, "the state file already exists: "+path) {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep" {
		t.Fatalf("file overwritten: %q", b)
	}
}

func TestServeStateMissingFails(t *testing.T) {
	path := filepath.Join(shortDir(t), "nope.json")
	_, errOut, code := runAdmin(t, "serve", "-addr", freeAddr(t), "-state", path)
	if code != 1 || !strings.Contains(errOut, path) {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// TestServeStateNeedsNoSharedToken: with -state each person brings a token,
// so serve listens beyond loopback without $DUGOUT_TOKEN. 192.0.2.1
// (TEST-NET-1) is never a local address: serve gets past the token check and
// fails to bind, and the test never listens on a public interface.
func TestServeStateNeedsNoSharedToken(t *testing.T) {
	path := filepath.Join(shortDir(t), "serve.json")
	if _, errOut, code := runAdmin(t, "admin", "init", "-state", path); code != 0 {
		t.Fatalf("admin init: exit %d: %s", code, errOut)
	}
	t.Setenv(wire.EnvToken, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "serve", "-addr", "192.0.2.1:7457", "-state", path).CombinedOutput()
	if strings.Contains(string(out), wire.EnvToken) {
		t.Fatalf("serve -state asked for the shared token: %s", out)
	}
	// A timeout means serve did bind there: past the token check too.
	if ctx.Err() == nil && (err == nil || !strings.Contains(string(out), "192.0.2.1:7457")) {
		t.Fatalf("serve -state on 192.0.2.1: %v, want a bind error: %s", err, out)
	}
}

// TestE2EServeState: serve -state takes the admin token on /v1/admin and a
// person's token elsewhere, and not the shared $DUGOUT_TOKEN.
func TestE2EServeState(t *testing.T) {
	path := filepath.Join(shortDir(t), "serve.json")
	out, errOut, code := runAdmin(t, "admin", "init", "-state", path)
	if code != 0 {
		t.Fatalf("admin init: exit %d: %s", code, errOut)
	}
	adminTok := strings.TrimSpace(out[strings.LastIndex(out, " ")+1:])
	addr := freeAddr(t)
	p := start(t, []string{wire.EnvToken + "=" + e2eToken}, "serve", "-addr", addr, "-state", path)
	call := func(method, route, tok, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+addr+route, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	eventually(t, 5*time.Second, "serve answers", func() bool {
		code, _ := call(http.MethodGet, "/v1/admin/tokens", adminTok, "")
		return code == http.StatusOK
	})
	code, body := call(http.MethodPost, "/v1/admin/tokens", adminTok, `{"name":"ana"}`)
	var issued struct{ Token string }
	if err := json.Unmarshal([]byte(body), &issued); code != http.StatusCreated || err != nil {
		t.Fatalf("issue: status %d %s", code, body)
	}
	if code, body := call(http.MethodGet, "/v1/state", issued.Token, ""); code != http.StatusOK {
		t.Fatalf("ana's token: status %d %s", code, body)
	}
	if code, _ := call(http.MethodGet, "/v1/state", e2eToken, ""); code != http.StatusUnauthorized {
		t.Fatalf("the shared token with -state: status %d, want 401", code)
	}
	if code, _ := call(http.MethodGet, "/v1/state", adminTok, ""); code != http.StatusUnauthorized {
		t.Fatalf("the admin token on /v1/state: status %d, want 401", code)
	}
	if strings.Contains(p.log.String(), issued.Token) || strings.Contains(p.log.String(), adminTok) {
		t.Fatalf("serve logged a token:\n%s", p.log)
	}
}

// TestE2ETeamPause: serve -state with an admin and one person, two daemons
// (two machines) with real hooks. A pause by HTTP silences both, with one
// notice per session; resuming brings back the overlap that was there all along.
func TestE2ETeamPause(t *testing.T) {
	path := filepath.Join(shortDir(t), "serve.json")
	out, errOut, code := runAdmin(t, "admin", "init", "-state", path)
	if code != 0 {
		t.Fatalf("admin init: exit %d: %s", code, errOut)
	}
	adminTok := strings.TrimSpace(out[strings.LastIndex(out, " ")+1:])
	addr := freeAddr(t)
	start(t, nil, "serve", "-addr", addr, "-state", path)
	call := func(method, route, tok, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+addr+route, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	eventually(t, 5*time.Second, "serve answers", func() bool {
		code, _ := call(http.MethodGet, "/v1/admin/tokens", adminTok, "")
		return code == http.StatusOK
	})
	code, body := call(http.MethodPost, "/v1/admin/tokens", adminTok, `{"name":"ana"}`)
	var issued struct{ Token string }
	if err := json.Unmarshal([]byte(body), &issued); code != http.StatusCreated || err != nil {
		t.Fatalf("issue: status %d %s", code, body)
	}
	// The fixture's repo has to be on the allowlist for serve to warn.
	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	a, b := f.Clone, f.AddWorktree(t, "wtb", "feat-b")
	if code, body := call(http.MethodPost, "/v1/admin/repos", adminTok, fmt.Sprintf(`{"repo":%q}`, f.Origin)); code != http.StatusNoContent {
		t.Fatalf("allow repo: status %d %s", code, body)
	}
	homeA, homeB := shortDir(t), shortDir(t)
	for _, home := range []string{homeA, homeB} {
		start(t, []string{statedir.EnvHome + "=" + home, wire.EnvToken + "=" + issued.Token},
			"daemon", "-server", "http://"+addr, "-poll", "300ms", "-resend", "2s")
		waitDaemonSocket(t, home)
	}
	sessions := []struct{ home, id, dir string }{{homeA, "sA", a}, {homeB, "sB", b}}
	for _, s := range sessions {
		runHook(t, s.home, "SessionStart", map[string]any{"session_id": s.id, "cwd": s.dir})
		testgit.WriteFile(t, filepath.Join(s.dir, "shared.go"), "edited in "+filepath.Base(s.dir)+"\n")
	}
	const notice = `{"systemMessage":"Dugout is paused for the whole team."}`
	prompt := func(s struct{ home, id, dir string }) string {
		return runHook(t, s.home, "UserPromptSubmit", map[string]any{"session_id": s.id, "cwd": s.dir, "prompt": "carry on"})
	}
	if code, body := call(http.MethodPost, "/v1/admin/pause", adminTok, `{"paused":true}`); code != http.StatusNoContent {
		t.Fatalf("pause: status %d %s", code, body)
	}
	for _, s := range sessions {
		eventually(t, 5*time.Second, s.id+" is told of the pause", func() bool { return prompt(s) == notice })
		if out := prompt(s); out != "{}" {
			t.Fatalf("%s second hook in the pause = %s, want {}", s.id, out)
		}
	}
	if code, body := call(http.MethodPost, "/v1/admin/pause", adminTok, `{"paused":false}`); code != http.StatusNoContent {
		t.Fatalf("resume: status %d %s", code, body)
	}
	for _, s := range sessions {
		eventually(t, 10*time.Second, s.id+" hears the overlap after the pause", func() bool {
			return strings.Contains(prompt(s), "shared.go")
		})
	}
}

// adminWorld is a serve -state on a free port plus the admin token that
// dugout admin init printed.
type adminWorld struct {
	addr, adminTok string
	dir            string // holds serve.json
	srv            *proc
}

func startAdminServe(t *testing.T) *adminWorld {
	t.Helper()
	dir := shortDir(t)
	path := filepath.Join(dir, "serve.json")
	out, errOut, code := runAdmin(t, "admin", "init", "-state", path)
	if code != 0 {
		t.Fatalf("admin init: exit %d: %s", code, errOut)
	}
	w := &adminWorld{addr: freeAddr(t), dir: dir, adminTok: strings.TrimSpace(out[strings.LastIndex(out, " ")+1:])}
	w.srv = start(t, nil, "serve", "-addr", w.addr, "-state", path)
	eventually(t, 5*time.Second, "serve answers", func() bool {
		req, _ := http.NewRequest(http.MethodGet, "http://"+w.addr+"/v1/admin/tokens", nil)
		req.Header.Set("Authorization", "Bearer "+w.adminTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return w
}

// admin runs `dugout admin ...` against the world with the given admin token.
func (w *adminWorld) admin(t *testing.T, tok string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runAdminEnv(t, []string{envServer + "=http://" + w.addr, "DUGOUT_ADMIN_TOKEN=" + tok}, append([]string{"admin"}, args...)...)
}

func runAdminEnv(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestAdminTokenIssueRevokeList(t *testing.T) {
	w := startAdminServe(t)
	out, errOut, code := w.admin(t, w.adminTok, "token", "issue", "ana")
	m := regexp.MustCompile(`^token for ana \(shown only once\): (dgt_[A-Za-z0-9_-]{43})\n$`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("issue: code=%d out=%q err=%q", code, out, errOut)
	}
	if out, _, code := w.admin(t, w.adminTok, "token", "issue", "ana"); code != 1 || out != "" {
		t.Fatalf("a repeated name: code=%d out=%q", code, out)
	}
	if _, errOut, code := w.admin(t, w.adminTok, "token", "issue", "a b!"); code != 1 || !strings.Contains(errOut, "invalid name") {
		t.Fatalf("an invalid name: code=%d err=%q", code, errOut)
	}
	if out, _, _ := w.admin(t, w.adminTok, "token", "list"); out != "ana\n" {
		t.Fatalf("list = %q", out)
	}
	if out, _, code := w.admin(t, w.adminTok, "token", "revoke", "ana"); code != 0 || out != "token for ana revoked\n" {
		t.Fatalf("revoke: code=%d out=%q", code, out)
	}
	if out, _, _ := w.admin(t, w.adminTok, "token", "list"); out != "ana (revoked)\n" {
		t.Fatalf("list after revoke = %q", out)
	}
	if _, errOut, code := w.admin(t, w.adminTok, "token", "revoke", "nobody"); code != 1 || !strings.Contains(errOut, "there is no token for nobody") {
		t.Fatalf("revoke unknown: code=%d err=%q", code, errOut)
	}
	if strings.Contains(w.srv.log.String(), m[1]) || strings.Contains(w.srv.log.String(), w.adminTok) {
		t.Fatal("serve logged a token")
	}
}

func TestAdminRepoAddRemoveList(t *testing.T) {
	w := startAdminServe(t)
	if out, errOut, code := w.admin(t, w.adminTok, "repo", "add", "git@github.com:Acme/App.git"); code != 0 || out != "repo github.com/acme/app allowed\n" {
		t.Fatalf("add: code=%d out=%q err=%q", code, out, errOut)
	}
	if out, _, _ := w.admin(t, w.adminTok, "repo", "list"); out != "github.com/acme/app\n" {
		t.Fatalf("list = %q", out)
	}
	if _, errOut, code := w.admin(t, w.adminTok, "repo", "add", " "); code != 1 || errOut == "" {
		t.Fatalf("add blank: code=%d err=%q", code, errOut)
	}
	if out, _, code := w.admin(t, w.adminTok, "repo", "remove", "https://github.com/Acme/App.git"); code != 0 || out != "repo github.com/acme/app removed\n" {
		t.Fatalf("remove: code=%d out=%q", code, out)
	}
	if out, _, _ := w.admin(t, w.adminTok, "repo", "list"); out != "" {
		t.Fatalf("list after remove = %q", out)
	}
}

// TestAdminRepoMembers: a repo added with members, granted and ungranted,
// and listed with them.
func TestAdminRepoMembers(t *testing.T) {
	w := startAdminServe(t)
	if out, errOut, code := w.admin(t, w.adminTok, "repo", "add", "git@github.com:Acme/App.git", "ana", "Beto"); code != 0 ||
		out != "repo github.com/acme/app allowed, only for: ana, beto\n" {
		t.Fatalf("add with members: code=%d out=%q err=%q", code, out, errOut)
	}
	w.admin(t, w.adminTok, "repo", "add", "github.com/acme/api")
	if out, _, code := w.admin(t, w.adminTok, "repo", "grant", "github.com/acme/app", "dani"); code != 0 ||
		out != "dani now sees github.com/acme/app (members: ana, beto, dani)\n" {
		t.Fatalf("grant: code=%d out=%q", code, out)
	}
	if out, _, code := w.admin(t, w.adminTok, "repo", "ungrant", "https://github.com/Acme/App", "beto"); code != 0 ||
		out != "beto no longer sees github.com/acme/app (members: ana, dani)\n" {
		t.Fatalf("ungrant: code=%d out=%q", code, out)
	}
	if out, _, _ := w.admin(t, w.adminTok, "repo", "list"); out != "github.com/acme/api\ngithub.com/acme/app, only for: ana, dani\n" {
		t.Fatalf("list = %q", out)
	}
	if _, errOut, code := w.admin(t, w.adminTok, "repo", "grant", "github.com/acme/app", "a b!"); code != 1 || !strings.Contains(errOut, "invalid name") {
		t.Fatalf("an invalid name: code=%d err=%q", code, errOut)
	}
	if _, errOut, code := w.admin(t, w.adminTok, "repo", "grant", "github.com/acme/nope", "ana"); code != 1 || !strings.Contains(errOut, "github.com/acme/nope is not allowed") {
		t.Fatalf("a repo not allowed: code=%d err=%q", code, errOut)
	}
	if _, errOut, code := w.admin(t, w.adminTok, "repo", "grant", "github.com/acme/app"); code != 2 || !strings.Contains(errOut, "repo grant|ungrant") {
		t.Fatalf("grant without a person: code=%d err=%q", code, errOut)
	}
}

func TestAdminPauseResume(t *testing.T) {
	w := startAdminServe(t)
	if out, errOut, code := w.admin(t, w.adminTok, "pause"); code != 0 || out != "Dugout paused for the whole team\n" {
		t.Fatalf("pause: code=%d out=%q err=%q", code, out, errOut)
	}
	if out, errOut, code := w.admin(t, w.adminTok, "resume"); code != 0 || out != "Dugout resumed for the whole team\n" {
		t.Fatalf("resume: code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestAdminWithoutTokenExplains(t *testing.T) {
	w := startAdminServe(t)
	out, errOut, code := w.admin(t, "", "token", "list")
	if code != 1 || out != "" || !strings.Contains(errOut, "missing DUGOUT_ADMIN_TOKEN (the token dugout admin init printed)") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestAdminWrongToken(t *testing.T) {
	w := startAdminServe(t)
	out, errOut, code := w.admin(t, "dgt_wrong", "token", "list")
	if code != 1 || out != "" || !strings.Contains(errOut, "serve rejected the admin token") || strings.Contains(errOut, "dgt_wrong") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// TestAdminShowsServeMessageOn5xx: a failing save shows serve's own words, not
// just the status.
func TestAdminShowsServeMessageOn5xx(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	w := startAdminServe(t)
	if err := os.Chmod(w.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(w.dir, 0o700) })
	out, errOut, code := w.admin(t, w.adminTok, "token", "issue", "ana")
	if code != 1 || out != "" || !strings.Contains(errOut, "serve answered 500: could not save the state file") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// TestAdminWarnsWithoutTLS: an http:// server that is not loopback gets a
// warning on stderr, and the command goes on.
func TestAdminWarnsWithoutTLS(t *testing.T) {
	server := "http://dugout.invalid:7457"
	_, errOut, code := runAdminEnv(t, []string{envServer + "=" + server, "DUGOUT_ADMIN_TOKEN=x"}, "admin", "pause")
	if code != 1 || !strings.Contains(errOut, "warning: "+server+" has no TLS; the token travels in the clear\n") || !strings.Contains(errOut, "serve is not responding") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	w := startAdminServe(t)
	if _, errOut, code := w.admin(t, w.adminTok, "pause"); code != 0 || strings.Contains(errOut, "warning") {
		t.Fatalf("loopback: code=%d err=%q", code, errOut)
	}
}

func TestAdminServerDown(t *testing.T) {
	addr := freeAddr(t)
	out, errOut, code := runAdminEnv(t, []string{envServer + "=http://" + addr, "DUGOUT_ADMIN_TOKEN=x"}, "admin", "pause")
	if code != 1 || out != "" || !strings.Contains(errOut, "serve is not responding at http://"+addr+": ") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// TestAdminRepoAwkwardIdentities: identities a URL path cannot carry
// round-trip through add and remove.
func TestAdminRepoAwkwardIdentities(t *testing.T) {
	w := startAdminServe(t)
	for _, repo := range []string{"github.com/acme/app", "github.com/acme/my app", "github.com/acme/a#b", "github.com/acme/a?b", "github.com/acme/a%41", "/srv/git/local repo"} {
		if _, errOut, code := w.admin(t, w.adminTok, "repo", "add", repo); code != 0 {
			t.Fatalf("add %q: code=%d err=%q", repo, code, errOut)
		}
		if out, _, _ := w.admin(t, w.adminTok, "repo", "list"); strings.TrimSpace(out) == "" {
			t.Fatalf("add %q left the list empty", repo)
		}
		if out, errOut, code := w.admin(t, w.adminTok, "repo", "remove", repo); code != 0 || !strings.HasPrefix(out, "repo ") {
			t.Fatalf("remove %q: code=%d out=%q err=%q", repo, code, out, errOut)
		}
		if out, _, _ := w.admin(t, w.adminTok, "repo", "list"); out != "" {
			t.Fatalf("after removing %q the list is %q", repo, out)
		}
	}
	if _, errOut, code := w.admin(t, w.adminTok, "repo", "add", "/"); code != 1 || !strings.Contains(errOut, "invalid repo") {
		t.Fatalf("add /: code=%d err=%q", code, errOut)
	}
}

// runScript runs plugin/bin/dugout-run under sh with a controlled PATH: pathDirs
// plus /bin:/usr/bin for sh and cat, never a real dugout.
func runScript(t *testing.T, pluginRoot string, pathDirs []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runScriptHome(t, "", pluginRoot, pathDirs, stdin, args...)
}

// runScriptHome is runScript with HOME set (empty leaves it unset).
func runScriptHome(t *testing.T, home, pluginRoot string, pathDirs []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	env := []string{
		"PATH=" + strings.Join(append(slices.Clone(pathDirs), "/bin", "/usr/bin"), ":"),
		"CLAUDE_PLUGIN_ROOT=" + pluginRoot,
	}
	if home != "" {
		env = append(env, "HOME="+home)
	}
	return runScriptEnv(t, env, stdin, args...)
}

// runScriptEnv runs plugin/bin/dugout-run under sh, as the hooks do, with
// exactly env.
func runScriptEnv(t *testing.T, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runScriptIn(t, "", env, stdin, args...)
}

// runScriptIn is runScriptEnv from the directory dir ("" is the test's own).
func runScriptIn(t *testing.T, dir string, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	script, err := filepath.Abs("../../plugin/bin/dugout-run")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// fakeDugout writes an executable `dugout` into dir that echoes its arguments
// and stdin, so tests can tell which binary the script picked.
func fakeDugout(t *testing.T, dir, tag string) {
	t.Helper()
	body := "#!/bin/sh\necho \"" + tag + " $*\"\ncat\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dugout"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHookScriptFailOpen(t *testing.T) {
	out, _, code := runScript(t, t.TempDir(), nil, "{}", "hook", "PreToolUse")
	if code != 0 || strings.TrimSpace(out) != "{}" {
		t.Fatalf("code=%d out=%q, want 0 and {}", code, out)
	}
}

func TestHookScriptUsesPath(t *testing.T) {
	bin := t.TempDir()
	fakeDugout(t, bin, "path")
	out, _, code := runScript(t, t.TempDir(), []string{bin}, "payload", "hook", "PreToolUse")
	if code != 0 || out != "path hook PreToolUse\npayload" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestHookScriptUsesPluginBinary(t *testing.T) {
	root := t.TempDir()
	fakeDugout(t, filepath.Join(root, "bin"), "plugin")
	out, _, code := runScript(t, root, nil, "payload", "hook", "PreToolUse")
	if code != 0 || out != "plugin hook PreToolUse\npayload" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// TestHookScriptIgnoresRelativePathEntries: a `.`, an empty entry (also the
// current directory) or any other relative entry in the PATH never runs the
// dugout that a repo ships, which every hook would otherwise run.
func TestHookScriptIgnoresRelativePathEntries(t *testing.T) {
	repo := t.TempDir()
	fakeDugout(t, repo, "foreign")
	fakeDugout(t, filepath.Join(repo, "sub"), "foreign-sub")
	for _, path := range []string{".", "", ".:", ":/bin", "sub", "./sub", "../" + filepath.Base(repo)} {
		env := []string{"PATH=" + path + ":/bin:/usr/bin", "CLAUDE_PLUGIN_ROOT=" + t.TempDir(), "HOME=" + t.TempDir()}
		for _, dir := range []string{repo, filepath.Join(repo, "sub")} {
			out, errOut, code := runScriptIn(t, dir, env, "payload", "hook", "PreToolUse")
			if code != 0 || strings.TrimSpace(out) != "{}" || errOut != "" {
				t.Errorf("PATH=%q in %s: code=%d out=%q err=%q, want the fail-open {}", path, dir, code, out, errOut)
			}
		}
	}
}

// TestHookScriptPrefersLinkOverPath: the binary dugout install linked wins
// over whatever the PATH says, and the plugin's own wins over the link; the
// PATH is the last resort.
func TestHookScriptPrefersLinkOverPath(t *testing.T) {
	home, onPath, plugin := t.TempDir(), t.TempDir(), t.TempDir()
	fakeDugout(t, onPath, "path")
	run := func() string {
		env := []string{"PATH=" + onPath + ":/bin:/usr/bin", "CLAUDE_PLUGIN_ROOT=" + plugin, "HOME=" + home}
		out, _, code := runScriptEnv(t, env, "payload", "hook", "PreToolUse")
		if code != 0 {
			t.Fatalf("code=%d out=%q", code, out)
		}
		return strings.SplitN(out, " ", 2)[0]
	}
	if got := run(); got != "path" {
		t.Fatalf("with no link and no plugin binary the script ran %q, want path", got)
	}
	real := t.TempDir()
	fakeDugout(t, real, "linked")
	link := filepath.Join(home, ".local", "state", "dugout", "bin", "dugout")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "dugout"), link); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "linked" {
		t.Fatalf("with a link and a PATH dugout the script ran %q, want linked", got)
	}
	fakeDugout(t, filepath.Join(plugin, "bin"), "plugin")
	if got := run(); got != "plugin" {
		t.Fatalf("with a plugin binary too the script ran %q, want plugin", got)
	}
}

func TestRunScriptOtherSubcommandFails(t *testing.T) {
	out, errOut, code := runScript(t, t.TempDir(), nil, "", "mcp")
	want := "dugout is not installed: see docs/install.md in github.com/joacofg/dugout"
	if code != 1 || out != "" || strings.TrimSpace(errOut) != want {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestMarketplaceManifest(t *testing.T) {
	raw, err := os.ReadFile("../../.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "dugout" || len(m.Plugins) != 1 || m.Plugins[0].Name != "dugout" || m.Plugins[0].Source != "./plugin" {
		t.Fatalf("unexpected marketplace: %+v", m)
	}
	pj, err := os.ReadFile(filepath.Join("../..", m.Plugins[0].Source, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code caches a plugin by its version: one that does not change
	// with the binary leaves people on the old plugin.
	var p struct{ Name, Version string }
	if err := json.Unmarshal(pj, &p); err != nil || p.Name != "dugout" || p.Version != version {
		t.Fatalf("plugin.json: name=%q version=%q err=%v, want dugout %s", p.Name, p.Version, err, version)
	}
}

func TestHooksUseRunScript(t *testing.T) {
	raw, err := os.ReadFile("../../plugin/hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	n := 0
	for event, groups := range h.Hooks {
		for _, g := range groups {
			for _, c := range g.Hooks {
				n++
				want := "sh \"${CLAUDE_PLUGIN_ROOT}/bin/dugout-run\" hook " + event
				if c.Command != want {
					t.Errorf("%s: command %q, want %q", event, c.Command, want)
				}
			}
		}
	}
	if n != 6 {
		t.Errorf("found %d hook commands, want 6", n)
	}
}

// fakeLaunchctl installs a launchctl that appends its arguments to a log file
// and returns the PATH dir and the log path. With failBootout, bootout exits 1.
func fakeLaunchctl(t *testing.T, failBootout bool) (dir, log string) {
	t.Helper()
	script := ""
	if failBootout {
		script = "[ \"$1\" = bootout ] && exit 1\n"
	}
	return fakeLaunchctlScript(t, script)
}

// fakeLaunchctlScript is fakeLaunchctl running script after logging; the
// script sees the log's path in $LOG.
func fakeLaunchctlScript(t *testing.T, script string) (dir, log string) {
	t.Helper()
	return fakeTool(t, "launchctl", script)
}

// fakeTool puts a name (launchctl, systemctl) in a temp dir that appends its
// arguments to a log file, runs script and exits 0. It returns the PATH dir
// and the log path; the script sees the log's path in $LOG.
func fakeTool(t *testing.T, name, script string) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(t.TempDir(), name+".log")
	writeFakeTool(t, dir, name, log, script)
	return dir, log
}

func writeFakeTool(t *testing.T, dir, name, log, script string) {
	t.Helper()
	body := "#!/bin/sh\nLOG=" + log + "\necho \"$*\" >>\"$LOG\"\n" + script + "exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runInstall runs the built dugout with HOME and PATH confined to temp dirs.
func runInstall(t *testing.T, home, pathDir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + pathDir + ":/bin:/usr/bin"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func skipUnlessDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("install and uninstall only work on macOS")
	}
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestInstallTwice(t *testing.T) {
	skipUnlessDarwin(t)
	home := t.TempDir()
	lc, log := fakeLaunchctl(t, true)
	uid := fmt.Sprint(os.Getuid())
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.dugout.daemon.plist")
	for i := 0; i < 2; i++ {
		out, errOut, code := runInstall(t, home, lc, "install", "-server", "http://127.0.0.1:7457")
		if code != 0 || out != "Dugout installed: the daemon starts with your session and talks to http://127.0.0.1:7457.\n" {
			t.Fatalf("run %d: code=%d out=%q err=%q", i, code, out, errOut)
		}
	}
	want := []string{
		"bootout gui/" + uid + "/com.dugout.daemon", "bootstrap gui/" + uid + " " + plist,
		"bootout gui/" + uid + "/com.dugout.daemon", "bootstrap gui/" + uid + " " + plist,
	}
	if got := readLog(t, log); !slices.Equal(got, want) {
		t.Fatalf("launchctl calls = %q, want %q", got, want)
	}
	raw, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := filepath.EvalSymlinks(bin)
	for _, s := range []string{"<string>" + exe + "</string>", "<string>http://127.0.0.1:7457</string>", "<string>json</string>",
		"<key>PATH</key>\n\t\t<string>" + lc + ":/bin:/usr/bin</string>"} {
		if !strings.Contains(string(raw), s) {
			t.Fatalf("plist lacks %s:\n%s", s, raw)
		}
	}
	if st, err := os.Stat(plist); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(home, "Library", "Logs", "dugout")); err != nil || !st.IsDir() {
		t.Fatalf("log dir: %v %v", st, err)
	}
}

// TestInstallRetriesBootstrap: right after a bootout launchd can refuse the
// bootstrap (exit 5, the old job is still going away). install tries three
// times and, if all fail, exits 1 with the last error. It runs in-process so
// the delay between tries can be zero.
func TestInstallRetriesBootstrap(t *testing.T) {
	skipUnlessDarwin(t)
	old := bootstrapDelay
	bootstrapDelay = 0
	t.Cleanup(func() { bootstrapDelay = old })
	for _, tc := range []struct{ fails, code, bootstraps int }{{1, 0, 2}, {3, 1, 3}} {
		home := t.TempDir()
		lc, log := fakeLaunchctlScript(t, `if [ "$1" = bootstrap ]; then
  n=$(grep -c '^bootstrap' "$LOG")
  [ "$n" -le `+fmt.Sprint(tc.fails)+` ] && { echo "Bootstrap failed: 5: Input/output error (attempt $n)" >&2; exit 5; }
fi
`)
		t.Setenv("HOME", home)
		t.Setenv("PATH", lc+":/bin:/usr/bin")
		var out, errb bytes.Buffer
		code := run([]string{"install", "-server", "http://127.0.0.1:7457"}, strings.NewReader(""), &out, &errb)
		if code != tc.code {
			t.Fatalf("%d failures: exit %d, want %d\n%s%s", tc.fails, code, tc.code, out.String(), errb.String())
		}
		n := 0
		for _, l := range readLog(t, log) {
			if strings.HasPrefix(l, "bootstrap ") {
				n++
			}
		}
		if n != tc.bootstraps {
			t.Fatalf("%d failures: %d bootstraps, want %d: %q", tc.fails, n, tc.bootstraps, readLog(t, log))
		}
		if tc.code != 0 {
			if e := errb.String(); !strings.Contains(e, "launchctl bootstrap: exit status 5: Bootstrap failed: 5: Input/output error (attempt 3)") || strings.Contains(e, "attempt 1") {
				t.Fatalf("stderr = %q, want only the last error", e)
			}
		}
	}
}

func TestInstallCreatesBinLink(t *testing.T) {
	skipUnlessDarwin(t)
	home := t.TempDir()
	lc, _ := fakeLaunchctl(t, false)
	link := filepath.Join(home, "Library", "Application Support", "dugout", "bin", "dugout")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/old", link); err != nil { // a stale link is replaced
		t.Fatal(err)
	}
	if _, errOut, code := runInstall(t, home, lc, "install", "-server", "http://127.0.0.1:1"); code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	exe, _ := filepath.EvalSymlinks(bin)
	if got, err := os.Readlink(link); err != nil || got != exe {
		t.Fatalf("link -> %q, %v; want %q", got, err, exe)
	}
}

func TestHookScriptUsesInstalledLink(t *testing.T) {
	home := t.TempDir()
	fakeDugout(t, t.TempDir(), "unused")
	real := t.TempDir()
	fakeDugout(t, real, "linked")
	link := filepath.Join(home, "Library", "Application Support", "dugout", "bin", "dugout")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "dugout"), link); err != nil {
		t.Fatal(err)
	}
	out, _, code := runScriptHome(t, home, t.TempDir(), nil, "payload", "hook", "PreToolUse")
	if code != 0 || out != "linked hook PreToolUse\npayload" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// TestHookScriptUsesLinuxLink: the script also finds the link install leaves
// in Linux's state dir, $XDG_STATE_HOME/dugout when that is absolute, else
// ~/.local/state/dugout. It is sh, so this runs on a Mac too.
func TestHookScriptUsesLinuxLink(t *testing.T) {
	link := func(t *testing.T, state, tag string) {
		t.Helper()
		real := t.TempDir()
		fakeDugout(t, real, tag)
		l := filepath.Join(state, "dugout", "bin", "dugout")
		if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, "dugout"), l); err != nil {
			t.Fatal(err)
		}
	}
	script := func(home, xdg string) (string, int) {
		env := []string{"PATH=/bin:/usr/bin", "CLAUDE_PLUGIN_ROOT=" + t.TempDir(), "HOME=" + home, "XDG_STATE_HOME=" + xdg}
		out, _, code := runScriptEnv(t, env, "payload", "hook", "PreToolUse")
		return out, code
	}
	home, xdg := t.TempDir(), t.TempDir()
	link(t, filepath.Join(home, ".local", "state"), "default")
	link(t, xdg, "xdg")
	for _, tc := range []struct{ xdg, want string }{
		{xdg, "xdg"},
		{"", "default"},
		{"relative", "default"},
		{t.TempDir(), "default"}, // an XDG_STATE_HOME without the link
	} {
		if out, code := script(home, tc.xdg); code != 0 || out != tc.want+" hook PreToolUse\npayload" {
			t.Errorf("XDG_STATE_HOME=%q: code=%d out=%q, want the %s link", tc.xdg, code, out, tc.want)
		}
	}
}

func TestUninstall(t *testing.T) {
	skipUnlessDarwin(t)
	home := t.TempDir()
	lc, log := fakeLaunchctl(t, true)
	uid := fmt.Sprint(os.Getuid())
	if _, _, code := runInstall(t, home, lc, "install", "-server", "http://127.0.0.1:1"); code != 0 {
		t.Fatal("install failed")
	}
	out, errOut, code := runInstall(t, home, lc, "uninstall")
	if code != 0 || out != "Dugout uninstalled.\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got := readLog(t, log); got[len(got)-1] != "bootout gui/"+uid+"/com.dugout.daemon" {
		t.Fatalf("last launchctl call = %q", got)
	}
	for _, p := range []string{
		filepath.Join(home, "Library", "LaunchAgents", "com.dugout.daemon.plist"),
		filepath.Join(home, "Library", "Application Support", "dugout", "bin", "dugout"),
	} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s still exists", p)
		}
	}
	out, _, code = runInstall(t, home, lc, "uninstall")
	if code != 0 || out != "Dugout was not installed.\n" {
		t.Fatalf("second uninstall: code=%d out=%q", code, out)
	}
}

func TestInstallDryRun(t *testing.T) {
	skipUnlessDarwin(t)
	home := t.TempDir()
	lc, log := fakeLaunchctl(t, false)
	out, _, code := runInstall(t, home, lc, "install", "-server", "http://127.0.0.1:1", "-dry-run")
	if code != 0 || !strings.Contains(out, "<key>RunAtLoad</key>") || !strings.Contains(out, "<string>http://127.0.0.1:1</string>") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("dry-run called launchctl")
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("dry-run touched HOME: %v", entries)
	}
}

func TestInstallNeedsServer(t *testing.T) {
	out, errOut, code := runInstall(t, t.TempDir(), t.TempDir(), "install")
	if code != 2 || out != "" || !strings.Contains(errOut, "usage:") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// TestInstallUnsupported: off macOS and Linux, install and uninstall say so
// and touch nothing. In-process, as a platform it does not run on.
func TestInstallUnsupported(t *testing.T) {
	onPlatform(t, "freebsd")
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, args := range [][]string{{"install", "-server", "http://127.0.0.1:1"}, {"uninstall"}} {
		var out, errb bytes.Buffer
		code := run(args, strings.NewReader(""), &out, &errb)
		if code != 1 || errb.String() != "dugout install only works on macOS and on Linux with systemd\n" {
			t.Fatalf("%s: code=%d err=%q", args[0], code, errb.String())
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("touched HOME: %v", entries)
	}
}

func TestVersion(t *testing.T) {
	out, _, code := runInstall(t, t.TempDir(), t.TempDir(), "version")
	if code != 0 || out != "dugout "+version+"\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// TestLogFormatJSON: with -log-format json every line serve and the daemon
// write is a JSON object with msg, level and time; an unknown format is a
// usage error (exit 2) for both.
func TestLogFormatJSON(t *testing.T) {
	addr := freeAddr(t)
	home := shortDir(t)
	serveProc := start(t, []string{wire.EnvToken + "=" + e2eToken}, "serve", "-addr", addr, "-log-format", "json")
	eventually(t, 5*time.Second, "serve answers", func() bool { _, err := state(addr); return err == nil })
	daemonProc := start(t, []string{statedir.EnvHome + "=" + home, wire.EnvToken + "=" + e2eToken},
		"daemon", "-server", "http://"+addr, "-poll", "300ms", "-resend", "2s", "-log-format", "json")
	waitDaemonSocket(t, home)
	serveProc.stop()
	daemonProc.stop()
	for name, p := range map[string]*proc{"serve": serveProc, "daemon": daemonProc} {
		lines := strings.Split(strings.TrimSpace(p.log.String()), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Errorf("%s wrote no log", name)
		}
		for _, l := range lines {
			var rec map[string]any
			if err := json.Unmarshal([]byte(l), &rec); err != nil {
				t.Errorf("%s: not JSON: %q", name, l)
				continue
			}
			for _, k := range []string{"msg", "level", "time"} {
				if _, ok := rec[k]; !ok {
					t.Errorf("%s: line without %q: %s", name, k, l)
				}
			}
		}
	}
	for _, cmd := range []string{"serve", "daemon"} {
		args := []string{cmd, "-log-format", "xml"}
		if cmd == "serve" {
			args = append(args, "-addr", freeAddr(t))
		} else {
			args = append(args, "-server", "off")
		}
		_, errOut, code := runIn(t, []string{"HOME=" + home, statedir.EnvHome + "=" + home}, "", args...)
		if code != 2 || !strings.Contains(errOut, "unknown log format: xml (text or json)") {
			t.Errorf("%s -log-format xml: exit %d, stderr %q", cmd, code, errOut)
		}
	}
}

func TestPluginMCPServer(t *testing.T) {
	raw, err := os.ReadFile("../../plugin/.mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	s, ok := m.MCPServers["dugout"]
	if len(m.MCPServers) != 1 || !ok || s.Command != "${CLAUDE_PLUGIN_ROOT}/bin/dugout-run" || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Fatalf("plugin/.mcp.json = %s", raw)
	}
	// Claude Code runs the command itself, not through sh as the hooks do.
	if info, err := os.Stat("../../plugin/bin/dugout-run"); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("plugin/bin/dugout-run is not executable: %v %v", info, err)
	}
}

// mcpClient is `dugout mcp` on pipes, as Claude Code runs it.
type mcpClient struct {
	t     *testing.T
	in    io.WriteCloser
	lines chan string
	id    int
}

func startMCP(t *testing.T, home, dir string) *mcpClient {
	t.Helper()
	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home, "HOME="+home)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &mcpClient{t: t, in: in, lines: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		in.Close() // EOF: dugout mcp returns
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("dugout mcp at EOF: %v", err)
			}
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Error("dugout mcp did not exit at EOF")
		}
		if t.Failed() {
			t.Logf("dugout mcp stderr:\n%s", stderr.String())
		}
	})
	return c
}

// rpc sends one request and returns its result, failing on a JSON-RPC error.
func (c *mcpClient) rpc(method string, params any) json.RawMessage {
	c.t.Helper()
	c.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	select {
	case line, ok := <-c.lines:
		var r struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if !ok || json.Unmarshal([]byte(line), &r) != nil || r.ID != c.id || r.Error != nil {
			c.t.Fatalf("%s: reply %q", method, line)
		}
		return r.Result
	case <-time.After(10 * time.Second):
		c.t.Fatalf("%s: no reply within 10s", method)
	}
	return nil
}

func (c *mcpClient) tool(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	var res struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	raw := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if json.Unmarshal(raw, &res) != nil || len(res.Content) != 1 {
		c.t.Fatalf("%s: result %s", name, raw)
	}
	return res.Content[0].Text, res.IsError
}

// TestE2EMCP: the built binary's `dugout mcp`, over pipes, against a real
// daemon: once a SessionStart registers the session, office_declare_intent
// lands in the daemon's full state, and office_who_touches answers.
func TestE2EMCP(t *testing.T) {
	f := testgit.NewFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "changed\n")
	addr, home := freeAddr(t), shortDir(t)
	startServe(t, addr)
	startDaemon(t, home, addr)
	c := startMCP(t, home, f.Clone)

	var hello struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct{ Name, Version string }
	}
	raw := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "1"}})
	if json.Unmarshal(raw, &hello) != nil || hello.ProtocolVersion != "2025-06-18" || hello.ServerInfo.Name != "dugout" || hello.ServerInfo.Version != version {
		t.Fatalf("initialize = %s", raw)
	}
	if _, err := c.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	if text, isErr := c.tool("office_declare_intent", map[string]any{"session_id": "sE", "summary": "fixing the login"}); !isErr || !strings.Contains(text, "does not know that session") {
		t.Fatalf("declare before SessionStart = %q isError=%v, want the unknown session error", text, isErr)
	}
	if out := runHook(t, home, "SessionStart", map[string]any{"session_id": "sE", "cwd": f.Clone}); !strings.Contains(out, "office_declare_intent") {
		t.Fatalf("SessionStart = %s", out)
	}
	if text, isErr := c.tool("office_declare_intent", map[string]any{"session_id": "sE", "summary": "fixing the login"}); text != "Intent recorded." || isErr {
		t.Fatalf("declare = %q isError=%v", text, isErr)
	}
	eventually(t, 10*time.Second, "the intent in the daemon's full state", func() bool {
		d, err := state(addr)
		return err == nil && len(d.Workstreams) == 1 && d.Workstreams[0].Intent == "fixing the login"
	})
	if text, isErr := c.tool("office_who_touches", map[string]any{"path": "shared.go"}); text != "Nobody else has that file in flight." || isErr {
		t.Fatalf("who shared.go = %q isError=%v", text, isErr)
	}

	// dugout pause quiets who on this machine; an intent is still taken.
	for _, sub := range []string{"pause", "resume"} {
		cmd := exec.Command(bin, sub)
		cmd.Env = append(os.Environ(), statedir.EnvHome+"="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dugout %s: %v %s", sub, err, out)
		}
		if sub == "resume" {
			break
		}
		if text, isErr := c.tool("office_who_touches", map[string]any{"path": "shared.go"}); text != "Dugout is paused on this machine." || isErr {
			t.Fatalf("who during dugout pause = %q isError=%v", text, isErr)
		}
		if text, isErr := c.tool("office_declare_intent", map[string]any{"session_id": "sE", "summary": "still on the login"}); text != "Intent recorded." || isErr {
			t.Fatalf("declare during dugout pause = %q isError=%v", text, isErr)
		}
	}
	if text, isErr := c.tool("office_who_touches", map[string]any{"path": "shared.go"}); text != "Nobody else has that file in flight." || isErr {
		t.Fatalf("who after dugout resume = %q isError=%v", text, isErr)
	}
}

// toldOverlap runs a serverless daemon on a new state dir, home, with two
// sessions, sA in the clone on feat-a and sB in a worktree on feat-b, that
// each change different lines of multi.txt and whose agents have been told of
// the overlap. In auto mode, the default, their humans were not.
func toldOverlap(t *testing.T) (home, a, b string) {
	t.Helper()
	f := testgit.NewFixture(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	testgit.WriteFile(t, filepath.Join(f.Seed, "multi.txt"), strings.Join(lines, "\n")+"\n")
	testgit.Git(t, f.Seed, "add", "-A")
	testgit.Git(t, f.Seed, "commit", "-q", "-m", "multi")
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, f.Clone, "fetch", "-q", "origin")
	testgit.Git(t, f.Clone, "merge", "-q", "--ff-only", "origin/main")
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	a, b = f.Clone, f.AddWorktree(t, "wtb", "feat-b")
	home = shortDir(t)
	startLocalDaemon(t, home)
	// Different lines of the same file: an overlap, never a conflict.
	for i, s := range []struct{ id, dir string }{{"sA", a}, {"sB", b}} {
		runHook(t, home, "SessionStart", map[string]any{"session_id": s.id, "cwd": s.dir})
		edited := slices.Clone(lines)
		edited[1+7*i] = "edited in " + s.id
		testgit.WriteFile(t, filepath.Join(s.dir, "multi.txt"), strings.Join(edited, "\n")+"\n")
	}
	for _, s := range []struct{ id, dir, want string }{
		{"sA", a, `- branch "feat-b" (daemon `},
		{"sB", b, `- branch "feat-a" (daemon `},
	} {
		var out struct {
			SystemMessage      string `json:"systemMessage"`
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		eventually(t, 10*time.Second, s.id+" is told of the overlap", func() bool {
			out.SystemMessage, out.HookSpecificOutput.AdditionalContext = "", ""
			json.Unmarshal([]byte(runHook(t, home, "UserPromptSubmit", map[string]any{"session_id": s.id, "cwd": s.dir})), &out)
			return strings.Contains(out.HookSpecificOutput.AdditionalContext, "multi.txt")
		})
		if !strings.Contains(out.HookSpecificOutput.AdditionalContext, s.want) || out.SystemMessage != "" {
			t.Fatalf("%s's overlap: context %q, systemMessage %q; want %q for the agent and nothing for the human", s.id, out.HookSpecificOutput.AdditionalContext, out.SystemMessage, s.want)
		}
	}
	return home, a, b
}

// linkDugout leaves the built binary where `dugout install` links it in home,
// for dugout-run to find with no dugout on the PATH.
func linkDugout(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(binLink(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, binLink(home)); err != nil {
		t.Fatal(err)
	}
}

// TestStartSaysNothingE2E: a session that starts in a worktree with no
// warning of its own, after another was told of one, hears nothing, through
// dugout-run as hooks.json runs it, and every hook exits 0 with nothing on
// stderr; `dugout rate` is gone.
func TestStartSaysNothingE2E(t *testing.T) {
	home, a, _ := toldOverlap(t)
	linkDugout(t, home)
	c := filepath.Join(t.TempDir(), "wtc")
	testgit.Git(t, a, "worktree", "add", "-q", "-b", "feat-c", c)
	hook := func(event, session, dir string) (stdout, stderr string, code int) {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"session_id": session, "cwd": dir, "hook_event_name": event})
		env := []string{statedir.EnvHome + "=" + home, "HOME=" + home, "PATH=/bin:/usr/bin", "CLAUDE_PLUGIN_ROOT=" + t.TempDir()}
		return runScriptEnv(t, env, string(in), "hook", event)
	}
	if out, errOut, code := hook("SessionEnd", "sA", a); code != 0 || strings.TrimSpace(out) != "{}" || errOut != "" {
		t.Fatalf("SessionEnd: exit %d, stdout %q, stderr %q, want 0, {} and nothing", code, out, errOut)
	}
	out, errOut, code := hook("SessionStart", "sC", c)
	if code != 0 || errOut != "" || strings.Contains(out, "systemMessage") {
		t.Fatalf("SessionStart: exit %d, stdout %q, stderr %q, want no systemMessage", code, out, errOut)
	}
	if _, errOut, code := runIn(t, []string{statedir.EnvHome + "=" + home, "HOME=" + home}, "", "rate"); code != 2 || !strings.Contains(errOut, "unknown") {
		t.Fatalf("dugout rate: exit %d %s, want 2 and an unknown subcommand", code, errOut)
	}
}

// TestAdminMetricsCommand: dugout admin metrics prints serve's Prometheus
// text as it is.
func TestAdminMetricsCommand(t *testing.T) {
	w := startAdminServe(t)
	out, errOut, code := w.admin(t, w.adminTok, "metrics")
	if code != 0 || !strings.HasPrefix(out, "# HELP dugout_daemons_connected ") ||
		!strings.Contains(out, "\ndugout_persons 0\n") || strings.Contains(out, "dgt_") {
		t.Fatalf("metrics: code=%d out=%q err=%q", code, out, errOut)
	}
	if out, errOut, code := w.admin(t, "dgt_nope", "metrics"); code != 1 || out != "" || errOut != "serve rejected the admin token\n" {
		t.Errorf("wrong token: code=%d out=%q err=%q", code, out, errOut)
	}
	if _, _, code := w.admin(t, w.adminTok, "metrics", "x"); code != 2 {
		t.Errorf("metrics with an argument: code=%d, want 2", code)
	}
}

// TestAdminMetricsWithoutRoute: a serve without the metrics route (an older
// one, or one run without -state) answers 404, and the command says so.
func TestAdminMetricsWithoutRoute(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	out, errOut, code := runAdminEnv(t, []string{envServer + "=" + srv.URL, "DUGOUT_ADMIN_TOKEN=dgt_whatever"}, "admin", "metrics")
	if code != 1 || out != "" || errOut != "serve has no metrics: update it, or run it with -state\n" {
		t.Fatalf("metrics against a serve without the route: code=%d out=%q err=%q", code, out, errOut)
	}
}
