package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

// shortSocket returns a socket path under a short temp dir (macOS caps
// unix socket paths at 104 bytes).
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "dugout.sock")
}

func run(t *testing.T, env Env, event, stdin string) map[string]any {
	t.Helper()
	m, errOut, code := runAll(t, env, event, stdin)
	if code != 0 || errOut != "" {
		t.Fatalf("hook %s exited %d with stderr %q, want 0 and nothing", event, code, errOut)
	}
	return m
}

// runAll is run that also returns the hook's stderr and exit code.
func runAll(t *testing.T, env Env, event, stdin string) (map[string]any, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(event, strings.NewReader(stdin), &out, &errOut, env)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("hook printed invalid JSON %q: %v", out.String(), err)
	}
	return m, errOut.String(), code
}

func noPID() int { return 0 }

// fakeDaemon answers every request with output and sends what it got on reqs.
func fakeDaemon(t *testing.T, output string) (string, <-chan wire.Request) {
	t.Helper()
	sock := shortSocket(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reqs := make(chan wire.Request, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req wire.Request
			json.Unmarshal(line, &req)
			reqs <- req
			b, _ := json.Marshal(wire.Response{Output: json.RawMessage(output)})
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	return sock, reqs
}

func TestFailOpenNoDaemon(t *testing.T) {
	env := Env{Socket: shortSocket(t), TempDir: t.TempDir(), ClaudePID: noPID}
	in := `{"session_id":"s1","cwd":"/x"}`
	if m := run(t, env, "PreToolUse", in); m["systemMessage"] != DegradedMessage || len(m) != 1 {
		t.Fatalf("first degraded hook = %v", m)
	}
	if m := run(t, env, "PostToolUse", in); len(m) != 0 {
		t.Fatalf("second degraded hook of the same session = %v, want {}", m)
	}
	if m := run(t, env, "PreToolUse", `{"session_id":"s2"}`); m["systemMessage"] != DegradedMessage {
		t.Fatalf("another session = %v, want the message once more", m)
	}
}

func TestSlowDaemon(t *testing.T) {
	sock := shortSocket(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(2 * time.Second); c.Close() }()
		}
	}()
	env := Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID}
	start := time.Now()
	m := run(t, env, "PreToolUse", `{"session_id":"s1"}`)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("hook waited %s for a stuck daemon", elapsed)
	}
	if m["systemMessage"] != DegradedMessage {
		t.Fatalf("stuck daemon = %v, want the degraded message", m)
	}
}

func TestPromptNeverForwarded(t *testing.T) {
	sock, reqs := fakeDaemon(t, `{}`)
	env := Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID}
	run(t, env, "UserPromptSubmit", `{"session_id":"s1","prompt":"user secret","cwd":"/x"}`)
	req := <-reqs
	if strings.Contains(string(req.Input), "secret") || strings.Contains(string(req.Input), `"prompt"`) {
		t.Fatalf("prompt reached the daemon: %s", req.Input)
	}
	if req.Type != wire.TypeHook || req.Event != "UserPromptSubmit" || !strings.Contains(string(req.Input), `"session_id":"s1"`) {
		t.Fatalf("request = %+v", req)
	}
}

// TestHookForwardsOnlyNeededFields: PostToolUse carries whole files in
// tool_input and tool_response. The hook forwards only what the daemon
// uses, and a multi-MiB event still keeps its session.
func TestHookForwardsOnlyNeededFields(t *testing.T) {
	sock, reqs := fakeDaemon(t, `{}`)
	big := strings.Repeat("x", 3<<20)
	in, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": "/x", "hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input":    map[string]any{"file_path": "/x/a.go", "content": big},
		"tool_response": map[string]any{"originalFile": big},
	})
	run(t, Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID}, "PostToolUse", string(in))
	req := <-reqs
	got := string(req.Input)
	if !strings.Contains(got, `"session_id":"s1"`) || !strings.Contains(got, `"file_path":"/x/a.go"`) {
		t.Fatalf("lost session or file_path: %.200s", got)
	}
	if strings.Contains(got, "content") || strings.Contains(got, "originalFile") || len(got) > 1024 {
		t.Fatalf("forwarded file contents (%d bytes): %.200s", len(got), got)
	}
}

