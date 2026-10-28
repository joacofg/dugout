package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

func (c client) agentMessage(d, from, to string) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeAgentMessage, FullState: core.FullState{Daemon: core.DaemonID(d)},
		AgentMessage: &wire.AgentMessage{From: core.WorkstreamID(from), To: core.WorkstreamID(to)}})
}

func (c client) escalated(d, trigger, reason string, ids ...string) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeEscalated, FullState: core.FullState{Daemon: core.DaemonID(d)},
		Escalated: &wire.EscalationNotice{Conflicts: ids, Trigger: trigger, Reason: reason}})
}

func (c client) escalatedUrgent(d, reason string, ids ...string) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeEscalated, FullState: core.FullState{Daemon: core.DaemonID(d)},
		Escalated: &wire.EscalationNotice{Conflicts: ids, Trigger: wire.TriggerAgent, Reason: reason, Urgent: true}})
}

func (c client) escalatedFrom(d, from, reason string, ids ...string) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeEscalated, FullState: core.FullState{Daemon: core.DaemonID(d)},
		Escalated: &wire.EscalationNotice{Conflicts: ids, Trigger: wire.TriggerAgent, Reason: reason, From: core.WorkstreamID(from)}})
}

func (c client) conflictsWithStances(d string, st map[string][2]core.Stance, cs ...core.Conflict) int {
	c.t.Helper()
	return c.msg(wire.Message{Type: wire.TypeConflicts, FullState: core.FullState{Daemon: core.DaemonID(d)}, Conflicts: cs, Stances: st})
}

// conflictOf is a content conflict on path between workstreams a and b, of
// daemons da and db.
func conflictOf(id, repo, path, da, a, db, b string) core.Conflict {
	return core.Conflict{ID: id, Repo: repo, Path: path, Kind: "content", Sides: [2]core.Side{
		{Daemon: core.DaemonID(da), Workstream: core.WorkstreamID(a)}, {Daemon: core.DaemonID(db), Workstream: core.WorkstreamID(b)}},
		Lines: [2][]core.Range{{{Start: 3, Len: 2}}, {{Start: 4, Len: 1}}}}
}

// officeOf is the repo entry of v, failing the test when v has none.
func officeOf(t *testing.T, v wire.OfficeView, repo string) wire.OfficeRepo {
	t.Helper()
	for _, r := range v.Repos {
		if r.Repo == repo {
			return r
		}
	}
	t.Fatalf("no %s in the view: %+v", repo, v.Repos)
	return wire.OfficeRepo{}
}

func (s *Server) officeFor(user string) wire.OfficeView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.officeViewLocked(user)
}

// twoLocal is a state server where ana's d1 has d1:a and d1:b on repoApp's
// x.go, and beto's d2 has d2:c on it too.
func twoLocal(t *testing.T) (*Server, client, client) {
	t.Helper()
	s, srv, _ := stateServer(t, repoApp, repoAPI)
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	ana.fullState("d1", 0, 0, wr("d1:a", repoApp, "x.go"), wr("d1:b", repoApp, "x.go"), wr("d1:api", repoAPI, "y.go"))
	beto.fullState("d2", 0, 0, wr("d2:c", repoApp, "x.go"))
	return s, ana, beto
}

