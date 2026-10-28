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
	"github.com/joacofg/dugout/internal/repoconfig"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

// shown records the desktop notifications a daemon tried, and fails them
// with err when set.
type shown struct {
	mu  sync.Mutex
	ns  []Notification
	err error
}

func (s *shown) notify(_ context.Context, n Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ns = append(s.ns, n)
	return s.err
}

func (s *shown) all() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.ns...)
}

// escOffice is an office with Ana's feat-a and Bruno's feat-b in conflict on
// shared.go, a session on each, a fake desktop and a clock the test moves.
type escOffice struct {
	office
	desk       *shown
	now        time.Time
	sA, sB     *Session
	wsA, wsB   core.WorkstreamID
	rootA      string
	c          core.Conflict
	keeper     core.WorkstreamID
	keeperName string
}

func newEscOffice(t *testing.T) *escOffice {
	t.Helper()
	o := &escOffice{office: newOffice(t), desk: &shown{}, now: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)}
	o.d.cfg.Notify = o.desk.notify
	o.d.clock = func() time.Time { return o.now }
	rootA, wsA := o.worktree(t, "r", "feat-a", "shared.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "shared.go")
	for root, person := range map[string]string{rootA: "Ana", rootB: "Bruno"} {
		o.d.worktrees[root].snap.Person = person
		o.d.worktrees[root].snap.EscalateAfter = 10 * time.Minute
	}
	o.rootA, o.wsA, o.wsB = rootA, wsA, wsB
	o.sA = o.session(t, "sA", rootA, 101, "a-1", true)
	o.sB = o.session(t, "sB", rootB, 102, "b-2", true)
	o.c = o.conflict(wsA, wsB, "shared.go")
	for i := range o.c.Sides {
		o.c.Sides[i].Person = map[core.WorkstreamID]string{wsA: "Ana", wsB: "Bruno"}[o.c.Sides[i].Workstream]
	}
	o.d.conflicts[o.c.ID], o.d.spans[o.c.ID] = o.c, o.c
	o.d.metrics.Sync(o.d.id, []core.Conflict{o.c}, nil, millis(o.now))
	o.keeper, o.keeperName = o.c.Keeper(), "Ana"
	if o.keeper == wsB {
		o.keeperName = "Bruno"
	}
	return o
}

// warn records that a session of ws was handed the conflict now.
func (o *escOffice) warn(ws core.WorkstreamID) {
	o.d.mu.Lock()
	o.d.noteWarnedLocked(ws, []core.Conflict{o.c}, o.now)
	o.d.mu.Unlock()
}

// tally is what the metrics measured of conflict id so far.
func (o *escOffice) tally(id string) core.Tally {
	o.d.mu.Lock()
	defer o.d.mu.Unlock()
	r, _ := o.d.metrics.Get(id)
	return r.Tally
}

// news is what session s of ws hears of escalations at event.
func (o *escOffice) news(event string, s *Session, ws core.WorkstreamID) (string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, human, terminal := o.d.escalationNews(event, s, ws)
	return strings.Join(ctx, "\n"), human, terminal
}

