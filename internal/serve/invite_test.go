package serve

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// invite asks the admin API for an invite for name and returns its code.
func invite(t *testing.T, admin client, body string) string {
	t.Helper()
	code, b := admin.do(http.MethodPost, "/v1/admin/invites", "application/json", []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("invite %s: status %d %s", body, code, b)
	}
	var got struct {
		Name, Code string
		Expires    time.Time
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Code, "dgi_") || len(got.Code) != 47 || got.Expires.IsZero() {
		t.Fatalf("invite %s: %s", body, b)
	}
	return got.Code
}

// join redeems code with no token and returns the status and the answer.
func join(t *testing.T, c client, code string) (int, struct{ Name, Token string }) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"code": code})
	status, body := c.do(http.MethodPost, "/v1/join", "application/json", b)
	var got struct{ Name, Token string }
	if status == http.StatusCreated {
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
	}
	return status, got
}

// testClock is a clock a test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) get() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestInviteRedeemsOnce: an invite redeems, without any token, for its
// person's token, which then works like an issued one; the same code fails a
// second time; the state file keeps only hashes.
func TestInviteRedeemsOnce(t *testing.T) {
	s, srv, path := stateServer(t, repoApp)
	var logs strings.Builder
	s.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	code := invite(t, as(t, srv, adminTok), `{"name":"Dani"}`)
	status, got := join(t, as(t, srv, ""), code)
	if status != http.StatusCreated || got.Name != "dani" || !strings.HasPrefix(got.Token, "dgt_") || len(got.Token) != 47 {
		t.Fatalf("join: %d %+v", status, got)
	}
	if code, _ := as(t, srv, got.Token).do(http.MethodGet, "/v1/state", "", nil); code != http.StatusOK {
		t.Fatalf("the joined token on /v1/state: %d", code)
	}
	if status, _ := join(t, as(t, srv, ""), code); status != http.StatusUnauthorized {
		t.Fatalf("second join: %d, want 401", status)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Invites) != 0 {
		t.Fatalf("invites left after the join: %+v", st.Invites)
	}
	raw, _ := json.Marshal(st)
	for _, secret := range []string{code, got.Token} {
		if strings.Contains(string(raw), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("%s is in the state file or the log:\n%s\n%s", secret, raw, logs.String())
		}
	}
	if !strings.Contains(string(raw), HashToken(got.Token)) {
		t.Fatalf("no hash of dani's token in the state: %s", raw)
	}
}

// TestInviteExpires: a code works until its expiry and not after; one past
// it is pruned with the next change.
func TestInviteExpires(t *testing.T) {
	s, srv, path := stateServer(t)
	clk := &testClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	s.clock = clk.get
	admin := as(t, srv, adminTok)
	code := invite(t, admin, `{"name":"dani","ttl_seconds":3600}`)
	clk.add(time.Hour)
	if status, _ := join(t, as(t, srv, ""), code); status != http.StatusUnauthorized {
		t.Fatalf("join at expiry: %d, want 401", status)
	}
	invite(t, admin, `{"name":"eve"}`)
	st, _ := LoadState(path)
	if len(st.Invites) != 1 || st.Invites[0].Name != "eve" || !st.Invites[0].Expires.Equal(clk.get().Add(DefaultInviteTTL)) {
		t.Fatalf("invites = %+v, want only eve's, for a day", st.Invites)
	}
	code = invite(t, admin, `{"name":"fede","ttl_seconds":600}`)
	clk.add(9 * time.Minute)
	if status, _ := join(t, as(t, srv, ""), code); status != http.StatusCreated {
		t.Fatalf("join before expiry: %d", status)
	}
}

// TestInviteRefusals: the admin API's checks, and that only the admin token
// makes an invite.
func TestInviteRefusals(t *testing.T) {
	_, srv, _ := stateServer(t)
	admin := as(t, srv, adminTok)
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"name":"ana"}`, http.StatusConflict}, // already has a token
		{`{"name":""}`, http.StatusBadRequest},
		{`{"name":"no spaces"}`, http.StatusBadRequest},
		{`{"name":"dani","ttl_seconds":30}`, http.StatusBadRequest},
		{`{"name":"dani","ttl_seconds":700000}`, http.StatusBadRequest},
		{`{"name":"dani","ttl_seconds":-5}`, http.StatusBadRequest},
	} {
		if code, b := admin.do(http.MethodPost, "/v1/admin/invites", "application/json", []byte(c.body)); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.body, code, b, c.code)
		}
	}
	for _, tok := range []string{"", anaTok, carlaTok} {
		if code, _ := as(t, srv, tok).do(http.MethodPost, "/v1/admin/invites", "application/json", []byte(`{"name":"dani"}`)); code != http.StatusUnauthorized {
			t.Errorf("invite with %q: %d, want 401", tok, code)
		}
	}
	for _, code := range []string{"", "dgi_nope", anaTok} {
		if status, _ := join(t, as(t, srv, ""), code); status != http.StatusUnauthorized {
			t.Errorf("join with %q: %d, want 401", code, status)
		}
	}
	// The join route still wants JSON and a known Host, like every POST.
	if code, _ := as(t, srv, "").do(http.MethodPost, "/v1/join", "text/plain", []byte(`{"code":"x"}`)); code != http.StatusUnsupportedMediaType {
		t.Errorf("join as text/plain: %d", code)
	}
}

// TestInviteReplacedAndRevoked: a new invite for the same person replaces
// the pending one, revoke cancels it, and a join for someone who got a token
// meanwhile is refused and leaves the code.
func TestInviteReplacedAndRevoked(t *testing.T) {
	_, srv, _ := stateServer(t)
	admin, anon := as(t, srv, adminTok), as(t, srv, "")
	first := invite(t, admin, `{"name":"dani"}`)
	second := invite(t, admin, `{"name":"dani"}`)
	if status, _ := join(t, anon, first); status != http.StatusUnauthorized {
		t.Fatalf("the replaced code: %d, want 401", status)
	}
	if code, _ := admin.do(http.MethodDelete, "/v1/admin/tokens/dani", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke a pending invite: %d", code)
	}
	if status, _ := join(t, anon, second); status != http.StatusUnauthorized {
		t.Fatalf("the revoked code: %d, want 401", status)
	}
	code := invite(t, admin, `{"name":"eve"}`)
	issue(t, admin, "eve")
	if status, _ := join(t, anon, code); status != http.StatusConflict {
		t.Fatalf("join with a token issued meanwhile: %d, want 409", status)
	}
}

// TestJoinNeedsState: without a state file there are no persons to join as.
func TestJoinNeedsState(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	t.Cleanup(srv.Close)
	if status, _ := join(t, as(t, srv, ""), "dgi_x"); status != http.StatusNotFound {
		t.Fatalf("join without a state file: %d, want 404", status)
	}
}