// TestStancesTakenFromOwnDaemonOnly: serve keeps the stances of a conflict
// whose two sides are the reporting daemon's, the only ones a registry can
// hold, and derives the owner from them; a conflict with another daemon's
// side keeps Dugout's rule whatever the report says.
func TestStancesTakenFromOwnDaemonOnly(t *testing.T) {
	s, ana, _ := twoLocal(t)
	local := conflictOf("c-local", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b")
	remote := conflictOf("c-remote", repoApp, "x.go", "d1", "d1:a", "d2", "d2:c")
	st := map[string][2]core.Stance{"c-local": {core.StanceNone, core.StanceClaim}, "c-remote": {core.StanceNone, core.StanceClaim}}
	if code := ana.conflictsWithStances("d1", st, local, remote); code != http.StatusNoContent {
		t.Fatalf("conflicts: status %d", code)
	}
	spans := map[string]wire.OfficeSpan{}
	for _, c := range officeOf(t, s.officeFor("ana"), repoApp).Conflicts {
		spans[c.ID] = c
	}
	if c := spans["c-local"]; c.Owner != "d1:b" || c.Why != core.OwnerClaimed {
		t.Errorf("local span = owner %s why %s, want d1:b claimed", c.Owner, c.Why)
	}
	if c := spans["c-remote"]; c.Owner != "d1:a" || c.Why != core.OwnerRule {
		t.Errorf("remote span = owner %s why %s, want d1:a by the rule", c.Owner, c.Why)
	}
}

// TestMessageNeedsOwnWorkstreams: a message goes on the record only between
// two workstreams of the sending daemon in one repo; serve stamps it with its
// own clock.
func TestMessageNeedsOwnWorkstreams(t *testing.T) {
	s, ana, _ := twoLocal(t)
	at := time.Date(2026, 10, 28, 14, 21, 0, 0, time.UTC)
	s.mu.Lock()
	s.clock = func() time.Time { return at }
	s.mu.Unlock()
	if code := ana.agentMessage("d1", "d1:a", "d1:b"); code != http.StatusNoContent {
		t.Fatalf("message between d1's workstreams: status %d", code)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"to another daemon's", "d1:a", "d2:c"},
		{"from another daemon's", "d2:c", "d1:a"},
		{"across repos", "d1:a", "d1:api"},
		{"to an unknown workstream", "d1:a", "d1:zzz"},
	} {
		if code := ana.agentMessage("d1", tc.from, tc.to); code != http.StatusUnprocessableEntity {
			t.Errorf("message %s: status %d, want 422", tc.name, code)
		}
	}
	got := officeOf(t, s.officeFor("ana"), repoApp).Messages
	if len(got) != 1 || got[0].From != "d1:a" || got[0].To != "d1:b" || !got[0].At.Equal(at) {
		t.Errorf("messages = %+v, want one d1:a to d1:b at %s", got, at)
	}
	if n := len(officeOf(t, s.officeFor("ana"), repoAPI).Messages); n != 0 {
		t.Errorf("repoAPI has %d messages, want 0", n)
	}
}

// TestMessagesBounded: a repo keeps its last 50 messages.
func TestMessagesBounded(t *testing.T) {
	s, ana, _ := twoLocal(t)
	n := 0
	s.mu.Lock()
	s.clock = func() time.Time { n++; return time.Unix(int64(n), 0) }
	s.mu.Unlock()
	for range 60 {
		ana.agentMessage("d1", "d1:a", "d1:b")
	}
	got := officeOf(t, s.officeFor("ana"), repoApp).Messages
	if len(got) != maxOfficeMessages || got[0].At.Unix() != 11 || got[len(got)-1].At.Unix() != 60 {
		t.Errorf("kept %d messages from %d to %d, want %d from 11 to 60", len(got), got[0].At.Unix(), got[len(got)-1].At.Unix(), maxOfficeMessages)
	}
}

