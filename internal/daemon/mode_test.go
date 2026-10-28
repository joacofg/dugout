package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"auto": ModeAuto, " Scoped ": ModeScoped, "SUPERVISED": ModeSupervised} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "autonomous", "yolo"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) took it", in)
		}
	}
	if Mode("").Or() != ModeAuto {
		t.Fatal("a mode never chosen should work as auto")
	}
}

// TestModeAdvice: the yielder's advice, in a deny and in a context, is its
// mode's: auto resolves without its human and stops only on the triggers, scoped
// aligns or yields within its task, supervised asks before changing its plan. A
// mode never chosen advises as auto. The keeper's advice is the same in every
// mode and never sends it to its human.
func TestModeAdvice(t *testing.T) {
	c := conflictWith(first, "x.go", core.Snippet{Lines: []string{"a := 1"}})
	cases := []struct {
		mode          Mode
		deny, context []string // in the yielder's answer
		never         []string // in either of the yielder's answers
	}{
		{ModeAuto,
			[]string{"retry the edit aligned with the other one across the whole span shown", "or without that span, without asking your human."},
			[]string{"settle it yourself before committing", "or drop that span from your change, without asking your human."},
			[]string{"within the scope of your task", "wait for the answer"}},
		{ModeScoped,
			[]string{"retry the edit aligned with the other one across the whole span shown", ", or without that span, within the scope of your task. Escalate it to both humans with office_escalate only if yielding would force you to leave part of your task undone."},
			[]string{"before committing align your change with the other one's across the whole span shown", ", or drop that span from your change, within the scope of your task. Escalate it to both humans with office_escalate only if yielding would force you to leave part of your task undone."},
			[]string{"without asking", "wait for the answer", "shared contract"}},
		{ModeSupervised,
			[]string{"do not retry it without your human: propose retrying it aligned with the other one across the whole span shown", ", or without that span, and wait for the answer."},
			[]string{"do not change your plan without your human: before committing propose aligning your change", ", or dropping that span from your change, and wait for the answer."},
			[]string{"without asking", "shared contract", "within the scope of your task"}},
	}
	// The triggers, and that the human is only told afterwards, are auto's.
	triggers := []string{
		"Escalate it to both humans with office_escalate only if the two tasks ask for contradictory things in that span",
		"if yielding would force you to leave part of your task undone",
		"or if aligning means deleting something or changing a shared contract (an API, a schema, a migration)",
		"otherwise tell your human at the end, in one line, how you settled it.",
	}
	cases[0].deny = append(cases[0].deny, triggers...)
	cases[0].context = append(cases[0].context, triggers...)
	keeperDeny, keeperCtx := denyIn(t, ModeAuto, first, c), contextIn(t, ModeAuto, first, c)
	for _, tc := range cases {
		deny, ctx := denyIn(t, tc.mode, me, c), contextIn(t, tc.mode, me, c)
		for _, want := range tc.deny {
			if !strings.Contains(deny, want) {
				t.Errorf("%s: yielder deny lacks %q: %q", tc.mode, want, deny)
			}
		}
		for _, want := range tc.context {
			if !strings.Contains(ctx, want) {
				t.Errorf("%s: yielder context lacks %q: %q", tc.mode, want, ctx)
			}
		}
		for _, bad := range tc.never {
			if strings.Contains(deny, bad) || strings.Contains(ctx, bad) {
				t.Errorf("%s: yielder advice says %q: %q / %q", tc.mode, bad, deny, ctx)
			}
		}
		if got := denyIn(t, tc.mode, first, c); got != keeperDeny || strings.Contains(got, humanLine) {
			t.Errorf("%s: keeper deny = %q, want %q", tc.mode, got, keeperDeny)
		}
		if got := contextIn(t, tc.mode, first, c); got != keeperCtx || strings.Contains(got, "human") {
			t.Errorf("%s: keeper context = %q, want %q", tc.mode, got, keeperCtx)
		}
	}
	if denyIn(t, "", me, c) != denyIn(t, ModeAuto, me, c) || contextIn(t, "", me, c) != contextIn(t, ModeAuto, me, c) {
		t.Fatal("a mode never chosen should advise as auto")
	}
}

