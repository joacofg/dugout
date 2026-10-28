package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/wire"
)

// captured is a serve that records every message posted to it before
// handling it.
type captured struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []wire.Message
}

func captureServe(t *testing.T) *captured {
	t.Helper()
	c := &captured{}
	s := serve.New()
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/msg" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			var m wire.Message
			if json.Unmarshal(b, &m) == nil {
				c.mu.Lock()
				c.got = append(c.got, m)
				c.mu.Unlock()
			}
		}
		s.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(c.srv) })
	return c
}

// of is every captured message of type typ.
func (c *captured) of(typ string) []wire.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []wire.Message
	for _, m := range c.got {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

// online points o's daemon at c and starts its uplink.
func (o office) online(t *testing.T, c *captured) {
	t.Helper()
	o.d.cfg.Server, o.d.uplink.Server = c.srv.URL, c.srv.URL
	startUplink(t, o.d.uplink)
}

// TestUplinkSendsStances: a conflicts message carries the stances of the
// conflicts that have one, by ID, and nothing for the others.
func TestUplinkSendsStances(t *testing.T) {
	c := captureServe(t)
	u := startUplink(t, &Uplink{Server: c.srv.URL, Daemon: "d1", Resend: time.Hour, Snapshot: fixedState,
		OnWarnings: func([]core.Warning) {}, Log: quiet})
	eventually(t, 3*time.Second, "full state arrived", func() bool { return len(warnings(t, c.srv).Workstreams) == 1 })
	claimed, plain := conflictFixture(), conflictFixture()
	claimed.Stances = [2]core.Stance{core.StanceNone, core.StanceYield}
	plain.ID = "c2"
	u.SetConflicts([]core.Conflict{claimed, plain})
	eventually(t, 3*time.Second, "conflicts posted", func() bool { return len(c.of(wire.TypeConflicts)) == 1 })
	m := c.of(wire.TypeConflicts)[0]
	if len(m.Stances) != 1 || m.Stances["c1"] != [2]core.Stance{core.StanceNone, core.StanceYield} {
		t.Fatalf("stances = %+v, want only c1's yield", m.Stances)
	}
}

// TestClaimResendsConflicts: a claim reaches serve on the conflicts message
// at once, without waiting for the next scan.
func TestClaimResendsConflicts(t *testing.T) {
	o := newEscOffice(t)
	c := captureServe(t)
	o.online(t, c)
	if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: "sA", Conflict: o.c.ID[:8]}); resp.Error != "" {
		t.Fatalf("claim: %s", resp.Error)
	}
	i := 0
	if o.c.Sides[1].Workstream == o.wsA {
		i = 1
	}
	eventually(t, 3*time.Second, "the claim on a conflicts message", func() bool {
		for _, m := range c.of(wire.TypeConflicts) {
			if m.Stances[o.c.ID][i] == core.StanceClaim {
				return true
			}
		}
		return false
	})
}

// TestMessagedNotifiesServe: a SendMessage to another workstream's session on
// this machine reaches serve as from and to, never its text; one to an
// address Dugout does not know sends nothing.
func TestMessagedNotifiesServe(t *testing.T) {
	o := newEscOffice(t)
	c := captureServe(t)
	o.online(t, c)
	o.d.messaged(o.wsA, "nobody-here")
	o.d.messaged(o.wsA, "b-2")
	eventually(t, 3*time.Second, "the message notice", func() bool { return len(c.of(wire.TypeAgentMessage)) > 0 })
	time.Sleep(200 * time.Millisecond)
	ms := c.of(wire.TypeAgentMessage)
	if len(ms) != 1 || ms[0].AgentMessage == nil || *ms[0].AgentMessage != (wire.AgentMessage{From: o.wsA, To: o.wsB}) || ms[0].Daemon != o.d.id {
		t.Fatalf("message notices = %+v, want one from %s to %s", ms, o.wsA, o.wsB)
	}
}

// TestEscalationsNotifyServe: every escalation this daemon raises reaches
// serve with its trigger: an agent's with its reason and urgency, the time
// rule's and a dispute's with none.
func TestEscalationsNotifyServe(t *testing.T) {
	notice := func(t *testing.T, c *captured) wire.EscalationNotice {
		t.Helper()
		eventually(t, 3*time.Second, "the escalation notice", func() bool { return len(c.of(wire.TypeEscalated)) > 0 })
		ms := c.of(wire.TypeEscalated)
		if len(ms) != 1 || ms[0].Escalated == nil {
			t.Fatalf("escalation notices = %+v, want one", ms)
		}
		return *ms[0].Escalated
	}
	t.Run("agent", func(t *testing.T) {
		o := newEscOffice(t)
		c := captureServe(t)
		o.online(t, c)
		if resp := o.d.handle(wire.Request{Type: wire.TypeEscalate, SessionID: "sA", Text: "which rounding?", Urgency: "urgent"}); resp.Error != "" {
			t.Fatalf("escalate: %s", resp.Error)
		}
		if n := notice(t, c); n.From != o.wsA || n.Trigger != wire.TriggerAgent || n.Reason != "which rounding?" || !n.Urgent || len(n.Conflicts) != 1 || n.Conflicts[0] != o.c.ID {
			t.Fatalf("notice = %+v", n)
		}
	})
	t.Run("time", func(t *testing.T) {
		o := newEscOffice(t)
		c := captureServe(t)
		o.online(t, c)
		o.warn(o.wsA)
		o.warn(o.wsB)
		o.now = o.now.Add(10 * time.Minute)
		o.d.checkEscalations()
		if n := notice(t, c); n.Trigger != wire.TriggerTime || n.Reason != "" || n.Conflicts[0] != o.c.ID {
			t.Fatalf("notice = %+v", n)
		}
	})
	t.Run("disputed", func(t *testing.T) {
		o := newEscOffice(t)
		c := captureServe(t)
		o.online(t, c)
		for _, s := range []string{"sA", "sB"} {
			if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: s, Conflict: o.c.ID[:8]}); resp.Error != "" {
				t.Fatalf("claim by %s: %s", s, resp.Error)
			}
		}
		o.d.checkEscalations()
		if n := notice(t, c); n.Trigger != wire.TriggerDisputed {
			t.Fatalf("notice = %+v", n)
		}
	})
}
