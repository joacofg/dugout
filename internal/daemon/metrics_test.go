package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

// outcomesSeen is a serve that records every outcomes message it takes.
type outcomesSeen struct {
	mu   sync.Mutex
	recs []core.Record
	raw  []string
}

func (o *outcomesSeen) all() ([]core.Record, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]core.Record(nil), o.recs...), append([]string(nil), o.raw...)
}

func newRecordingServe(t *testing.T) (*httptest.Server, *outcomesSeen) {
	t.Helper()
	seen := &outcomesSeen{}
	h := serve.New().Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/msg" {
			b, _ := io.ReadAll(r.Body)
			var msg wire.Message
			if json.Unmarshal(b, &msg) == nil && msg.Type == wire.TypeOutcomes {
				seen.mu.Lock()
				seen.recs = append(seen.recs, msg.Outcomes...)
				seen.raw = append(seen.raw, string(b))
				seen.mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv, seen
}

// hookWith sends one hook event with tool_input and the extra fields the
// hook forwards, from Claude Code config dir claudeDir.
func hookWith(t *testing.T, sock, event, session, cwd, claudeDir string, in map[string]any) string {
	t.Helper()
	in["session_id"], in["cwd"], in["hook_event_name"] = session, cwd, event
	b, _ := json.Marshal(in)
	resp := call(t, sock, wire.Request{Type: wire.TypeHook, Event: event, Input: b, ClaudeDir: claudeDir})
	if resp.Error != "" {
		t.Fatalf("hook %s: %s", event, resp.Error)
	}
	return string(resp.Output)
}

func editIn(path, hash string) map[string]any {
	return map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": path}, "edit_hash": hash}
}

func sendIn(to string) map[string]any {
	return map[string]any{"tool_name": "SendMessage", "tool_input": map[string]any{"to": to}}
}

func status(t *testing.T, sock string) wire.Status {
	t.Helper()
	var st wire.Status
	resp := call(t, sock, wire.Request{Type: wire.TypeStatus})
	if err := json.Unmarshal(resp.Output, &st); err != nil {
		t.Fatalf("status %s: %v", resp.Output, err)
	}
	return st
}

// TestMetricsResolvedOverTheChannel, against a running daemon, real git and
// serve: two sessions in conflict on shared.go; A's edit is held and A tries a
// different one; the two sessions message each other; A aligns its lines with
// B's. With no human action the conflict ends resolved before any commit, with
// its four metrics, in `dugout status` and in what goes to serve, which carries
// no path and no code.
func TestMetricsResolvedOverTheChannel(t *testing.T) {
	f, wt := overlap(t)
	srv, seen := newRecordingServe(t)
	r := startDaemonWith(t, srv.URL, func(d *Daemon) { d.metrics = core.NewLedger(300) })
	reg := newRegistry(t)
	reg.add(t, 901, "sA", "ana-session", 1)
	reg.add(t, 902, "sB", "bruno-session", 2)
	sessions := []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}}
	for _, s := range sessions {
		hookWith(t, r.sock, "SessionStart", s.id, s.cwd, string(reg), map[string]any{})
	}
	for _, s := range sessions {
		eventually(t, 15*time.Second, s.id+" is handed the conflict", func() bool {
			return strings.Contains(hookWith(t, r.sock, "PostToolUse", s.id, s.cwd, string(reg), editIn(filepath.Join(s.cwd, "shared.go"), "")), "- conflict ")
		})
	}
	shared := filepath.Join(f.Clone, "shared.go")
	if out := hookWith(t, r.sock, "PreToolUse", "sA", f.Clone, string(reg), editIn(shared, "h1")); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("A's first edit of shared.go was not held: %s", out)
	}
	hookWith(t, r.sock, "PostToolUse", "sA", f.Clone, string(reg), sendIn("bruno-session"))
	hookWith(t, r.sock, "PostToolUse", "sB", wt, string(reg), sendIn("sA"))
	hookWith(t, r.sock, "PostToolUse", "sB", wt, string(reg), sendIn("nobody"))
	if out := hookWith(t, r.sock, "PreToolUse", "sA", f.Clone, string(reg), editIn(shared, "h2")); strings.Contains(out, "deny") {
		t.Fatalf("A's second edit was held too: %s", out)
	}
	var st wire.Status
	var standing *wire.StatusConflict
	eventually(t, 15*time.Second, "the standing conflict's tally", func() bool {
		st = status(t, r.sock)
		standing = nil
		for _, w := range st.Workstreams {
			for i := range w.Conflicts {
				standing = &w.Conflicts[i]
			}
		}
		return standing != nil && standing.Tally == (core.Tally{Messages: 2, Held: 1, Changed: 1})
	})

	testgit.WriteFile(t, shared, "from b\n") // A aligns with B
	var ended []wire.StatusEnded
	eventually(t, 15*time.Second, "the conflict ends in dugout status", func() bool {
		ended = status(t, r.sock).Ended
		return len(ended) == 1
	})
	want := wire.StatusEnded{Repo: ended[0].Repo, Path: "shared.go", Branches: ended[0].Branches, Outcome: "resolved",
		Tally: core.Tally{Messages: 2, Held: 1, Changed: 1}}
	if b := ended[0].Branches; b != [2]string{"feat-a", "feat-b"} && b != [2]string{"feat-b", "feat-a"} {
		t.Fatalf("ended branches %v", b)
	}
	if ended[0] != want {
		t.Fatalf("ended %+v, want %+v", ended[0], want)
	}
	var recs []core.Record
	var raw []string
	eventually(t, 10*time.Second, "serve gets the conflict's record", func() bool {
		recs, raw = seen.all()
		return len(recs) > 0
	})
	got := recs[0]
	if got.Outcome != core.OutcomeResolved || got.Tally != (core.Tally{Messages: 2, Held: 1, Changed: 1}) || got.Daemons != [2]core.DaemonID{r.d.id, r.d.id} {
		t.Fatalf("serve got %+v", got)
	}
	for _, leak := range []string{"shared.go", "from a", "from b", "feat-a", "bruno-session"} {
		if strings.Contains(strings.Join(raw, ""), leak) {
			t.Fatalf("the outcomes message carries %q: %s", leak, raw)
		}
	}
	st = status(t, r.sock)
	if len(st.Totals) != 1 || st.Totals[0].Totals != (core.Totals{Conflicts: 1, Resolved: 1, Messages: 2, Held: 1, Changed: 1}) {
		t.Fatalf("totals %+v", st.Totals)
	}
}