// TestModeTightRoom: in every mode, me keeps one conflict and yields another
// under every room from the smallest to the full answer: the answer never
// passes its room, and the align advice appears only with the yielded lines.
func TestModeTightRoom(t *testing.T) {
	snip := func(tag string) core.Snippet {
		return core.Snippet{Lines: []string{tag + " := 1", tag + " := 2", tag + " := 3"}}
	}
	cs := []core.Conflict{conflictWith("d2:later", "a.go", snip("kept")), conflictWith(first, "b.go", snip("yielded"))}
	for _, m := range Modes {
		base := utf8.RuneCountInString(RenderContext(me, View{}, false, m))
		full := utf8.RuneCountInString(RenderContext(me, View{Conflicts: cs}, false, m))
		for room := base; room <= full; room++ {
			ctx := renderContext(me, View{Conflicts: cs}, false, m, room)
			if n := utf8.RuneCountInString(ctx); n > room {
				t.Fatalf("%s, room %d: %d runes", m, room, n)
			}
			if got, shown := strings.Contains(ctx, alignSentence), strings.Contains(ctx, "yielded := "); got != shown {
				t.Fatalf("%s, room %d: align advice = %v with the yielded lines shown = %v: %q", m, room, got, shown, ctx)
			}
		}
	}
}

// TestModeLineAtSessionStart: a SessionStart tells the session its repo's
// mode, then asks for its intent; a mode never chosen is told as the default.
func TestModeLineAtSessionStart(t *testing.T) {
	for _, m := range append([]Mode{""}, Modes...) {
		s := NewSession("s1", "/repo", 0)
		s.Mode = m
		ctx := ctxOf(t, Respond("SessionStart", "", "", s, me, View{}))
		lines := strings.Split(ctx, "\n")
		if len(lines) != 2 || lines[0] != modeLine(m) || !strings.Contains(lines[1], "office_declare_intent") {
			t.Fatalf("%q: SessionStart = %q, want the mode line and then the ask", m, ctx)
		}
		if def := strings.Contains(lines[0], "default"); def != (m == "") {
			t.Fatalf("%q: mode line says default = %v: %q", m, def, lines[0])
		}
		if !strings.HasPrefix(lines[0], "Dugout: mode "+string(m.Or())+" in this repo") || !strings.Contains(lines[0], "/dugout") {
			t.Fatalf("%q: mode line = %q", m, lines[0])
		}
	}
	if ctx := ctxOf(t, Respond("SessionStart", "", "", NewSession("s1", "/repo", 0), me, View{})); ctx != askFor("s1") {
		t.Fatalf("default SessionStart = %q, want %q", ctx, askFor("s1"))
	}
}

// TestModeSwitchMidSession: when the repo's mode changes mid-session, the next
// answer that carries context tells the new mode, once, and hands a conflict
// it was already handed again with the new mode's advice. A Stop waits for
// the next one; a deny keeps its once-per-conflict rule.
func TestModeSwitchMidSession(t *testing.T) {
	c := conflictWith(first, "x.go", core.Snippet{Lines: []string{"a := 1"}})
	v := View{Conflicts: []core.Conflict{c}}
	s := NewSession("s1", "/repo", 0)
	if ctx := ctxOf(t, Respond("SessionStart", "", "", s, me, v)); !strings.Contains(ctx, "without asking your human") {
		t.Fatalf("SessionStart in the default mode = %q", ctx)
	}
	if out := Respond("UserPromptSubmit", "", "", s, me, v); len(out) != 0 {
		t.Fatalf("nothing new: %s", asJSON(t, out))
	}
	s.Mode = ModeSupervised
	if out := Respond("Stop", "", "", s, me, View{}); len(out) != 0 {
		t.Fatalf("Stop after the switch = %s, want nothing", asJSON(t, out))
	}
	ctx := ctxOf(t, Respond("UserPromptSubmit", "", "", s, me, v))
	if !strings.HasSuffix(ctx, "</dugout-context>\n"+modeLine(ModeSupervised)) || !strings.Contains(ctx, "do not change your plan without your human") || strings.Contains(ctx, "without asking") {
		t.Fatalf("after the switch = %q, want the conflict in supervised and the new mode", ctx)
	}
	if out := Respond("UserPromptSubmit", "", "", s, me, v); len(out) != 0 {
		t.Fatalf("told twice: %s", asJSON(t, out))
	}
	// Back to auto: the switch with no conflict to hand over is the mode line alone.
	s.Mode = ModeAuto
	if ctx := ctxOf(t, Respond("PostToolUse", "", "", s, me, View{})); ctx != modeLine(ModeAuto) {
		t.Fatalf("switch with nothing to hand = %q", ctx)
	}
	if !strings.Contains(denyIn(t, ModeScoped, me, c), "within the scope of your task") {
		t.Fatal("a deny should carry its session's mode")
	}
}