// TestHookForwardsEditHashAndAddress: an edit's PreToolUse goes
// with a hash of what it writes, the same for the same edit and another for
// another, never the text; a SendMessage goes with its address alone.
func TestHookForwardsEditHashAndAddress(t *testing.T) {
	sock, reqs := fakeDaemon(t, `{}`)
	env := Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID}
	forward := func(event, tool string, input map[string]any) map[string]any {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": "/x", "tool_name": tool, "tool_input": input})
		run(t, env, event, string(in))
		req := <-reqs
		if strings.Contains(string(req.Input), "secret") {
			t.Fatalf("%s %s forwarded its text: %s", event, tool, req.Input)
		}
		var got map[string]any
		if err := json.Unmarshal(req.Input, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	edit := func(path, newText string) map[string]any {
		return map[string]any{"file_path": path, "old_string": "secret a", "new_string": newText}
	}
	first := forward("PreToolUse", "Edit", edit("/x/a.go", "secret b"))["edit_hash"]
	again := forward("PreToolUse", "Edit", edit("/x/a.go", "secret b"))["edit_hash"]
	other := forward("PreToolUse", "Edit", edit("/x/a.go", "secret c"))["edit_hash"]
	moved := forward("PreToolUse", "Edit", edit("/x/b.go", "secret b"))["edit_hash"]
	if s, _ := first.(string); len(s) != 16 || again != first || other == first || moved != first {
		t.Fatalf("hashes: first %v, again %v, other %v, other path %v; want 16 hex digits, equal, different, equal", first, again, other, moved)
	}
	if got := forward("PostToolUse", "Edit", edit("/x/a.go", "secret b")); got["edit_hash"] != nil {
		t.Fatalf("PostToolUse got a hash: %v", got)
	}
	got := forward("PostToolUse", "SendMessage", map[string]any{"to": "bruno-session", "message": "secret plan", "summary": "secret"})
	if ti, _ := got["tool_input"].(map[string]any); ti["to"] != "bruno-session" || len(ti) != 1 || got["edit_hash"] != nil {
		t.Fatalf("SendMessage forwarded %v, want its address alone", got)
	}
}

// TestHookRecoversPanic: a panic would exit 2, which Claude Code reads
// as "block the tool". The hook prints {} instead.
func TestHookRecoversPanic(t *testing.T) {
	sock, _ := fakeDaemon(t, `{}`)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped the hook: %v", r)
		}
	}()
	m := run(t, Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: func() int { panic("boom") }}, "SessionStart", `{"session_id":"s1"}`)
	if len(m) != 0 {
		t.Fatalf("after a panic the hook printed %v, want {}", m)
	}
}

