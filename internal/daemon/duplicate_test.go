package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/judge"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

// fakeJudge finds duplicates where same says, and records each pair it was
// asked about, as the judge receives it.
type fakeJudge struct {
	mu    sync.Mutex
	calls [][2]string
	same  func(a, b string) bool
	err   error
}

func (f *fakeJudge) Name() string { return "fake" }

func (f *fakeJudge) Judge(_ context.Context, a, b string) (judge.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{a, b})
	u := judge.Usage{InputTokens: 100, OutputTokens: 10, USD: 0.001, Priced: true}
	if f.err != nil {
		return judge.Verdict{Usage: u}, f.err
	}
	dup := f.same(a, b)
	reason := "They differ."
	if dup {
		reason = "Both limit failed logins."
	}
	return judge.Verdict{Duplicate: dup, Reason: reason, Usage: u}, nil
}

func (f *fakeJudge) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// loginJudge finds two intents about login limits the same task.
func loginJudge() *fakeJudge {
	return &fakeJudge{same: func(a, b string) bool { return strings.Contains(a, "login") && strings.Contains(b, "login") }}
}

func newDupDetector(d *Daemon) *dupDetector {
	return &dupDetector{d: d, verdicts: map[dupKey]judge.Verdict{}, failed: map[dupKey]dupFailure{}, current: map[string]Duplicate{}}
}

func (o office) intent(root, text string) {
	o.d.worktrees[root].intent = SanitizeIntent(text)
}