// TestEscalatedNeedsReportedConflict: serve records an escalation only of
// conflicts the sending daemon reported, with the repo, paths and sides of
// its own copy, and the reason cut like an intent.
func TestEscalatedNeedsReportedConflict(t *testing.T) {
	s, ana, beto := twoLocal(t)
	ana.conflicts("d1", conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d2", "d2:c"))
	long := ""
	for range maxIntent + 20 {
		long += "é"
	}
	if code := ana.escalated("d1", wire.TriggerAgent, long, "c1"); code != http.StatusNoContent {
		t.Fatalf("escalation of a reported conflict: status %d", code)
	}
	if code := beto.escalated("d2", wire.TriggerAgent, "not mine", "c1"); code != http.StatusUnprocessableEntity {
		t.Errorf("escalation of a conflict d2 never reported: status %d, want 422", code)
	}
	if code := ana.escalated("d1", wire.TriggerAgent, "unknown", "c-none"); code != http.StatusUnprocessableEntity {
		t.Errorf("escalation of an unknown conflict: status %d, want 422", code)
	}
	if code := ana.escalated("d1", "bogus", "", "c1"); code != http.StatusUnprocessableEntity {
		t.Errorf("escalation with an unknown trigger: status %d, want 422", code)
	}
	got := officeOf(t, s.officeFor("ana"), repoApp).Escalations
	if len(got) != 1 {
		t.Fatalf("escalations = %+v, want one", got)
	}
	e := got[0]
	if e.Sides != [2]core.WorkstreamID{"d1:a", "d2:c"} || fmt.Sprint(e.Paths) != "[x.go]" || !e.Open ||
		e.Trigger != wire.TriggerAgent || len([]rune(e.Reason)) != maxIntent {
		t.Errorf("escalation = %+v, want sides d1:a,d2:c, paths [x.go], open, agent, a %d-rune reason", e, maxIntent)
	}
}

// TestTimeEscalationDedup: both daemons of a cross-machine pair run the time
// rule; serve keeps one record of it.
func TestTimeEscalationDedup(t *testing.T) {
	s, ana, beto := twoLocal(t)
	c := conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d2", "d2:c")
	ana.conflicts("d1", c)
	beto.conflicts("d2", c)
	for _, cl := range []struct {
		c client
		d string
	}{{ana, "d1"}, {beto, "d2"}} {
		if code := cl.c.escalated(cl.d, wire.TriggerTime, "", "c1"); code != http.StatusNoContent {
			t.Fatalf("%s's time escalation: status %d", cl.d, code)
		}
	}
	if got := officeOf(t, s.officeFor("ana"), repoApp).Escalations; len(got) != 1 {
		t.Errorf("escalations = %+v, want one", got)
	}
}

// TestRepeatEscalationUpdates: an agent's escalation re-sent as urgent or
// with a new reason updates its record in place.
func TestRepeatEscalationUpdates(t *testing.T) {
	s, ana, _ := twoLocal(t)
	ana.conflicts("d1", conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b"))
	ana.escalated("d1", wire.TriggerAgent, "first", "c1")
	ana.escalatedUrgent("d1", "first", "c1")
	got := officeOf(t, s.officeFor("ana"), repoApp).Escalations
	if len(got) != 1 || !got[0].Urgent || got[0].Reason != "first" {
		t.Fatalf("after an urgent repeat = %+v, want one urgent", got)
	}
	ana.escalated("d1", wire.TriggerAgent, "second", "c1")
	got = officeOf(t, s.officeFor("ana"), repoApp).Escalations
	if len(got) != 1 || got[0].Urgent || got[0].Reason != "second" {
		t.Errorf("after a repeat with a new reason = %+v, want one with reason second", got)
	}
}

// TestRepeatEscalationByOtherSide: the other side re-escalating the same
// conflict is named first, with its reason.
func TestRepeatEscalationByOtherSide(t *testing.T) {
	s, ana, _ := twoLocal(t)
	ana.conflicts("d1", conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b"))
	ana.escalatedFrom("d1", "d1:a", "from a", "c1")
	ana.escalatedFrom("d1", "d1:b", "from b", "c1")
	got := officeOf(t, s.officeFor("ana"), repoApp).Escalations
	if len(got) != 1 || got[0].Sides[0] != "d1:b" || got[0].Reason != "from b" {
		t.Errorf("escalations = %+v, want one asked first by d1:b with reason from b", got)
	}
}

// TestEscalationRecursAfterConflictEnded: a conflict that ended and came back
// and is escalated again makes a new record.
func TestEscalationRecursAfterConflictEnded(t *testing.T) {
	s, ana, _ := twoLocal(t)
	c := conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b")
	ana.conflicts("d1", c)
	ana.escalated("d1", wire.TriggerAgent, "first", "c1")
	ana.conflicts("d1")
	ana.conflicts("d1", c)
	ana.escalated("d1", wire.TriggerAgent, "again", "c1")
	got := officeOf(t, s.officeFor("ana"), repoApp).Escalations
	if len(got) != 1 || got[0].Reason != "again" || !got[0].Open {
		t.Errorf("escalations = %+v, want one open record of the recurrence", got)
	}
}

// TestEscalationOpenWhileConflictStands: an escalation stays listed after
// its conflict ends, no longer open.
func TestEscalationOpenWhileConflictStands(t *testing.T) {
	s, ana, _ := twoLocal(t)
	ana.conflicts("d1", conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b"))
	ana.escalated("d1", wire.TriggerDisputed, "", "c1")
	if e := officeOf(t, s.officeFor("ana"), repoApp).Escalations; len(e) != 1 || !e[0].Open {
		t.Fatalf("escalations = %+v, want one open", e)
	}
	ana.conflicts("d1")
	if e := officeOf(t, s.officeFor("ana"), repoApp).Escalations; len(e) != 1 || e[0].Open {
		t.Errorf("escalations after the conflict ended = %+v, want one closed", e)
	}
}

// viewToken mints a view token as c's person.
func (c client) viewToken() string {
	c.t.Helper()
	code, b := c.do(http.MethodPost, "/v1/office/token", "application/json", []byte("{}"))
	if code != http.StatusOK {
		c.t.Fatalf("POST /v1/office/token: status %d %s", code, b)
	}
	var vt wire.ViewToken
	if err := json.Unmarshal(b, &vt); err != nil || !strings.HasPrefix(vt.Token, wire.ViewTokenPrefix) {
		c.t.Fatalf("view token %s: %v", b, err)
	}
	return vt.Token
}

// office is GET /v1/office as c.
func (c client) office() (int, wire.OfficeView) {
	c.t.Helper()
	code, b := c.do(http.MethodGet, "/v1/office", "", nil)
	var v wire.OfficeView
	if code == http.StatusOK {
		if err := json.Unmarshal(b, &v); err != nil {
			c.t.Fatal(err)
		}
	}
	return code, v
}

// officeStream opens the office stream as c. With a 200 the channel gets
// each view and closes when the stream ends.
func (c client) officeStream() (int, <-chan wire.OfficeView) {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.srv.URL+"/v1/office/stream", nil)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	out := make(chan wire.OfficeView, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "office":
				var v wire.OfficeView
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err == nil {
					out <- v
				}
			}
		}
	}()
	return resp.StatusCode, out
}

// nextView waits for a view on ch that satisfies ok.
func nextView(t *testing.T, ch <-chan wire.OfficeView, what string, ok func(wire.OfficeView) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case v, open := <-ch:
			if !open {
				t.Fatalf("the stream ended before %s", what)
			}
			if ok(v) {
				return
			}
		case <-deadline:
			t.Fatalf("no view with %s", what)
		}
	}
}