// TestTimeRuleEscalates: a conflict still standing the
// repo's delay after both sides were warned goes to both humans once, by
// name, with what to decide: one desktop notification, and each session hears
// of it at its next hook, in the block and with Dugout's word after it.
func TestTimeRuleEscalates(t *testing.T) {
	o := newEscOffice(t)
	o.d.checkEscalations()
	o.warn(o.wsA)
	o.now = o.now.Add(time.Hour)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("only one side warned, an hour on: %d notifications", len(n))
	}
	o.warn(o.wsB)
	o.now = o.now.Add(10*time.Minute - time.Second)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("a second before the delay: %d notifications", len(n))
	}
	o.now = o.now.Add(time.Second)
	o.d.checkEscalations()
	o.d.checkEscalations()
	ns := o.desk.all()
	if len(ns) != 1 {
		t.Fatalf("at the delay, checked twice: %d notifications, want 1", len(ns))
	}
	if got := o.tally(o.c.ID); got != (core.Tally{RuleEscalations: 1}) {
		t.Fatalf("metrics of the escalated conflict: %+v, want one rule escalation", got)
	}
	n := ns[0]
	for _, want := range []string{`"shared.go" still standing 10 min after both agents were warned`, `Ana (branch "feat-a")`, `Bruno (branch "feat-b")`,
		"Dugout's rule keeps " + o.keeperName + "'s lines: decide whether"} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("notification body %q lacks %q", n.Body, want)
		}
	}
	if n.Title != escTitle || !strings.Contains(n.Subtitle, "Ana") || !strings.Contains(n.Subtitle, "Bruno") || !strings.HasSuffix(n.Subtitle, `, "shared.go"`) || n.Urgent {
		t.Errorf("notification %+v", n)
	}
	for _, side := range []struct {
		s  *Session
		ws core.WorkstreamID
	}{{o.sA, o.wsA}, {o.sB, o.wsB}} {
		if ctx, _, _ := o.news("PreToolUse", side.s, side.ws); ctx != "" {
			t.Fatalf("%s at PreToolUse heard %q", side.s.ID, ctx)
		}
		ctx, human, terminal := o.news("PostToolUse", side.s, side.ws)
		if !strings.HasPrefix(ctx, "<dugout-context>\n") || !strings.Contains(ctx, "- escalated to both humans: Conflict in \"shared.go\"") ||
			!strings.Contains(ctx, "</dugout-context>\nDugout: that conflict went to both humans") || strings.Contains(ctx, "could not show") || terminal != "" {
			t.Fatalf("%s's agent heard %q, terminal %q", side.s.ID, ctx, terminal)
		}
		if !strings.HasPrefix(human, "Dugout escalation: Conflict in \"shared.go\"") {
			t.Fatalf("%s's human read %q", side.s.ID, human)
		}
		if ctx, human, _ := o.news("UserPromptSubmit", side.s, side.ws); ctx != "" || human != "" {
			t.Fatalf("%s heard it twice: %q, %q", side.s.ID, ctx, human)
		}
	}
	// A new session on the worktree hears of it too.
	late := o.session(t, "sA2", o.rootA, 103, "a-3", true)
	if ctx, _, _ := o.news("SessionStart", late, o.wsA); !strings.Contains(ctx, "escalated to both humans") {
		t.Fatalf("a new session on feat-a heard %q", ctx)
	}
}

// TestYieldDoesNotEscalate (done 3): a conflict settled by a yield, recorded
// or by the lines no longer conflicting, never escalates; one that comes back
// starts the time rule over.
func TestYieldDoesNotEscalate(t *testing.T) {
	o := newEscOffice(t)
	o.warn(o.wsA)
	o.warn(o.wsB)
	yielder := o.sB
	if o.keeper == o.wsB {
		yielder = o.sA
	}
	if resp := o.d.handle(wire.Request{Type: wire.TypeYield, SessionID: yielder.ID, Conflict: o.c.ID}); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	o.now = o.now.Add(time.Hour)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("an agreed yield escalated: %+v", n)
	}
	o.d.mu.Lock()
	delete(o.d.stances, o.c.ID)
	delete(o.d.conflicts, o.c.ID) // the yielder aligned: the lines merge
	o.d.mu.Unlock()
	o.now = o.now.Add(time.Hour)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("a resolved conflict escalated: %+v", n)
	}
	o.d.mu.Lock()
	o.d.conflicts[o.c.ID] = o.c // it comes back
	_, warned := o.d.warned[o.c.ID]
	o.d.mu.Unlock()
	if warned {
		t.Fatal("the resolved conflict's warnings were kept")
	}
	o.d.checkEscalations()
	o.warn(o.wsA)
	o.warn(o.wsB)
	o.now = o.now.Add(9 * time.Minute)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("the conflict that came back escalated before its own delay: %+v", n)
	}
	o.now = o.now.Add(time.Minute)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 1 {
		t.Fatalf("the conflict that came back, after its delay: %d notifications, want 1", len(n))
	}
}