func TestPassThrough(t *testing.T) {
	sock, _ := fakeDaemon(t, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"hello"}}`)
	m := run(t, Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID}, "PostToolUse", `{"session_id":"s1"}`)
	hso, _ := m["hookSpecificOutput"].(map[string]any)
	if hso["additionalContext"] != "hello" {
		t.Fatalf("daemon output not passed through: %v", m)
	}
}

// TestHookAlwaysExitsZero: the daemon's answer goes to stdout with exit 0 and
// nothing on stderr, for SessionEnd as for any event, and so does a daemon
// that is down.
func TestHookAlwaysExitsZero(t *testing.T) {
	sock, _ := fakeDaemon(t, `{"systemMessage":"x"}`)
	for _, event := range []string{"SessionEnd", "PostToolUse"} {
		m, errOut, code := runAll(t, Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID}, event, `{"session_id":"s1"}`)
		if m["systemMessage"] != "x" || errOut != "" || code != 0 {
			t.Fatalf("%s: stdout %v, stderr %q, exit %d; want the daemon's answer, no stderr and 0", event, m, errOut, code)
		}
	}
	_, errOut, code := runAll(t, Env{Socket: shortSocket(t), TempDir: t.TempDir(), ClaudePID: noPID}, "SessionEnd", `{"session_id":"s1"}`)
	if errOut != "" || code != 0 {
		t.Fatalf("SessionEnd with the daemon down: stderr %q, exit %d; want no stderr and 0", errOut, code)
	}
}

func TestClaudePIDOnlyOnSessionStart(t *testing.T) {
	sock, reqs := fakeDaemon(t, `{}`)
	calls := 0
	env := Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: func() int { calls++; return 4242 }}
	run(t, env, "SessionStart", `{"session_id":"s1"}`)
	if req := <-reqs; req.ClaudePID != 4242 {
		t.Fatalf("SessionStart claude_pid = %d", req.ClaudePID)
	}
	run(t, env, "PostToolUse", `{"session_id":"s1"}`)
	if req := <-reqs; req.ClaudePID != 0 || calls != 1 {
		t.Fatalf("PostToolUse looked up the PID: claude_pid %d, %d lookups", req.ClaudePID, calls)
	}
}

// TestClaudeDirOnEveryEvent: every event names the session's Claude Code
// config dir, so a daemon that registers the session at any hook, after a
// restart too, can read its name from the registry.
func TestClaudeDirOnEveryEvent(t *testing.T) {
	sock, reqs := fakeDaemon(t, `{}`)
	env := Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID, ClaudeDir: "/c/.claude"}
	for _, event := range []string{"SessionStart", "PostToolUse", "Stop"} {
		run(t, env, event, `{"session_id":"s1"}`)
		if req := <-reqs; req.ClaudeDir != "/c/.claude" {
			t.Fatalf("%s claude_dir = %q", event, req.ClaudeDir)
		}
	}
}

func TestFindClaudePID(t *testing.T) {
	table := map[int]struct {
		ppid int
		comm string
	}{10: {9, "dugout"}, 9: {8, "zsh"}, 8: {1, "claude"}, 7: {1, "Claude"}}
	lookups := 0
	lookup := func(pid int) (int, string, error) {
		lookups++
		p, ok := table[pid]
		if !ok {
			return 0, "", errors.New("no such process")
		}
		return p.ppid, p.comm, nil
	}
	if got := FindClaudePID(9, lookup); got != 8 {
		t.Errorf("FindClaudePID(9) = %d, want 8", got)
	}
	lookups = 0
	if got := FindClaudePID(7, lookup); got != 7 || lookups != 1 {
		t.Errorf("direct parent: FindClaudePID(7) = %d after %d lookups, want 7 after 1", got, lookups)
	}
	if got := FindClaudePID(10, func(pid int) (int, string, error) {
		if pid == 8 {
			return 1, "launchd", nil
		}
		return lookup(pid)
	}); got != 0 {
		t.Errorf("no claude ancestor: got %d, want 0", got)
	}
}

func TestSysctlLookupSelf(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("process lookup via sysctl is macOS-only (pid_other.go reports it unsupported)")
	}
	ppid, comm, err := SysctlLookup(os.Getpid())
	if err != nil || ppid != os.Getppid() || comm == "" {
		t.Fatalf("SysctlLookup(self) = %d %q %v, want ppid %d", ppid, comm, err, os.Getppid())
	}
}

func TestProcLookupSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux's (procLookup is tested on a fake one below)")
	}
	ppid, comm, err := ProcLookup(os.Getpid())
	if err != nil || ppid != os.Getppid() || comm == "" {
		t.Fatalf("ProcLookup(self) = %d %q %v, want ppid %d", ppid, comm, err, os.Getppid())
	}
}

// fakeProc writes a /proc with one stat file per pid under a temp root.
func fakeProc(t *testing.T, stats map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, stat := range stats {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestProcLookupFindsClaude: the walk goes up /proc's parents to the claude
// process, through a name with spaces and parentheses that would fool a
// split on spaces.
func TestProcLookupFindsClaude(t *testing.T) {
	root := fakeProc(t, map[int]string{
		40: "40 (sh) S 30 40 30 0 -1 4194560 0 0 0 0\n",
		30: "30 (a b) (c)) S 20 30 20 0 -1 4194560\n",
		20: "20 (claude) S 1 20 20 34816 20 4194560\n",
	})
	lookup := func(pid int) (int, string, error) { return procLookup(root, pid) }
	if ppid, comm, err := lookup(30); err != nil || ppid != 20 || comm != "a b) (c)" {
		t.Fatalf("lookup(30) = %d %q %v", ppid, comm, err)
	}
	if got := FindClaudePID(40, lookup); got != 20 {
		t.Fatalf("FindClaudePID = %d, want 20", got)
	}
	if got := FindClaudePID(99, lookup); got != 0 {
		t.Fatalf("FindClaudePID of a missing pid = %d, want 0", got)
	}
	for _, bad := range []string{"", "41 sh S 1", "41 (sh)", "41 (sh) S x"} {
		if _, _, err := parseStat([]byte(bad)); err == nil {
			t.Errorf("parseStat(%q) parsed", bad)
		}
	}
}

// onPlatform makes the hook take platform p's kickstart for the test.
func onPlatform(t *testing.T, p string) {
	t.Helper()
	old := goos
	goos = p
	t.Cleanup(func() { goos = old })
}

// TestKickstartCmd: with the unit installed (under $XDG_CONFIG_HOME when it
// is set) the Linux kickstart is a non-blocking systemctl --user start;
// without it, or without a home, there is nothing to start.
func TestKickstartCmd(t *testing.T) {
	onPlatform(t, "linux")
	home, config := t.TempDir(), t.TempDir()
	for _, xdg := range []string{"", config} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if cmd := kickstartCmd(home); cmd != nil {
			t.Fatalf("XDG_CONFIG_HOME=%q, no unit: kickstart %v", xdg, cmd.Args)
		}
		unit := systemd.UnitPath(home, os.Getenv)
		if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unit, systemd.Render("/b/dugout", "http://x", "/l", false, nil), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := kickstartCmd(home)
		if cmd == nil || strings.Join(cmd.Args, " ") != "systemctl --user --no-block start dugout.service" {
			t.Fatalf("XDG_CONFIG_HOME=%q: kickstart = %v", xdg, cmd)
		}
	}
	if cmd := kickstartCmd(""); cmd != nil {
		t.Fatalf("no home: kickstart %v", cmd.Args)
	}
	onPlatform(t, "freebsd")
	if cmd := kickstartCmd(home); cmd != nil {
		t.Fatalf("freebsd: kickstart %v", cmd.Args)
	}
}

// TestTerminalNotification: an escalation the daemon could not show on the
// desktop goes to the terminal as OSC 9, never to stdout, which stays the
// hook's JSON; a daemon that sends none writes nothing there.
func TestTerminalNotification(t *testing.T) {
	sock := shortSocket(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	terminal := "Dugout: decide: \x1b]9;evil\x07 which cap stays?"
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadBytes('\n')
			b, _ := json.Marshal(wire.Response{Output: json.RawMessage(`{"systemMessage":"m"}`), Terminal: terminal})
			c.Write(append(b, '\n'))
			c.Close()
			terminal = ""
		}
	}()
	var got []string
	env := Env{Socket: sock, TempDir: t.TempDir(), ClaudePID: noPID, Terminal: func(s string) { got = append(got, s) }}
	if m := run(t, env, "Stop", `{"session_id":"s1"}`); m["systemMessage"] != "m" || len(m) != 1 {
		t.Fatalf("stdout = %v, want the daemon's output alone", m)
	}
	if len(got) != 1 || got[0] != "Dugout: decide: \x1b]9;evil\x07 which cap stays?" {
		t.Fatalf("terminal got %q", got)
	}
	run(t, env, "Stop", `{"session_id":"s1"}`)
	if len(got) != 1 {
		t.Fatalf("a daemon with nothing for the terminal wrote %q", got[1:])
	}
	if want := "\x1b]9;Dugout: decide: ]9;evil which cap stays?\x07"; OSC9(got[0]) != want {
		t.Fatalf("OSC9 = %q, want %q", OSC9(got[0]), want)
	}
}