// TestMetricsCommitted: a side that commits while the conflict stands ends
// it committed, however it goes away after.
func TestMetricsCommitted(t *testing.T) {
	f, wt := overlap(t)
	r := startDaemonWith(t, "", func(d *Daemon) { d.metrics = core.NewLedger(300) })
	for _, s := range []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}} {
		hook(t, r.sock, "SessionStart", s.id, s.cwd, "", "")
	}
	eventually(t, 15*time.Second, "the conflict stands", func() bool {
		st := status(t, r.sock)
		return len(st.Workstreams) == 2 && len(st.Workstreams[0].Conflicts) == 1
	})
	testgit.Git(t, wt, "add", "shared.go")
	testgit.Git(t, wt, "commit", "-qm", "b")
	eventually(t, 15*time.Second, "the commit is seen while it stands", func() bool {
		for _, w := range status(t, r.sock).Workstreams {
			for _, c := range w.Conflicts {
				if c.Tally.Committed {
					return true
				}
			}
		}
		return false
	})
	testgit.WriteFile(t, filepath.Join(f.Clone, "shared.go"), "from b\n")
	eventually(t, 15*time.Second, "the conflict ends committed", func() bool {
		st := status(t, r.sock)
		return len(st.Ended) == 1 && st.Ended[0].Outcome == "committed" && len(st.Totals) == 1 && st.Totals[0].Committed == 1
	})
}
