package serve

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// events subscribes as d with token tok and returns each event of the
// stream in order, as "control:<data>" or "warnings:<data>".
func (c client) events(d string) <-chan string {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.srv.URL+"/v1/stream?daemon_id="+d, nil)
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		c.t.Fatalf("stream: status %d", resp.StatusCode)
	}
	out := make(chan string, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- event + ":" + strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
		return ""
	}
}

func setPause(t *testing.T, c client, paused bool) {
	t.Helper()
	body := `{"paused":false}`
	if paused {
		body = `{"paused":true}`
	}
	if code, b := c.do(http.MethodPost, "/v1/admin/pause", "application/json", []byte(body)); code != http.StatusNoContent {
		t.Fatalf("pause %v: status %d %s", paused, code, b)
	}
}

// TestStreamSendsControlFirst: the stream opens with the control event, then
// the warnings; without a state file the pause is always off.
func TestStreamSendsControlFirst(t *testing.T) {
	srv := newServer(t)
	ch := client{t, srv, ""}.events("d1")
	if e := nextEvent(t, ch); e != `control:{"injection_off":false}` {
		t.Fatalf("first event = %s", e)
	}
	if e := nextEvent(t, ch); e != "warnings:[]" {
		t.Fatalf("second event = %s", e)
	}
}

// TestPauseOnReconnect: a daemon that was away while the team paused learns
// it on connect, before the first warnings it could hear.
func TestPauseOnReconnect(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	setPause(t, as(t, srv, adminTok), true)
	ch := as(t, srv, anaTok).events("d1")
	if e := nextEvent(t, ch); e != `control:{"injection_off":true}` {
		t.Fatalf("first event = %s, want the pause", e)
	}
	if e := nextEvent(t, ch); !strings.HasPrefix(e, "warnings:") {
		t.Fatalf("second event = %s", e)
	}
}

// TestPausePushedToOpenStreams: a change reaches every open stream, of every
// person; setting the value it has says nothing.
func TestPausePushedToOpenStreams(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	admin := as(t, srv, adminTok)
	a, b := as(t, srv, anaTok).events("da"), as(t, srv, betoTok).events("db")
	for _, ch := range []<-chan string{a, b} {
		nextEvent(t, ch) // control
		nextEvent(t, ch) // warnings
	}
	setPause(t, admin, true)
	setPause(t, admin, true) // no change, no event
	setPause(t, admin, false)
	for i, ch := range []<-chan string{a, b} {
		for _, want := range []string{`control:{"injection_off":true}`, `control:{"injection_off":false}`} {
			if e := nextEvent(t, ch); e != want {
				t.Fatalf("stream %d: event = %s, want %s", i, e, want)
			}
		}
	}
}