// TestDisputeEscalates: both branches claiming the span is agents that cannot
// agree on the channel, and escalates at once, without waiting the delay.
func TestDisputeEscalates(t *testing.T) {
	o := newEscOffice(t)
	o.warn(o.wsA)
	o.warn(o.wsB)
	for _, s := range []string{"sA", "sB"} {
		if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: s, Conflict: o.c.ID}); resp.Error != "" {
			t.Fatal(resp.Error)
		}
	}
	o.d.checkEscalations()
	ns := o.desk.all()
	if len(ns) != 1 || !strings.Contains(ns[0].Body, `both claimed the conflicting lines of "shared.go": decide who keeps them.`) {
		t.Fatalf("both claimed: %+v", ns)
	}
}

// TestTimeRuleOff: escalate_after off turns the time rule off.
func TestTimeRuleOff(t *testing.T) {
	o := newEscOffice(t)
	for _, wt := range o.d.worktrees {
		wt.snap.EscalateAfter = 0
	}
	o.warn(o.wsA)
	o.warn(o.wsB)
	o.now = o.now.Add(24 * time.Hour)
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("time rule off: %+v", n)
	}
}

// TestDesktopFailureToldInSession: a notification the desktop could not show
// is never dropped: each session's agent is told to ask its human there,
// and its terminal gets it once.
func TestDesktopFailureToldInSession(t *testing.T) {
	o := newEscOffice(t)
	o.desk.err = errors.New("no desktop notifications on linux yet")
	o.warn(o.wsA)
	o.warn(o.wsB)
	o.now = o.now.Add(10 * time.Minute)
	o.d.checkEscalations()
	ctx, _, terminal := o.news("Stop", o.sA, o.wsA)
	if !strings.Contains(ctx, "Dugout: this machine could not show your human a desktop notification (no desktop notifications on linux yet): tell them in this session now") {
		t.Fatalf("the agent heard %q", ctx)
	}
	if !strings.HasPrefix(terminal, escTitle+": Conflict in \"shared.go\"") {
		t.Fatalf("terminal = %q", terminal)
	}
	if _, _, terminal := o.news("Stop", o.sA, o.wsA); terminal != "" {
		t.Fatalf("the terminal got it twice: %q", terminal)
	}
}

