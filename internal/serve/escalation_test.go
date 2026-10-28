package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// escalations subscribes as d and returns a channel of the escalation events
// it gets.
func escalations(t *testing.T, srv *httptest.Server, d string) <-chan wire.Escalation {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/stream?daemon_id="+d, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: status %d", resp.StatusCode)
	}
	out := make(chan wire.Escalation, 16)
	ready := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "warnings":
				select {
				case <-ready:
				default:
					close(ready) // registered: serve pushed the first warning set
				}
			case strings.HasPrefix(line, "data: ") && event == "escalation":
				var e wire.Escalation
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err == nil {
					out <- e
				}
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("stream not registered within 2s")
	}
	return out
}

// postEscalation posts e as daemon d and returns the status and, on 200, the
// answer.
func postEscalation(t *testing.T, srv *httptest.Server, d core.DaemonID, e *wire.Escalation) (int, wire.Relayed) {
	t.Helper()
	b, err := json.Marshal(wire.Message{Type: wire.TypeEscalation, FullState: core.FullState{Daemon: d}, Escalation: e})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/v1/msg", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r wire.Relayed
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("answer %q: %v", body, err)
		}
	}
	return resp.StatusCode, r
}

// escPair puts d1:a and d2:b on x.go, Ana's and Bruno's, and returns an
// escalation from a to b.
func escPair(t *testing.T, srv *httptest.Server) *wire.Escalation {
	t.Helper()
	a, b := w("d1:a", "x.go"), w("d2:b", "x.go")
	a.Person, b.Person = "Ana", "Bruno"
	postState(t, srv, "d1", a)
	postState(t, srv, "d2", b)
	return &wire.Escalation{Conflict: "c1", Repo: "r", Path: "x.go",
		From:   core.Side{Daemon: "d1", Workstream: a.ID, Branch: a.Branch, Person: "Ana"},
		To:     core.Side{Daemon: "d2", Workstream: b.ID, Branch: b.Branch, Person: "Bruno"},
		Reason: "Ana keeps the 500 cap, Bruno raises it to 2000: which one stays?"}
}

// TestEscalationRelayed: serve hands an escalation to the other daemon's
// stream and says so, with both sides as its own state has them: a side the
// sender misnames is not passed on as such.
func TestEscalationRelayed(t *testing.T) {
	srv := newServer(t)
	e := escPair(t, srv)
	ch := escalations(t, srv, "d2")
	e.To.Person = "someone else"
	e.Reason = strings.Repeat("r", 1000)
	code, r := postEscalation(t, srv, "d1", e)
	if code != http.StatusOK || !r.Delivered {
		t.Fatalf("relay: status %d, %+v; want 200, delivered", code, r)
	}
	select {
	case got := <-ch:
		if got.Conflict != "c1" || got.Path != "x.go" || got.From.Person != "Ana" || got.To.Person != "Bruno" || got.From.Workstream != "d1:a" {
			t.Fatalf("relayed %+v", got)
		}
		if got.Reason != strings.Repeat("r", maxIntent) {
			t.Fatalf("relayed a reason of %d runes, want %d", len(got.Reason), maxIntent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no escalation event within 2s")
	}
}

// TestEscalationNotConnected: with no stream of the other daemon open,
// serve says it delivered nothing.
func TestEscalationNotConnected(t *testing.T) {
	srv := newServer(t)
	e := escPair(t, srv)
	if code, r := postEscalation(t, srv, "d1", e); code != http.StatusOK || r.Delivered {
		t.Fatalf("relay to nobody: status %d, %+v; want 200, not delivered", code, r)
	}
}

// TestEscalationRefused: serve relays only from the daemon of the asking
// workstream, to another daemon's, for a pair it holds a warning on that
// path for.
func TestEscalationRefused(t *testing.T) {
	srv := newServer(t)
	base := escPair(t, srv)
	ch := escalations(t, srv, "d2")
	cases := []struct {
		name   string
		sender core.DaemonID
		edit   func(e *wire.Escalation)
		want   int
	}{
		{"none", "d1", nil, http.StatusBadRequest},
		{"from another daemon's workstream", "d3", func(e *wire.Escalation) { e.From.Daemon = "d3" }, http.StatusUnprocessableEntity},
		{"from a workstream the sender does not have", "d1", func(e *wire.Escalation) { e.From.Workstream = "d1:zz" }, http.StatusUnprocessableEntity},
		{"to the sender itself", "d1", func(e *wire.Escalation) { e.To = e.From }, http.StatusUnprocessableEntity},
		{"on a path they do not share", "d1", func(e *wire.Escalation) { e.Path = "y.go" }, http.StatusUnprocessableEntity},
		{"in another repo", "d1", func(e *wire.Escalation) { e.Repo = "other" }, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		var e *wire.Escalation
		if tc.edit != nil {
			cp := *base
			e = &cp
			tc.edit(e)
		}
		if code, _ := postEscalation(t, srv, tc.sender, e); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
	select {
	case got := <-ch:
		t.Fatalf("a refused escalation reached d2: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}
