package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// idle is a daemon on dir that never runs: hooks go through answerHook and
// handleHook, requests through handle.
func idle(t *testing.T, dir string) *Daemon {
	t.Helper()
	d, err := New(Config{StateDir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func sysMsg(out map[string]any) string {
	s, _ := out["systemMessage"].(string)
	return s
}

// tell hands v to session s of d as a UserPromptSubmit would, and returns the
// systemMessage.
func tell(d *Daemon, s *Session, v View) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, _ := d.answerHook("UserPromptSubmit", "", "", s, me, v)
	return sysMsg(out)
}

func overlaps(ws ...core.Warning) View { return View{Overlaps: ws} }

// scopedSession is a session in ModeScoped, told of it already: outside
// ModeAuto the human hears of overlaps too.
func scopedSession(id string) *Session {
	s := NewSession(id, "/repo", 0)
	s.Mode, s.modeTold = ModeScoped, ModeScoped
	return s
}

// TestAutoOverlapsSilent: in ModeAuto, the default, an overlap
// with no conflict is the agents' alone: the human hears nothing of it, but
// still of a conflict. The agent still gets both.
func TestAutoOverlapsSilent(t *testing.T) {
	d := idle(t, shortDir(t))
	for _, m := range []Mode{"", ModeAuto} {
		s := NewSession("s1", "/repo", 0)
		s.Mode = m
		s.mu.Lock()
		out, _ := d.answerHook("UserPromptSubmit", "", "", s, me, overlaps(warning("x.go", "feat")))
		s.mu.Unlock()
		if got := sysMsg(out); got != "" || !strings.Contains(ctxOf(t, out), `"x.go"`) {
			t.Fatalf("mode %q, an overlap: systemMessage %q, context %q; want none for the human and the overlap for the agent", m, got, ctxOf(t, out))
		}
		v := View{Overlaps: []core.Warning{warning("a.go", "feat")}, Conflicts: []core.Conflict{conflict("y.go", "feat-b", "content", core.Range{Start: 3, Len: 1})}}
		s.mu.Lock()
		out, _ = d.answerHook("UserPromptSubmit", "", "", s, me, v)
		s.mu.Unlock()
		if got, want := sysMsg(out), `Dugout: y.go conflicts with branch "feat-b"`; got != want {
			t.Fatalf("mode %q, a conflict and an overlap: systemMessage %q, want %q", m, got, want)
		}
	}
}

func onFile(path string) string {
	return fmt.Sprintf(`Dugout: branch "feat" also has %s in flight`, path)
}

// TestSystemMessagePerNewWarning: an answer tells the human, in its
// systemMessage, each overlap and conflict the session is handed for the
// first time, conflicts first; not again, not on a content change, at most 5
// lines and then a count of the rest. Another session hears the same.
func TestSystemMessagePerNewWarning(t *testing.T) {
	d := idle(t, shortDir(t))
	s := scopedSession("s1")
	w := warning("x.go", "feat")
	c := conflict("y.go", "feat-b", "content", core.Range{Start: 3, Len: 1})
	view := func() View { return View{Overlaps: []core.Warning{w}, Conflicts: []core.Conflict{c}} }
	const first = "Dugout: y.go conflicts with branch \"feat-b\"\n" + `Dugout: branch "feat" also has x.go in flight`

	s.mu.Lock()
	out, _ := d.answerHook("UserPromptSubmit", "", "", s, me, view())
	if sysMsg(out) != first || ctxOf(t, out) == "" {
		t.Fatalf("first answer = %s, want the context and %q", asJSON(t, out), first)
	}
	if out, _ := d.answerHook("UserPromptSubmit", "", "", s, me, view()); len(out) != 0 {
		t.Fatalf("nothing new = %s, want {}", asJSON(t, out))
	}
	w.Sides[1].Intent, c.Sides[1].Intent = "migrate payments", "delete y.go"
	out, _ = d.answerHook("UserPromptSubmit", "", "", s, me, view())
	if ctxOf(t, out) == "" || out["systemMessage"] != nil {
		t.Fatalf("a content change = %s, want the context handed again without a systemMessage", asJSON(t, out))
	}
	// A deny carries the conflict it hands over for the first time, and only it.
	z := conflict("z.go", "feat-c", "content", core.Range{Start: 1, Len: 1})
	out, _ = d.answerHook("PreToolUse", "Edit", "z.go", s, me, View{Overlaps: []core.Warning{w}, Conflicts: []core.Conflict{c, z}})
	if hso, _ := out["hookSpecificOutput"].(map[string]any); hso["permissionDecision"] != "deny" || sysMsg(out) != `Dugout: z.go conflicts with branch "feat-c"` {
		t.Fatalf("deny of z.go = %s, want a deny that tells z.go", asJSON(t, out))
	}
	s.mu.Unlock()

	if got := tell(d, scopedSession("s2"), view()); got != first {
		t.Fatalf("another session's systemMessage = %q, want %q", got, first)
	}
	// A session on the other side, on this machine too, hears of its own
	// other side, "mine".
	other := scopedSession("s-other")
	other.mu.Lock()
	out, _ = d.answerHook("UserPromptSubmit", "", "", other, w.Sides[1].Workstream, overlaps(w))
	other.mu.Unlock()
	if got, want := sysMsg(out), `Dugout: branch "mine" also has x.go in flight`; got != want {
		t.Fatalf("the other side's systemMessage = %q, want %q", got, want)
	}

	var many []core.Warning
	for i := range 7 {
		many = append(many, warning(fmt.Sprintf("m%d.go", i), "feat"))
	}
	s3 := scopedSession("s3")
	lines := strings.Split(tell(d, s3, overlaps(many...)), "\n")
	if len(lines) != 6 || lines[5] != "Dugout: and 2 more warnings: dugout status." {
		t.Fatalf("7 new warnings = %q, want 5 lines and the count of 2 more", lines)
	}
	for i, l := range lines[:5] {
		if want := onFile(fmt.Sprintf("m%d.go", i)); l != want {
			t.Fatalf("line %d = %q, want %q", i, l, want)
		}
	}
	// The two past the cap count as told, and are never told again.
	if got, want := tell(d, s3, overlaps(append(many, warning("n.go", "feat"))...)), onFile("n.go"); got != want {
		t.Fatalf("one more = %q, want %q", got, want)
	}
	var six []core.Warning
	for i := range 6 {
		six = append(six, warning(fmt.Sprintf("p%d.go", i), "feat"))
	}
	if lines := strings.Split(tell(d, scopedSession("s4"), overlaps(six...)), "\n"); len(lines) != 6 || lines[5] != "Dugout: and 1 more warning: dugout status." {
		t.Fatalf("6 new warnings = %q, want 5 lines and the count of 1 more", lines)
	}

	// The other side names its file and branch: both go through Sanitize.
	evil := warning("a\"b\n.go", "branch‮\"x")
	if got, want := tell(d, scopedSession("s5"), overlaps(evil)), `Dugout: branch "branch'x" also has a'b.go in flight`; got != want {
		t.Fatalf("unsanitized fields: %q, want %q", got, want)
	}
}

// TestStopTellsTheHuman: a conflict handed over at Stop is told to the human
// like at any other hook, and a Stop with nothing new says nothing.
func TestStopTellsTheHuman(t *testing.T) {
	d := idle(t, shortDir(t))
	s := scopedSession("s1")
	v := View{Conflicts: []core.Conflict{conflict("y.go", "feat-b", "content", core.Range{Start: 3, Len: 1})}}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, _ := d.answerHook("Stop", "", "", s, me, v)
	if got, want := sysMsg(out), `Dugout: y.go conflicts with branch "feat-b"`; got != want || ctxOf(t, out) == "" {
		t.Fatalf("Stop with a new conflict = %s, want the context and %q", asJSON(t, out), want)
	}
	if out, _ := d.answerHook("Stop", "", "", s, me, v); len(out) != 0 {
		t.Fatalf("Stop with nothing new = %s, want {}", asJSON(t, out))
	}
}

// TestSessionEndSaysNothing: SessionEnd drops the session and answers {}:
// Claude Code shows a SessionEnd's output to no one.
func TestSessionEndSaysNothing(t *testing.T) {
	d := idle(t, shortDir(t))
	s := scopedSession("s1")
	d.mu.Lock()
	d.sessions["s1"] = s
	d.mu.Unlock()
	tell(d, s, overlaps(warning("a.go", "feat"), warning("b.go", "feat")))
	in, _ := json.Marshal(map[string]string{"session_id": "s1", "hook_event_name": "SessionEnd"})
	if got := string(d.handleHook(wire.Request{Type: wire.TypeHook, Event: "SessionEnd", Input: in}).Output); got != "{}" {
		t.Fatalf("SessionEnd = %s, want {}", got)
	}
	d.mu.Lock()
	gone := d.sessions["s1"] == nil
	d.mu.Unlock()
	if !gone {
		t.Fatal("SessionEnd did not drop the session")
	}
}

// TestOldRatingsFileIgnored: the ratings.json an older daemon left in the
// state dir, readable or not, neither stops the daemon from starting nor is
// touched, and a rate request from an older CLI is refused as unknown.
func TestOldRatingsFileIgnored(t *testing.T) {
	for name, content := range map[string]string{
		"old":     `{"rated":[{"short":1,"id":"a","kind":"overlap","path":"a.go","branch":"feat","at":"2026-10-20T10:00:00Z"}]}`,
		"garbage": "not json",
	} {
		dir := shortDir(t)
		path := filepath.Join(dir, "ratings.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		d := idle(t, dir)
		if got, want := tell(d, scopedSession("s1"), overlaps(warning("a.go", "feat"))), onFile("a.go"); got != want {
			t.Fatalf("%s: systemMessage = %q, want %q", name, got, want)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != content {
			t.Fatalf("%s: ratings.json = %q (%v), want it untouched", name, raw, err)
		}
		if resp := d.handle(wire.Request{Type: "rate"}); !strings.Contains(resp.Error, "unknown request type") {
			t.Fatalf("%s: rate request = %+v, want an unknown-type error", name, resp)
		}
	}
}