// TestOfficeViewFiltersByMembers: a person sees nothing of a repo for
// members only they are not one of, in every block of the view.
func TestOfficeViewFiltersByMembers(t *testing.T) {
	_, ana, beto, dani, _, _ := membersWorld(t)
	ana.escalated("d1", wire.TriggerTime, "", "c-app")
	for _, c := range []struct {
		who  client
		name string
		sees bool
	}{{ana, "ana", true}, {dani, "dani", true}, {beto, "beto", false}} {
		code, v := as(t, c.who.srv, c.who.viewToken()).office()
		if code != http.StatusOK {
			t.Fatalf("%s's view: status %d", c.name, code)
		}
		has := slices.ContainsFunc(v.Repos, func(r wire.OfficeRepo) bool { return r.Repo == repoApp })
		if has != c.sees {
			t.Errorf("%s sees %s: %v, want %v (%+v)", c.name, repoApp, has, c.sees, v.Repos)
		}
		if !slices.ContainsFunc(v.Repos, func(r wire.OfficeRepo) bool { return r.Repo == repoAPI }) {
			t.Errorf("%s does not see %s, which is everyone's", c.name, repoAPI)
		}
		b, _ := json.Marshal(v)
		if !c.sees && (strings.Contains(string(b), "app") || strings.Contains(string(b), "c-app")) {
			t.Errorf("%s's view leaks %s: %s", c.name, repoApp, b)
		}
	}
}