func modeOf(t *testing.T, sock, path, set string) wire.Mode {
	t.Helper()
	resp := call(t, sock, wire.Request{Type: wire.TypeMode, Path: path, Text: set})
	if resp.Error != "" {
		t.Fatalf("mode %q of %s: %s", set, path, resp.Error)
	}
	var m wire.Mode
	if err := json.Unmarshal(resp.Output, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestModePersistsPerRepo: a mode set through the daemon is the repo's, for
// every worktree of it, saved in modes.json, and a new daemon on the same state
// dir starts with it. An unknown mode is refused and changes nothing.
func TestModePersistsPerRepo(t *testing.T) {
	f, wt := overlap(t)
	r := startDaemon(t, "")
	if m := modeOf(t, r.sock, f.Clone, ""); m.Mode != "" || m.Repo == "" {
		t.Fatalf("mode never chosen = %+v", m)
	}
	if m := modeOf(t, r.sock, f.Clone, "supervised"); m.Mode != "supervised" {
		t.Fatalf("set = %+v", m)
	}
	if m := modeOf(t, r.sock, wt, ""); m.Mode != "supervised" {
		t.Fatalf("the repo's other worktree = %+v, want supervised", m)
	}
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeMode, Path: f.Clone, Text: "yolo"}); resp.Error == "" {
		t.Fatal("an unknown mode was taken")
	}
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeMode, Path: "relative", Text: "auto"}); resp.Error == "" {
		t.Fatal("a relative path was taken")
	}
	stored, err := statedir.LoadModes(r.dir)
	if err != nil || len(stored) != 1 {
		t.Fatalf("modes.json = %v, %v", stored, err)
	}
	for _, m := range stored {
		if m != "supervised" {
			t.Fatalf("modes.json = %v", stored)
		}
	}
	d, err := New(Config{StateDir: r.dir, Socket: filepath.Join(shortDir(t), "s.sock"), Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	for repo := range stored {
		if d.modes[repo] != ModeSupervised {
			t.Fatalf("new daemon's modes = %v", d.modes)
		}
	}
}

// TestModeReachesHooks: a session's hooks answer in its repo's mode, from the
// first SessionStart, before the worktree's first scan, to a switch the human
// makes mid-session. The mode goes to the agent, not to the human, who hears
// only of warnings.
func TestModeReachesHooks(t *testing.T) {
	f, wt := overlap(t)
	r := startDaemon(t, "")
	modeOf(t, r.sock, f.Clone, "scoped")
	var out struct {
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput struct{ AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.HookSpecificOutput.AdditionalContext, modeLine(ModeScoped)) {
		t.Fatalf("SessionStart context = %q, want the scoped mode line first", out.HookSpecificOutput.AdditionalContext)
	}
	if out.SystemMessage != "" {
		t.Fatalf("SessionStart systemMessage = %q, want none", out.SystemMessage)
	}
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 15*time.Second, "the conflict reaches sA", func() bool {
		return strings.Contains(hook(t, r.sock, "PostToolUse", "sA", f.Clone, "", ""), "conflict")
	})
	modeOf(t, r.sock, f.Clone, "supervised")
	got := hook(t, r.sock, "UserPromptSubmit", "sA", f.Clone, "", "")
	if !strings.Contains(got, "Dugout: mode supervised in this repo") || !strings.Contains(got, "conflict") {
		t.Fatalf("UserPromptSubmit after the switch = %s, want the conflict again and the new mode", got)
	}
}
