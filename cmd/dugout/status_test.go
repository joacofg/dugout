package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

// fakeStatusDaemon (the daemon sorts by path; st comes sorted) answers every status request on home's socket with st.
func fakeStatusDaemon(t *testing.T, home string, st wire.Status) {
	t.Helper()
	ln, err := net.Listen("unix", statedir.Socket(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadBytes('\n')
				var req wire.Request
				var resp wire.Response
				if json.Unmarshal(line, &req) != nil || req.Type != wire.TypeStatus {
					resp.Error = "unexpected request " + string(line)
				} else {
					resp.Output, _ = json.Marshal(st)
				}
				b, _ := json.Marshal(resp)
				c.Write(append(b, '\n'))
			}()
		}
	}()
}

func status(t *testing.T, home string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv(statedir.EnvHome, home)
	var out, errb bytes.Buffer
	code = run([]string{"status"}, strings.NewReader(""), &out, &errb)
	return out.String(), errb.String(), code
}

func TestStatusOutput(t *testing.T) {
	home := shortDir(t)
	fakeStatusDaemon(t, home, wire.Status{
		Daemon: "0123456789abcdef", Server: "https://dugout.example.com", Connected: true, Paused: "local",
		Judge: wire.StatusJudge{Name: "anthropic claude-haiku-5-5", Comparisons: 3, Duplicates: 1, Errors: 1, InputTokens: 1200, OutputTokens: 90,
			USD: 0.000165, LastError: "timed out"},
		Workstreams: []wire.StatusWorkstream{{
			Root: "/work/app", Repo: "app", Branch: "feat/login", Files: []string{"a.go", "b.go", "c.go"}, Intent: "fixing the login",
			Overlaps: []wire.StatusOverlap{
				{Path: "b.go", Branch: "feat/payment", Daemon: "89abcdef01234567", Intent: "charge"},
				{Path: "z.go", Branch: "other", Daemon: "ffffffff00000000"},
			},
			Conflicts: []wire.StatusConflict{
				{Path: "a.go", Branch: "feat/auth", Daemon: "fedcba9876543210", Lines: "your lines 3-5",
					Tally: core.Tally{Messages: 1, Held: 2, Changed: 1, AgentEscalations: 1, Committed: true}},
				{Path: "z.go", Branch: "main2", Daemon: "aaaaaaaa11111111"},
			},
			Duplicates: []wire.StatusDuplicate{
				{Branch: "feat/throttle", Daemon: "1111111122222222", Intent: "limit login attempts", Reason: "Both rate-limit the login."},
			},
		}, {
			Root: "/work/lib", Repo: "lib", Branch: "main", Files: []string{"x.go"},
		}},
		Ended: []wire.StatusEnded{
			{Repo: "app", Path: "a.go", Branches: [2]string{"feat/login", "feat/auth"}, Outcome: "resolved", Tally: core.Tally{Messages: 3, RuleEscalations: 1, Held: 1, Changed: 1}},
			{Repo: "app", Path: "b.go", Branches: [2]string{"feat/login", "feat/pay"}, Outcome: "committed", Tally: core.Tally{Committed: true, Held: 1, Unchanged: 1}},
			{Repo: "lib", Path: "x.go", Branches: [2]string{"main", "dev"}, Outcome: "gone"},
		},
		Totals: []wire.StatusTotals{
			{Repo: "app", Totals: core.Totals{Conflicts: 3, Resolved: 2, Committed: 1, Messages: 4, Escalations: 1, Held: 2, Changed: 1, Unchanged: 1}},
			{Repo: "lib", Totals: core.Totals{Conflicts: 1, Gone: 1}},
		},
	})
	want := `Dugout on this machine (daemon 01234567)
Server: https://dugout.example.com (connected)
Paused: on this machine
Duplicate detection: on, judge anthropic claude-haiku-5-5: 3 comparisons, 1 duplicates, 1200 input and 90 output tokens, $0.0002; 1 failed, the last: timed out

/work/app
  branch "feat/login" in app, 3 files in flight
  Intent: "fixing the login"
  conflict: a.go conflicts with branch "feat/auth" (daemon fedcba98)
    so far: 1 message, 1 escalation, 2 edits held: 1 changed after, 0 retried unchanged, 1 not retried; a side committed or pushed while it stood
  conflict: z.go conflicts with branch "main2" (daemon aaaaaaaa)
    so far: 0 messages, 0 escalations, no edit held
  overlap: branch "feat/payment" also has b.go in flight
  overlap: branch "other" also has z.go in flight
  possible duplicate: branch "feat/throttle" (daemon 11111111) declared "limit login attempts": Both rate-limit the login.

/work/lib
  branch "main" in lib, 1 file in flight

Conflicts ended since the daemon started
  app: 3 conflicts, 2 resolved before a commit or push (67% of those resolved or committed), 1 committed while standing, 0 gone
    per conflict: 1.3 messages, 0.3 escalations; 2 edits held: 1 changed after, 1 retried unchanged, 0 not retried
    latest: a.go, branches "feat/login" and "feat/auth": resolved before a commit or push; 3 messages, 1 escalation, 1 edit held: 1 changed after, 0 retried unchanged, 0 not retried
    latest: b.go, branches "feat/login" and "feat/pay": a side committed or pushed while it stood; 0 messages, 0 escalations, 1 edit held: 0 changed after, 1 retried unchanged, 0 not retried
  lib: 1 conflict, 0 resolved before a commit or push, 0 committed while standing, 1 gone
    per conflict: 0.0 messages, 0.0 escalations; no edit held
    latest: x.go, branches "main" and "dev": a side's worktree or branch went away; 0 messages, 0 escalations, no edit held
`
	out, errOut, code := status(t, home)
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("status = %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

func TestStatusNothing(t *testing.T) {
	home := shortDir(t)
	fakeStatusDaemon(t, home, wire.Status{Daemon: "0123456789abcdef", Judge: wire.StatusJudge{Off: "no judge configured"}, Workstreams: []wire.StatusWorkstream{}})
	out, errOut, code := status(t, home)
	want := "Dugout on this machine (daemon 01234567)\nServer: none\nDuplicate detection: off (no judge configured)\n\nNothing in flight on this machine.\n"
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("status = %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	// A server that is down says so; a team pause shows.
	fakeStatusDaemon2 := wire.Status{Daemon: "0123456789abcdef", Server: "http://127.0.0.1:7457", Paused: "team"}
	home2 := shortDir(t)
	fakeStatusDaemon(t, home2, fakeStatusDaemon2)
	out, _, _ = status(t, home2)
	want = "Dugout on this machine (daemon 01234567)\nServer: http://127.0.0.1:7457 (disconnected)\nPaused: for the whole team\n" +
		"Duplicate detection: off (the daemon does not report it: update dugout)\n\nNothing in flight on this machine.\n"
	if out != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out, want)
	}
}

func TestStatusDaemonDown(t *testing.T) {
	out, errOut, code := status(t, shortDir(t))
	if code != 1 || out != "" || errOut != "dugout status: the daemon is not responding: run dugout doctor\n" {
		t.Fatalf("status = %d, stdout %q, stderr %q", code, out, errOut)
	}
}