// TestViewTokenOnlyReads: a view token reads the office and nothing else.
func TestViewTokenOnlyReads(t *testing.T) {
	_, ana, _ := twoLocal(t)
	view := as(t, ana.srv, ana.viewToken())
	if code, _ := view.office(); code != http.StatusOK {
		t.Fatalf("GET /v1/office with a view token: status %d", code)
	}
	for _, c := range []struct{ method, path, ctype, body string }{
		{http.MethodPost, "/v1/msg", "application/json", `{"type":"full_state","daemon_id":"d1","workstreams":[]}`},
		{http.MethodGet, "/v1/state", "", ""},
		{http.MethodGet, "/v1/stream?daemon_id=d1", "", ""},
		{http.MethodGet, "/v1/snapshot?ws=d1:a&daemon_id=d1", "", ""},
		{http.MethodPost, "/v1/office/token", "application/json", "{}"},
		{http.MethodGet, "/v1/admin/tokens", "", ""},
		{http.MethodGet, "/v1/office/../state", "", ""},
	} {
		if code, _ := view.do(c.method, c.path, c.ctype, []byte(c.body)); code != http.StatusUnauthorized {
			t.Errorf("%s %s with a view token: status %d, want 401", c.method, c.path, code)
		}
	}
	if n := len(ana.state().Workstreams); n != 4 {
		t.Errorf("the view token changed the state: %d workstreams, want 4", n)
	}
}

// TestOfficeRoutesGetOnly: nothing but GET reaches the view's routes.
func TestOfficeRoutesGetOnly(t *testing.T) {
	_, ana, _ := twoLocal(t)
	for _, path := range []string{"/v1/office", "/v1/office/stream"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			if code, _ := ana.do(m, path, "application/json", []byte("{}")); code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 405", m, path, code)
			}
		}
	}
}

// TestOfficeStreamPushes: the stream sends the view at once and again after
// a new intent, a message, a claim and an escalation.
func TestOfficeStreamPushes(t *testing.T) {
	_, ana, _ := twoLocal(t)
	code, ch := as(t, ana.srv, ana.viewToken()).officeStream()
	if code != http.StatusOK {
		t.Fatalf("office stream: status %d", code)
	}
	app := func(v wire.OfficeView) wire.OfficeRepo {
		for _, r := range v.Repos {
			if r.Repo == repoApp {
				return r
			}
		}
		return wire.NewOfficeRepo(repoApp)
	}
	nextView(t, ch, "the first view", func(v wire.OfficeView) bool { return len(app(v).Workstreams) == 3 })
	a := wr("d1:a", repoApp, "x.go")
	a.Intent = "Round invoice totals half-even"
	ana.fullState("d1", 0, 1, a, wr("d1:b", repoApp, "x.go"), wr("d1:api", repoAPI, "y.go"))
	nextView(t, ch, "the new intent", func(v wire.OfficeView) bool {
		return slices.ContainsFunc(app(v).Workstreams, func(w core.Workstream) bool { return w.Intent == a.Intent })
	})
	ana.agentMessage("d1", "d1:a", "d1:b")
	nextView(t, ch, "the message", func(v wire.OfficeView) bool { return len(app(v).Messages) == 1 })
	c := conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b")
	ana.conflictsWithStances("d1", map[string][2]core.Stance{"c1": {core.StanceNone, core.StanceClaim}}, c)
	nextView(t, ch, "the claim", func(v wire.OfficeView) bool {
		cs := app(v).Conflicts
		return len(cs) == 1 && cs[0].Owner == "d1:b" && cs[0].Why == core.OwnerClaimed
	})
	ana.escalated("d1", wire.TriggerAgent, "who owns the rounding", "c1")
	nextView(t, ch, "the escalation", func(v wire.OfficeView) bool { return len(app(v).Escalations) == 1 })
}

// TestOfficeStreamEndsOnRevoke: revoking a person ends their office stream
// and kills their view tokens.
func TestOfficeStreamEndsOnRevoke(t *testing.T) {
	_, ana, _ := twoLocal(t)
	view := as(t, ana.srv, ana.viewToken())
	code, ch := view.officeStream()
	if code != http.StatusOK {
		t.Fatalf("office stream: status %d", code)
	}
	nextView(t, ch, "the first view", func(wire.OfficeView) bool { return true })
	if code, b := as(t, ana.srv, adminTok).do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d %s", code, b)
	}
	deadline := time.After(3 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
		case <-deadline:
			t.Fatal("the stream outlived the revoke")
		}
	}
	if code, _ := view.office(); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/office with a revoked person's view token: status %d, want 401", code)
	}
}