// TestDesktopNotifyElsewhere: off a Mac there is no desktop notification yet,
// and DesktopNotify says so rather than showing nothing.
func TestDesktopNotifyElsewhere(t *testing.T) {
	was := notifyGOOS
	notifyGOOS = "linux"
	t.Cleanup(func() { notifyGOOS = was })
	if err := DesktopNotify(context.Background(), Notification{Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("DesktopNotify on linux = %v", err)
	}
}

// TestEscalateTool (done 2): office_escalate shows this machine's desktop
// notification at once, saying what to decide and naming both sides, and the
// other session hears of it at its next hook; the asking one already knows.
// One question per session: the next waits for a prompt from its human. The
// time rule does not repeat what an agent escalated.
func TestEscalateTool(t *testing.T) {
	o := newEscOffice(t)
	o.warn(o.wsA)
	o.warn(o.wsB)
	ask := func(session, reason string) wire.Response {
		return o.d.handle(wire.Request{Type: wire.TypeEscalate, SessionID: session, Text: reason, Urgency: "urgent"})
	}
	resp := ask("sA", "Ana keeps the 500 cap, Bruno raises it to 2000: which one stays?")
	var e wire.Escalated
	if resp.Error != "" || json.Unmarshal(resp.Output, &e) != nil {
		t.Fatalf("escalate = %+v", resp)
	}
	if e.ID != o.c.ID[:8] || e.Path != "shared.go" || e.Mine != `Ana (branch "feat-a")` || e.Theirs != `Bruno (branch "feat-b")` ||
		e.Desktop != "" || e.Other != "local" || e.Session != "b-2" {
		t.Fatalf("escalated %+v", e)
	}
	if got := o.tally(o.c.ID); got != (core.Tally{AgentEscalations: 1}) {
		t.Fatalf("metrics after office_escalate: %+v, want one agent escalation", got)
	}
	ns := o.desk.all()
	if len(ns) != 1 || !ns[0].Urgent || ns[0].Title != escTitle+" (urgent)" ||
		ns[0].Body != `Ana (branch "feat-a") asks about the conflict in "shared.go" with Bruno (branch "feat-b"): Ana keeps the 500 cap, Bruno raises it to 2000: which one stays?` {
		t.Fatalf("notifications %+v", ns)
	}
	if ctx, _, _ := o.news("PostToolUse", o.sA, o.wsA); ctx != "" {
		t.Fatalf("the asking session heard its own escalation again: %q", ctx)
	}
	if ctx, human, _ := o.news("PostToolUse", o.sB, o.wsB); !strings.Contains(ctx, "which one stays?") || !strings.Contains(human, "Ana (branch \"feat-a\") asks about") {
		t.Fatalf("the other session heard %q, its human %q", ctx, human)
	}
	if resp := ask("sA", "and another thing"); !strings.Contains(resp.Error, "one question at a time") {
		t.Fatalf("second question before the answer = %+v", resp)
	}
	o.news("UserPromptSubmit", o.sA, o.wsA) // the human answers in the session
	if resp := ask("sA", "and another thing"); resp.Error != "" {
		t.Fatalf("a question after the answer = %+v", resp)
	}
	o.now = o.now.Add(time.Hour)
	o.d.checkEscalations()
	if n := len(o.desk.all()); n != 2 {
		t.Fatalf("after two questions and the delay: %d notifications, want 2 (the time rule does not repeat them)", n)
	}
	for _, tc := range []struct {
		req  wire.Request
		want string
	}{
		{wire.Request{Type: wire.TypeEscalate, SessionID: "nope", Text: "r"}, "does not know that session"},
		{wire.Request{Type: wire.TypeEscalate, SessionID: "sB", Text: " "}, "the reason is empty"},
		{wire.Request{Type: wire.TypeEscalate, SessionID: "sB", Text: "r", Conflict: "zzzz"}, "does not see that conflict"},
	} {
		if resp := o.d.handle(tc.req); !strings.Contains(resp.Error, tc.want) {
			t.Errorf("%+v: %+v, want an error with %q", tc.req, resp, tc.want)
		}
	}
	o.d.mu.Lock()
	delete(o.d.conflicts, o.c.ID)
	o.d.mu.Unlock()
	if resp := ask("sB", "r"); !strings.Contains(resp.Error, "sees no conflict on your branch") {
		t.Fatalf("escalate with no conflict = %+v", resp)
	}
}

// TestEscalateRemote: the other side on another machine gets it through
// serve, which hands it to that daemon's stream; without a server, or with
// that daemon not connected, the answer says it did not reach them.
func TestEscalateRemote(t *testing.T) {
	o := newEscOffice(t)
	far := core.Side{Daemon: "d9", Workstream: "d9:far", Branch: "feat-far", Person: "Caro"}
	c := core.Conflict{ID: core.ConflictID("r", "far.go", o.wsA, far.Workstream), Repo: "r", Path: "far.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: o.d.id, Workstream: o.wsA, Branch: "feat-a", Person: "Ana"}, far}}
	if far.Workstream < o.wsA {
		c.Sides[0], c.Sides[1] = c.Sides[1], c.Sides[0]
	}
	o.d.conflicts = map[string]core.Conflict{c.ID: c}
	var e wire.Escalated
	if resp := o.d.handle(wire.Request{Type: wire.TypeEscalate, SessionID: "sA", Text: "which?"}); json.Unmarshal(resp.Output, &e) != nil ||
		e.Other != "unreached" || !strings.Contains(e.OtherWhy, "without a server") || e.Theirs != `Caro (branch "feat-far")` {
		t.Fatalf("no server: %+v %+v", resp, e)
	}

	srv := httptest.NewServer(serve.New().Handler())
	t.Cleanup(func() { closeServer(srv) })
	for _, fs := range []core.FullState{
		{Daemon: o.d.id, Workstreams: []core.Workstream{{ID: o.wsA, Repo: "r", Branch: "feat-a", Files: []string{"far.go"}, Person: "Ana"}}},
		{Daemon: "d9", Workstreams: []core.Workstream{{ID: "d9:far", Repo: "r", Branch: "feat-far", Files: []string{"far.go"}, Person: "Caro"}}},
	} {
		b, _ := json.Marshal(wire.Message{Type: wire.TypeFullState, FullState: fs})
		resp, err := http.Post(srv.URL+"/v1/msg", "application/json", strings.NewReader(string(b)))
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("post state: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	o.d.cfg.Server, o.d.uplink.Server = srv.URL, srv.URL
	o.news("UserPromptSubmit", o.sA, o.wsA)
	if resp := o.d.handle(wire.Request{Type: wire.TypeEscalate, SessionID: "sA", Text: "which?"}); json.Unmarshal(resp.Output, &e) != nil ||
		e.Other != "unreached" || !strings.Contains(e.OtherWhy, "not connected") {
		t.Fatalf("d9 not connected: %+v %+v", resp, e)
	}
	got := make(chan wire.Escalation, 1)
	u9 := startUplink(t, &Uplink{Server: srv.URL, Daemon: "d9", Resend: time.Hour,
		Snapshot: func() core.FullState {
			return core.FullState{Daemon: "d9", Workstreams: []core.Workstream{{ID: "d9:far", Repo: "r", Branch: "feat-far", Files: []string{"far.go"}, Person: "Caro"}}}
		},
		OnWarnings: func([]core.Warning) {}, OnEscalation: func(e wire.Escalation) { got <- e }, Log: quiet})
	eventually(t, 3*time.Second, "d9's stream connected", u9.Connected)
	o.news("UserPromptSubmit", o.sA, o.wsA)
	if resp := o.d.handle(wire.Request{Type: wire.TypeEscalate, SessionID: "sA", Text: "which?"}); json.Unmarshal(resp.Output, &e) != nil || e.Other != "relayed" {
		t.Fatalf("d9 connected: %+v %+v", resp, e)
	}
	select {
	case r := <-got:
		if r.Reason != "which?" || r.From.Person != "Ana" || r.To.Person != "Caro" || r.Path != "far.go" {
			t.Fatalf("d9 got %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("d9 got no escalation within 3s")
	}
}

// TestTakeRelayed: an escalation another machine's agent asked for reaches
// this machine's human on the desktop and its sessions on that workstream;
// one for a workstream this machine does not have is dropped, and so is any
// while Dugout is paused.
func TestTakeRelayed(t *testing.T) {
	o := newEscOffice(t)
	r := wire.Escalation{Conflict: "c9", Repo: "r", Path: "far.go", Reason: "Caro keeps the old API, Ana removes it: which?",
		From: core.Side{Daemon: "d9", Workstream: "d9:far", Branch: "feat-far", Person: "Caro"},
		To:   core.Side{Daemon: o.d.id, Workstream: o.wsA, Branch: "feat-a", Person: "Ana"}}
	far := core.Conflict{ID: "c9", Repo: "r", Path: "far.go", Sides: [2]core.Side{r.To, r.From}}
	o.d.metrics.Sync(o.d.id, []core.Conflict{o.c, far}, nil, millis(o.now))
	wrong := r
	wrong.To.Workstream = "d1:nope"
	o.d.takeRelayed(wrong)
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("an escalation for a workstream this machine lacks was shown: %+v", n)
	}
	if err := os.WriteFile(statedir.PausedPath(o.d.cfg.StateDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o.d.takeRelayed(r)
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("paused, an escalation was shown: %+v", n)
	}
	os.Remove(statedir.PausedPath(o.d.cfg.StateDir))
	o.d.takeRelayed(r)
	ns := o.desk.all()
	if len(ns) != 1 || !strings.Contains(ns[0].Body, `Caro (branch "feat-far") asks about the conflict in "far.go" with Ana (branch "feat-a"): Caro keeps the old API`) {
		t.Fatalf("relayed: %+v", ns)
	}
	if ctx, _, _ := o.news("UserPromptSubmit", o.sA, o.wsA); !strings.Contains(ctx, "Caro keeps the old API") {
		t.Fatalf("feat-a's session heard %q", ctx)
	}
	if got := o.tally("c9"); got != (core.Tally{Relayed: 1}) {
		t.Fatalf("metrics after the relayed escalation: %+v, want one relayed (the paused and the misaddressed ones never count)", got)
	}
	if ctx, _, _ := o.news("UserPromptSubmit", o.sB, o.wsB); ctx != "" {
		t.Fatalf("feat-b's session heard another workstream's escalation: %q", ctx)
	}
}

// TestPausedDoesNotEscalate: while Dugout is paused, neither the rules nor an
// agent escalate.
func TestPausedDoesNotEscalate(t *testing.T) {
	o := newEscOffice(t)
	o.warn(o.wsA)
	o.warn(o.wsB)
	if err := os.WriteFile(statedir.PausedPath(o.d.cfg.StateDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o.now = o.now.Add(time.Hour)
	o.d.checkEscalations()
	if resp := o.d.handle(wire.Request{Type: wire.TypeEscalate, SessionID: "sA", Text: "r"}); !strings.Contains(resp.Error, "paused") {
		t.Fatalf("paused escalate = %+v", resp)
	}
	if n := o.desk.all(); len(n) != 0 {
		t.Fatalf("paused: %+v", n)
	}
	os.Remove(statedir.PausedPath(o.d.cfg.StateDir))
	o.d.checkEscalations()
	if n := o.desk.all(); len(n) != 1 {
		t.Fatalf("after the pause: %d notifications, want 1", len(n))
	}
}

// TestEscalationThroughHooks drives a real daemon: two worktrees of Ana's
// conflict, each session is handed the conflict at a hook, and once the
// delay passes the poll escalates it, the desktop shows it naming Ana, and
// each session's next hook carries it to the agent and to the human.
func TestEscalationThroughHooks(t *testing.T) {
	f, wt := overlap(t)
	testgit.Git(t, f.Clone, "config", "user.name", "Ana")
	desk := &shown{}
	// The detector reads the clock too: it runs on, ahead by skip.
	var mu sync.Mutex
	var skip time.Duration
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(skip) }
	r := startDaemonWith(t, "", func(d *Daemon) { d.cfg.Notify, d.clock = desk.notify, clock })
	sessions := []struct{ id, cwd string }{{"sA", f.Clone}, {"sB", wt}}
	for _, s := range sessions {
		hook(t, r.sock, "SessionStart", s.id, s.cwd, "", "")
	}
	for _, s := range sessions {
		eventually(t, 15*time.Second, s.id+" is handed the conflict", func() bool {
			return strings.Contains(hook(t, r.sock, "PostToolUse", s.id, s.cwd, "Edit", filepath.Join(s.cwd, "shared.go")), "- conflict ")
		})
	}
	if n := desk.all(); len(n) != 0 {
		t.Fatalf("before the delay: %+v", n)
	}
	mu.Lock()
	skip = repoconfig.DefaultEscalateAfter + time.Second
	mu.Unlock()
	eventually(t, 5*time.Second, "the poll escalates the conflict", func() bool { return len(desk.all()) == 1 })
	if body := desk.all()[0].Body; !strings.Contains(body, `Ana (branch "feat-a")`) || !strings.Contains(body, `Ana (branch "feat-b")`) || !strings.Contains(body, `"shared.go"`) {
		t.Fatalf("notification body %q", body)
	}
	for _, s := range sessions {
		var out struct {
			SystemMessage      string `json:"systemMessage"`
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(hook(t, r.sock, "UserPromptSubmit", s.id, s.cwd, "", "")), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "Dugout: that conflict went to both humans") ||
			!strings.Contains(out.SystemMessage, "Dugout escalation: Conflict in \"shared.go\"") {
			t.Fatalf("%s's UserPromptSubmit after the escalation = %+v", s.id, out)
		}
	}
	time.Sleep(500 * time.Millisecond) // a few more polls
	if n := len(desk.all()); n != 1 {
		t.Fatalf("the conflict escalated %d times, want once", n)
	}
}