// TestDuplicateAcrossFiles: two workstreams of a repo that share
// no file but declared the same task get a possible duplicate each, naming
// the other's session and intent; the judge saw only the two intents, once.
func TestDuplicateAcrossFiles(t *testing.T) {
	o := newOffice(t)
	j := loginJudge()
	o.d.cfg.Judge = j
	rootA, wsA := o.worktree(t, "r", "feat-a", "auth/login.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "middleware/limit.go")
	rootC, _ := o.worktree(t, "r", "feat-c", "docs/readme.md")
	rootX, _ := o.worktree(t, "other", "feat-x", "x.go")
	o.session(t, "sA", rootA, 101, "a-1", true)
	o.session(t, "sB", rootB, 102, "b-2", true)
	o.intent(rootA, "add rate limiting to login")
	o.intent(rootB, "limit login attempts")
	o.intent(rootC, "fix a typo in the readme")
	o.intent(rootX, "limit login attempts") // another repo: never compared
	det := newDupDetector(o.d)
	det.run(context.Background())

	if got := j.count(); got != 3 {
		t.Fatalf("judge calls = %d (%v), want the 3 pairs of repo r", got, j.calls)
	}
	for _, c := range j.calls {
		if c[0] > c[1] {
			t.Fatalf("pair not in order: %q", c)
		}
	}
	va, vb := o.d.viewFor(wsA), o.d.viewFor(wsB)
	if len(va.Duplicates) != 1 || len(vb.Duplicates) != 1 || va.Duplicates[0].ID != vb.Duplicates[0].ID {
		t.Fatalf("duplicates: A %+v, B %+v", va.Duplicates, vb.Duplicates)
	}
	for _, o := range va.Office {
		if o.Workstream == wsB {
			t.Fatal("the duplicate's other side is in the office too")
		}
	}
	ctx := ctxOf(t, Respond("UserPromptSubmit", "", "", NewSession("sA", rootA, 0), wsA, va))
	want := `- possible duplicate ` + va.Duplicates[0].ID[:8] + `: branch "feat-b" (daemon ` + short(string(o.d.id)) + `, session "b-2") declared "limit login attempts", which looks like the same task as yours ("add rate limiting to login"). Why: Both limit failed logins.`
	if !strings.Contains(ctx, "\n"+want+"\n"+dupAdvice+"\n") || !strings.Contains(ctx, "\n"+sendLine) {
		t.Fatalf("A's context = %q\nwant the line %q, the advice and the send hint", ctx, want)
	}
	if ctx := ctxOf(t, Respond("SessionStart", "", "", NewSession("sB", rootB, 0), wsB, vb)); !strings.Contains(ctx, `session "a-1") declared "add rate limiting to login"`) {
		t.Fatalf("B's context = %q", ctx)
	}

	// Nothing changed: the next runs judge nothing again.
	det.run(context.Background())
	det.run(context.Background())
	if got := j.count(); got != 3 {
		t.Fatalf("judge calls after unchanged runs = %d, want 3", got)
	}
	// status and office_who name it too.
	var st wire.Status
	o.ask(t, wire.Request{Type: wire.TypeStatus}, &st)
	if st.Judge.Name != "fake" || st.Judge.Comparisons != 3 || st.Judge.Duplicates != 1 || st.Judge.InputTokens != 300 || st.Judge.USD < 0.0029 {
		t.Fatalf("status judge = %+v", st.Judge)
	}
	var dupOf []wire.StatusDuplicate
	for _, w := range st.Workstreams {
		if w.Branch == "feat-a" {
			dupOf = w.Duplicates
		}
	}
	if len(dupOf) != 1 || dupOf[0].Branch != "feat-b" || dupOf[0].Intent != "limit login attempts" || dupOf[0].Reason != "Both limit failed logins." {
		t.Fatalf("status duplicates of feat-a = %+v", dupOf)
	}
	var of wire.Office
	o.ask(t, wire.Request{Type: wire.TypeOffice, Path: filepath.Join(rootA, "x.go")}, &of)
	found := false
	for _, w := range of.Workstreams {
		if w.Branch == "feat-b" {
			found = w.Duplicate != nil && w.Duplicate.ID == va.Duplicates[0].ID[:8] && w.Duplicate.Reason == "Both limit failed logins."
		} else if w.Duplicate != nil {
			t.Fatalf("office marks %s a duplicate", w.Branch)
		}
	}
	if !found {
		t.Fatalf("office = %+v, want feat-b marked", of.Workstreams)
	}

	// A new intent is judged again; one that is not the same task resolves it.
	o.intent(rootB, "write the release notes")
	det.run(context.Background())
	if got := j.count(); got != 5 {
		t.Fatalf("judge calls after B's new intent = %d, want 5", got)
	}
	if v := o.d.viewFor(wsA); len(v.Duplicates) != 0 {
		t.Fatalf("resolved duplicate still there: %+v", v.Duplicates)
	}
}

// TestDuplicateNotFalse: different tasks give no warning, and a
// pair is compared only when both declared an intent and each has files in
// flight or a session open.
func TestDuplicateNotFalse(t *testing.T) {
	o := newOffice(t)
	j := loginJudge()
	o.d.cfg.Judge = j
	rootA, wsA := o.worktree(t, "r", "feat-a", "a.go")
	rootB, _ := o.worktree(t, "r", "feat-b", "b.go")
	rootC, _ := o.worktree(t, "r", "feat-c") // nothing in flight, no session
	rootD, _ := o.worktree(t, "r", "feat-d", "d.go")
	o.intent(rootA, "add rate limiting to login")
	o.intent(rootB, "migrate the billing tables")
	o.intent(rootC, "limit login attempts")
	_ = rootD // no intent: never compared
	det := newDupDetector(o.d)
	det.run(context.Background())
	if got := j.count(); got != 1 {
		t.Fatalf("judge calls = %d (%v), want only A with B", got, j.calls)
	}
	if v := o.d.viewFor(wsA); len(v.Duplicates) != 0 {
		t.Fatalf("different tasks warned: %+v", v.Duplicates)
	}
	// A session opened on C brings it in, before any file.
	o.session(t, "sC", rootC, 103, "c-3", true)
	det.run(context.Background())
	if v := o.d.viewFor(wsA); len(v.Duplicates) != 1 {
		t.Fatalf("C with a session: %+v", v.Duplicates)
	}
}

// TestDuplicateJudgeFails: a failed comparison warns of nothing, counts as an
// error, and the pair waits judgeRetry before it is asked again; the hour's
// budget caps the comparisons; while paused nothing is compared.
func TestDuplicateJudgeFails(t *testing.T) {
	o := newOffice(t)
	j := loginJudge()
	j.err = errors.New("overloaded")
	o.d.cfg.Judge = j
	rootA, wsA := o.worktree(t, "r", "feat-a", "a.go")
	rootB, _ := o.worktree(t, "r", "feat-b", "b.go")
	o.intent(rootA, "add rate limiting to login")
	o.intent(rootB, "limit login attempts")
	det := newDupDetector(o.d)
	det.run(context.Background())
	det.run(context.Background())
	if got, st := j.count(), o.d.judgeStats(); got != 1 || st.Errors != 1 || st.LastError != "overloaded" || len(o.d.viewFor(wsA).Duplicates) != 0 {
		t.Fatalf("calls %d, stats %+v", got, st)
	}
	det.failed[keyOfIntents("add rate limiting to login", "limit login attempts")] = dupFailure{at: time.Now().Add(-judgeRetry), msg: "overloaded"}
	j.err = nil
	det.run(context.Background())
	if j.count() != 2 || len(o.d.viewFor(wsA).Duplicates) != 1 {
		t.Fatalf("after judgeRetry: calls %d, view %+v", j.count(), o.d.viewFor(wsA).Duplicates)
	}

	// The budget: a full hour of comparisons stops the next one, and the last
	// verdict on the pair stands meanwhile.
	for range judgeBudget {
		det.calls = append(det.calls, time.Now())
	}
	o.intent(rootB, "limit login attempts per user")
	det.run(context.Background())
	if j.count() != 2 {
		t.Fatalf("over budget, the judge was called: %d", j.count())
	}
	det.calls = nil

	// Paused: nothing compared, nothing dropped.
	if err := os.WriteFile(statedir.PausedPath(o.d.cfg.StateDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	det.run(context.Background())
	if j.count() != 2 || len(o.d.viewFor(wsA).Duplicates) != 1 {
		t.Fatalf("paused: calls %d", j.count())
	}
	os.Remove(statedir.PausedPath(o.d.cfg.StateDir))
	det.run(context.Background())
	if j.count() != 3 {
		t.Fatalf("resumed: calls %d", j.count())
	}
}

// TestDuplicateRemote: another daemon's workstream, as serve has it, is
// compared with this machine's, and two other daemons' never are.
func TestDuplicateRemote(t *testing.T) {
	var mu sync.Mutex
	dump := wire.StateDump{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(dump)
	}))
	t.Cleanup(srv.Close)
	d, err := New(Config{StateDir: shortDir(t), Server: srv.URL, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	o := office{d: d, reg: newRegistry(t)}
	j := loginJudge()
	o.d.cfg.Judge = j
	rootA, wsA := o.worktree(t, "r", "feat-a", "a.go")
	o.intent(rootA, "add rate limiting to login")
	mu.Lock()
	dump.Workstreams = []core.Workstream{
		{ID: "zz:far", Repo: "r", Branch: "feat-far", Files: []string{"far.go"}, Intent: "limit login attempts"},
		{ID: "yy:other", Repo: "r", Branch: "feat-y", Files: []string{"y.go"}, Intent: "limit login attempts too"},
		{ID: core.WorkstreamID(string(o.d.id) + ":stale"), Repo: "r", Branch: "mine", Files: []string{"m.go"}, Intent: "login"},
	}
	mu.Unlock()
	det := newDupDetector(o.d)
	det.run(context.Background())
	if got := j.count(); got != 2 {
		t.Fatalf("judge calls = %d (%v), want A with each remote", got, j.calls)
	}
	v := o.d.viewFor(wsA)
	if len(v.Duplicates) != 2 {
		t.Fatalf("duplicates = %+v", v.Duplicates)
	}
	ctx := ctxOf(t, Respond("UserPromptSubmit", "", "", NewSession("sA", rootA, 0), wsA, v))
	if !strings.Contains(ctx, `branch "feat-far" (daemon zz) declared`) || !strings.Contains(ctx, "\n"+dupAdvice+dupFar+"\n") || strings.Contains(ctx, sendLine) {
		t.Fatalf("remote duplicate = %q, want no session, the far advice and no send hint", ctx)
	}
}

// TestDuplicateDelivery: a duplicate is handed at a turn's start and at the
// next PostToolUse once, again when the other side's intent changes but not
// when the session's own does, never at a Stop, and never denies.
func TestDuplicateDelivery(t *testing.T) {
	other := core.Side{Daemon: "d2", Workstream: "d2:other", Branch: "feat-b", Intent: "limit login attempts", Session: "b-2"}
	dup := Duplicate{ID: DuplicateID("r", me, other.Workstream), Repo: "r", Reason: "Both limit logins.",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: me, Branch: "mine", Intent: "add rate limiting to login"}, other}}
	view := func(d Duplicate) View { return View{Duplicates: []Duplicate{d}} }

	s := NewSession("s1", "/repo", 0)
	if out := Respond("Stop", "", "", s, me, view(dup)); len(out) != 0 {
		t.Fatalf("Stop handed a duplicate: %s", asJSON(t, out))
	}
	if out := Respond("PreToolUse", "Edit", "a.go", s, me, view(dup)); len(out) != 0 {
		t.Fatalf("a duplicate held an edit: %s", asJSON(t, out))
	}
	if ctx := ctxOf(t, Respond("PostToolUse", "Edit", "a.go", s, me, view(dup))); !strings.Contains(ctx, "possible duplicate") {
		t.Fatalf("PostToolUse = %q", ctx)
	}
	if out := Respond("PostToolUse", "Edit", "a.go", s, me, view(dup)); len(out) != 0 {
		t.Fatalf("handed twice: %s", asJSON(t, out))
	}
	if out := Respond("UserPromptSubmit", "", "", s, me, view(dup)); len(out) != 0 {
		t.Fatalf("handed again at the next prompt: %s", asJSON(t, out))
	}
	mine := dup
	mine.Sides[0].Intent = "add rate limiting to the login endpoint"
	if out := Respond("UserPromptSubmit", "", "", s, me, view(mine)); len(out) != 0 {
		t.Fatalf("own new intent handed it again: %s", asJSON(t, out))
	}
	theirs := dup
	theirs.Sides[1].Intent = "throttle login attempts per IP"
	if ctx := ctxOf(t, Respond("UserPromptSubmit", "", "", s, me, view(theirs))); !strings.Contains(ctx, "throttle login attempts per IP") {
		t.Fatalf("their new intent = %q", ctx)
	}
}

// TestDuplicatesFit: duplicates past the answer's room are counted, the advice
// goes with the first one shown, and an intent shown in a duplicate is not
// repeated in the office line.
func TestDuplicatesFit(t *testing.T) {
	var dups []Duplicate
	for i := range 40 {
		ws := core.WorkstreamID("d2:o" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		dups = append(dups, Duplicate{ID: DuplicateID("r", me, ws), Reason: strings.Repeat("r", 150),
			Sides: [2]core.Side{{Workstream: me, Intent: strings.Repeat("m", 280)}, {Daemon: "d2", Workstream: ws, Branch: "b", Intent: strings.Repeat("o", 280)}}})
	}
	ctx := RenderContext(me, View{Duplicates: dups}, true, ModeAuto)
	if width(ctx) > maxAnswer+1 || !strings.Contains(ctx, " more possible duplicates: dugout status.") || strings.Count(ctx, dupAdvice) != 1 {
		t.Fatalf("width %d, ctx tail %q", width(ctx), ctx[len(ctx)-300:])
	}
	if ctx := RenderContext(me, View{}, true, ModeAuto); strings.Contains(ctx, dupAdvice) {
		t.Fatal("advice without a duplicate")
	}
}

// TestDuplicatesEndToEnd drives the daemon's own loop: two worktrees sharing
// no file declare the same task, and each session's next hook carries the
// possible duplicate.
func TestDuplicatesEndToEnd(t *testing.T) {
	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	wt := f.AddWorktree(t, "wtb", "feat-b")
	testgit.WriteFile(t, filepath.Join(f.Clone, "login.go"), "a\n")
	testgit.WriteFile(t, filepath.Join(wt, "limit.go"), "b\n")
	j := loginJudge()
	r := startDaemonWith(t, "", func(d *Daemon) { d.cfg.Judge = j })
	for _, s := range []struct{ id, cwd, intent string }{{"sA", f.Clone, "add rate limiting to login"}, {"sB", wt, "limit login attempts"}} {
		hook(t, r.sock, "SessionStart", s.id, s.cwd, "", "")
		eventually(t, 10*time.Second, s.id+" declares", func() bool {
			return call(t, r.sock, wire.Request{Type: wire.TypeIntent, SessionID: s.id, Text: s.intent}).Error == ""
		})
	}
	for _, s := range []struct{ id, cwd, other string }{{"sA", f.Clone, "limit login attempts"}, {"sB", wt, "add rate limiting to login"}} {
		eventually(t, 10*time.Second, s.id+" hears of the duplicate", func() bool {
			out := hook(t, r.sock, "PostToolUse", s.id, s.cwd, "Bash", "")
			return strings.Contains(out, "possible duplicate") && strings.Contains(out, `declared \"`+s.other+`\"`)
		})
	}
}