// TestViewTokensBounded: a person holds at most maxViewTokens view tokens,
// the oldest going first, and each expires after viewTokenTTL.
func TestViewTokensBounded(t *testing.T) {
	s, ana, _ := twoLocal(t)
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	s.mu.Lock()
	s.clock = func() time.Time { return now }
	s.mu.Unlock()
	var toks []string
	for i := range maxViewTokens + 1 {
		now = now.Add(time.Second * time.Duration(i))
		toks = append(toks, ana.viewToken())
	}
	if code, _ := as(t, ana.srv, toks[0]).office(); code != http.StatusUnauthorized {
		t.Errorf("the oldest of %d view tokens: status %d, want 401", maxViewTokens+1, code)
	}
	if code, _ := as(t, ana.srv, toks[1]).office(); code != http.StatusOK {
		t.Errorf("the second view token: status %d, want 200", code)
	}
	s.mu.Lock()
	s.clock = func() time.Time { return now.Add(viewTokenTTL + time.Second) }
	s.mu.Unlock()
	if code, _ := as(t, ana.srv, toks[len(toks)-1]).office(); code != http.StatusUnauthorized {
		t.Errorf("an expired view token: status %d, want 401", code)
	}
}

// TestOfficeViewEmpty: with nothing in flight the view lists no repo, as [].
func TestOfficeViewEmpty(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	code, b := as(t, srv, anaTok).do(http.MethodGet, "/v1/office", "", nil)
	if code != http.StatusOK || !strings.Contains(string(b), `"repos":[]`) {
		t.Errorf("empty office: status %d %s, want 200 with \"repos\":[]", code, b)
	}
}

// TestOfficeViewKeepsBounds: the view carries the same bounded fields the
// daemons get, and leaves markup as data for the client to show as text.
func TestOfficeViewKeepsBounds(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	ana := as(t, srv, anaTok)
	long := wr("d1:a", repoApp, "x.go")
	long.Intent = strings.Repeat("a", 400)
	tagged := wr("d1:b", repoApp, "x.go")
	tagged.Intent = `<script>alert(1)</script>`
	ana.fullState("d1", 0, 0, long, tagged)
	_, v := ana.office()
	got := map[core.WorkstreamID]string{}
	for _, w := range officeOf(t, v, repoApp).Workstreams {
		got[w.ID] = w.Intent
	}
	if n := len([]rune(got["d1:a"])); n != maxIntent {
		t.Errorf("a 400-rune intent came back with %d runes, want %d", n, maxIntent)
	}
	if got["d1:b"] != tagged.Intent {
		t.Errorf("markup came back as %q, want it verbatim", got["d1:b"])
	}
}

// TestOfficeWithoutStateFile: without a state file and without a shared
// token, a loopback serve shows the office to anyone, as its other routes.
func TestOfficeWithoutStateFile(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	t.Cleanup(srv.Close)
	if code, _ := as(t, srv, "").office(); code != http.StatusOK {
		t.Errorf("GET /v1/office without a state file: status %d, want 200", code)
	}
	if tok := as(t, srv, "").viewToken(); tok == "" {
		t.Error("no view token without a state file")
	}
}

// TestAgentEscalationAskerFirst: an agent's escalation lists the asking
// workstream first, whatever the conflict's side order, so the view says who
// asked; an asker that is not a side of the conflict is refused.
func TestAgentEscalationAskerFirst(t *testing.T) {
	s, ana, _ := twoLocal(t)
	ana.conflicts("d1", conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b"))
	ask := func(from string) int {
		return ana.msg(wire.Message{Type: wire.TypeEscalated, FullState: core.FullState{Daemon: "d1"},
			Escalated: &wire.EscalationNotice{Conflicts: []string{"c1"}, Trigger: wire.TriggerAgent, From: core.WorkstreamID(from), Reason: "which?"}})
	}
	if code := ask("d2:c"); code != http.StatusUnprocessableEntity {
		t.Errorf("an asker outside the conflict: status %d, want 422", code)
	}
	if code := ask("d1:b"); code != http.StatusNoContent {
		t.Fatalf("d1:b asks: status %d", code)
	}
	e := officeOf(t, s.officeFor("ana"), repoApp).Escalations
	if len(e) != 1 || e[0].Sides != [2]core.WorkstreamID{"d1:b", "d1:a"} {
		t.Errorf("escalations = %+v, want the asker d1:b first", e)
	}
}

// TestOfficeDropsDisallowedRepo: a repo taken off the allowlist leaves the
// view whole, its messages and escalations too, as it leaves /v1/state.
func TestOfficeDropsDisallowedRepo(t *testing.T) {
	s, ana, _ := twoLocal(t)
	ana.agentMessage("d1", "d1:a", "d1:b")
	ana.conflicts("d1", conflictOf("c1", repoApp, "x.go", "d1", "d1:a", "d1", "d1:b"))
	ana.escalated("d1", wire.TriggerTime, "", "c1")
	officeOf(t, s.officeFor("ana"), repoApp)
	if code, b := as(t, ana.srv, adminTok).do(http.MethodDelete, "/v1/admin/repos/"+repoApp, "", nil); code != http.StatusNoContent {
		t.Fatalf("remove %s: status %d %s", repoApp, code, b)
	}
	for _, r := range s.officeFor("ana").Repos {
		if r.Repo == repoApp {
			t.Fatalf("%s is still in the view after leaving the allowlist: %+v", repoApp, r)
		}
	}
}

// TestOfficeViewFilesNeverNull: a workstream sent without files comes back
// with an empty list, so a client never meets a null.
func TestOfficeViewFilesNeverNull(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	ana := as(t, srv, anaTok)
	ana.fullState("d1", 0, 0, core.Workstream{ID: "d1:a", Repo: repoApp, Branch: "b", Intent: "no files yet"})
	code, b := ana.do(http.MethodGet, "/v1/office", "", nil)
	if code != http.StatusOK || strings.Contains(string(b), "null") || !strings.Contains(string(b), `"files":[]`) {
		t.Errorf("view: status %d %s, want files as [] and no null", code, b)
	}
}

// TestOfficeStreamEndsAtExpiry: an open stream ends once its view token
// expires or is evicted, so a token's 12 hours bound what it can watch.
func TestOfficeStreamEndsAtExpiry(t *testing.T) {
	s, ana, _ := twoLocal(t)
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	s.mu.Lock()
	s.clock = func() time.Time { return now }
	s.mu.Unlock()
	ended := func(ch <-chan wire.OfficeView) bool {
		deadline := time.After(3 * time.Second)
		for {
			select {
			case _, open := <-ch:
				if !open {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	code, ch := as(t, ana.srv, ana.viewToken()).officeStream()
	if code != http.StatusOK {
		t.Fatalf("office stream: status %d", code)
	}
	nextView(t, ch, "the first view", func(wire.OfficeView) bool { return true })
	s.mu.Lock()
	s.clock = func() time.Time { return now.Add(viewTokenTTL + time.Second) }
	s.mu.Unlock()
	ana.agentMessage("d1", "d1:a", "d1:b")
	if !ended(ch) {
		t.Error("the stream outlived its token's expiry")
	}

	// Each mint a second later, so the stream's token is the oldest but one.
	tick := now
	s.mu.Lock()
	s.clock = func() time.Time { tick = tick.Add(time.Second); return tick }
	s.mu.Unlock()
	code, ch = as(t, ana.srv, ana.viewToken()).officeStream()
	if code != http.StatusOK {
		t.Fatalf("second office stream: status %d", code)
	}
	nextView(t, ch, "the first view", func(wire.OfficeView) bool { return true })
	for range maxViewTokens {
		ana.viewToken()
	}
	if !ended(ch) {
		t.Error("the stream outlived its evicted token")
	}
}
