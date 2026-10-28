package serve

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// issue asks the admin API for a token for name and returns it.
func issue(t *testing.T, admin client, name string) string {
	t.Helper()
	code, b := admin.do(http.MethodPost, "/v1/admin/tokens", "application/json", []byte(`{"name":"`+name+`"}`))
	if code != http.StatusCreated {
		t.Fatalf("issue %s: status %d %s", name, code, b)
	}
	var got struct{ Name, Token string }
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != name || !strings.HasPrefix(got.Token, "dgt_") || len(got.Token) != 47 {
		t.Fatalf("issue %s: %s", name, b)
	}
	return got.Token
}

// TestAdminNeedsAdminToken: /v1/admin takes only the admin token, and the
// admin token opens nothing else.
func TestAdminNeedsAdminToken(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/admin/tokens", ""},
		{http.MethodPost, "/v1/admin/tokens", `{"name":"eve"}`},
		{http.MethodDelete, "/v1/admin/tokens/beto", ""},
		{http.MethodGet, "/v1/admin/repos", ""},
		{http.MethodPost, "/v1/admin/repos", `{"repo":"github.com/evil/x"}`},
		{http.MethodDelete, "/v1/admin/repos/r", ""},
		{http.MethodPost, "/v1/admin/repos/grant", `{"repo":"r","name":"beto"}`},
		{http.MethodPost, "/v1/admin/repos/ungrant", `{"repo":"r","name":"beto"}`},
		{http.MethodPost, "/v1/admin/pause", `{"paused":true}`},
	}
	for _, tok := range []string{"", anaTok, carlaTok, "dgt_nope"} {
		for _, r := range routes {
			ctype := ""
			if r.method == http.MethodPost {
				ctype = "application/json"
			}
			if code, _ := as(t, srv, tok).do(r.method, r.path, ctype, []byte(r.body)); code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: status %d, want 401", r.method, r.path, tok, code)
			}
		}
	}
	admin := as(t, srv, adminTok)
	if code := admin.fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusUnauthorized {
		t.Errorf("admin token on /v1/msg: status %d, want 401", code)
	}
	if code, _ := admin.do(http.MethodGet, "/v1/state", "", nil); code != http.StatusUnauthorized {
		t.Errorf("admin token on /v1/state: status %d, want 401", code)
	}
	if code, _ := admin.stream("d1"); code != http.StatusUnauthorized {
		t.Errorf("admin token on /v1/stream: status %d, want 401", code)
	}
	// Nothing above changed a thing.
	if code, b := admin.do(http.MethodGet, "/v1/admin/tokens", "", nil); code != http.StatusOK ||
		string(b) != `[{"name":"ana","revoked":false},{"name":"beto","revoked":false},{"name":"carla","revoked":true}]`+"\n" {
		t.Fatalf("tokens: status %d %s", code, b)
	}
}

func TestAdminTokens(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	admin := as(t, srv, adminTok)
	tok := issue(t, admin, "dani")
	if code := as(t, srv, tok).fullState("d5", 0, 0, w("d5:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("the issued token: status %d", code)
	}
	for body, want := range map[string]int{
		`{"name":""}`:    http.StatusBadRequest,
		`{"name":"  "}`:  http.StatusBadRequest,
		`{}`:             http.StatusBadRequest,
		`{not json`:      http.StatusBadRequest,
		`{"name":"ana"}`: http.StatusConflict,
		`{"name":"ANA"}`: http.StatusConflict, // lowercased first
	} {
		if code, b := admin.do(http.MethodPost, "/v1/admin/tokens", "application/json", []byte(body)); code != want {
			t.Errorf("issue %s: status %d %s, want %d", body, code, b, want)
		}
	}
	// A name goes in URLs and logs: a few safe characters, 32 at most.
	for _, name := range []string{"ana b", "a/b", "zoë", "e\tve", strings.Repeat("x", 33)} {
		body, _ := json.Marshal(map[string]string{"name": name})
		code, b := admin.do(http.MethodPost, "/v1/admin/tokens", "application/json", body)
		if code != http.StatusBadRequest || !strings.Contains(string(b), "invalid name: use lowercase letters, numbers, dot, hyphen or underscore (up to 32)") {
			t.Errorf("issue %q: status %d %q, want 400 invalid name", name, code, b)
		}
	}
	issue(t, admin, strings.Repeat("x", 32))
	code, b := admin.do(http.MethodPost, "/v1/admin/tokens", "application/json", []byte(`{"name":" Eva.Q-1_x "}`))
	if code != http.StatusCreated || !strings.Contains(string(b), `"name":"eva.q-1_x"`) {
		t.Fatalf("issue Eva.Q-1_x: status %d %s, want 201 as eva.q-1_x", code, b)
	}
	// carla is revoked: her name is free again, and her new token works.
	carla := issue(t, admin, "carla")
	if code := as(t, srv, carla).fullState("d3", 0, 0); code != http.StatusNoContent {
		t.Fatalf("carla's new token: status %d", code)
	}
	if code, _ := as(t, srv, carlaTok).do(http.MethodGet, "/v1/state", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("carla's old token: status %d, want 401", code)
	}

	if code, b := admin.do(http.MethodDelete, "/v1/admin/tokens/beto", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke beto: status %d %s", code, b)
	}
	for _, name := range []string{"beto", "nobody"} {
		if code, b := admin.do(http.MethodDelete, "/v1/admin/tokens/"+name, "", nil); code != http.StatusNotFound ||
			!strings.Contains(string(b), "there is no token for "+name) {
			t.Errorf("revoke %s again: status %d %q, want 404", name, code, b)
		}
	}
	code, b = admin.do(http.MethodGet, "/v1/admin/tokens", "", nil)
	var list []struct {
		Name    string `json:"name"`
		Revoked bool   `json:"revoked"`
	}
	if err := json.Unmarshal(b, &list); code != http.StatusOK || err != nil {
		t.Fatalf("list: status %d %s", code, b)
	}
	var got []string
	for _, u := range list {
		got = append(got, u.Name+map[bool]string{true: "-", false: "+"}[u.Revoked])
	}
	if strings.Join(got, " ") != "ana+ beto- carla- dani+ "+strings.Repeat("x", 32)+"+ eva.q-1_x+ carla+" {
		t.Fatalf("list = %v", got)
	}
	if strings.Contains(string(b), "hash") {
		t.Fatalf("list shows hashes: %s", b)
	}
}

func TestAdminRepos(t *testing.T) {
	_, srv, _ := stateServer(t)
	admin := as(t, srv, adminTok)
	list := func() string {
		t.Helper()
		code, b := admin.do(http.MethodGet, "/v1/admin/repos", "", nil)
		if code != http.StatusOK {
			t.Fatalf("list: status %d %s", code, b)
		}
		return strings.TrimSpace(string(b))
	}
	if got := list(); got != `[]` {
		t.Fatalf("empty list = %s", got)
	}
	for _, raw := range []string{"git@github.com:Acme/App.git", "https://github.com/acme/app", repoApp, "github.com/Acme/API"} {
		if code, b := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", []byte(`{"repo":"`+raw+`"}`)); code != http.StatusNoContent {
			t.Fatalf("add %s: status %d %s", raw, code, b)
		}
	}
	if got := list(); got != `[{"repo":"github.com/acme/api","members":[]},{"repo":"github.com/acme/app","members":[]}]` {
		t.Fatalf("list = %s", got)
	}
	if code, _ := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", []byte(`{"repo":" "}`)); code != http.StatusBadRequest {
		t.Fatalf("add an empty repo: status %d, want 400", code)
	}
	for _, raw := range []string{"/", "://", "git@:"} {
		code, b := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", []byte(`{"repo":"`+raw+`"}`))
		if code != http.StatusBadRequest || !strings.Contains(string(b), "invalid repo") {
			t.Errorf("add %q: status %d %q, want 400 invalid repo", raw, code, b)
		}
	}
	if code, b := admin.do(http.MethodDelete, "/v1/admin/repos/github.com/Acme/App.git", "", nil); code != http.StatusNoContent {
		t.Fatalf("remove: status %d %s", code, b)
	}
	if got := list(); got != `[{"repo":"github.com/acme/api","members":[]}]` {
		t.Fatalf("list after the removal = %s", got)
	}
	if code, b := admin.do(http.MethodDelete, "/v1/admin/repos?repo="+url.QueryEscape("git@github.com:Acme/API.git"), "", nil); code != http.StatusNoContent {
		t.Fatalf("remove by query: status %d %s", code, b)
	}
	if got := list(); got != `[]` {
		t.Fatalf("list after the query removal = %s", got)
	}
	for _, route := range []string{"/v1/admin/repos", "/v1/admin/repos?repo=", "/v1/admin/repos?repo=%2F"} {
		code, b := admin.do(http.MethodDelete, route, "", nil)
		if code != http.StatusBadRequest || !strings.Contains(string(b), "invalid repo") {
			t.Errorf("DELETE %s: status %d %q, want 400 invalid repo", route, code, b)
		}
	}
}

func TestAdminPause(t *testing.T) {
	s, srv, _ := stateServer(t)
	admin := as(t, srv, adminTok)
	paused := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stored.Paused
	}
	for _, c := range []struct {
		body string
		code int
		want bool
	}{
		{`{"paused":true}`, http.StatusNoContent, true},
		{`{}`, http.StatusBadRequest, true},
		{`{"paused":false}`, http.StatusNoContent, false},
	} {
		if code, b := admin.do(http.MethodPost, "/v1/admin/pause", "application/json", []byte(c.body)); code != c.code {
			t.Fatalf("pause %s: status %d %s, want %d", c.body, code, b, c.code)
		}
		if paused() != c.want {
			t.Fatalf("after %s: paused = %v", c.body, paused())
		}
	}
}

// TestAdminChangesPersist: every admin change is in the file before the
// response, so a new serve sees it.
func TestAdminChangesPersist(t *testing.T) {
	_, srv, path := stateServer(t, "r")
	admin := as(t, srv, adminTok)
	tok := issue(t, admin, "dani")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodDelete, "/v1/admin/tokens/beto", ""},
		{http.MethodPost, "/v1/admin/repos", `{"repo":"git@github.com:Acme/App.git"}`},
		{http.MethodDelete, "/v1/admin/repos/r", ""},
		{http.MethodPost, "/v1/admin/pause", `{"paused":true}`},
	} {
		ctype := ""
		if c.method == http.MethodPost {
			ctype = "application/json"
		}
		if code, b := admin.do(c.method, c.path, ctype, []byte(c.body)); code != http.StatusNoContent {
			t.Fatalf("%s %s: status %d %s", c.method, c.path, code, b)
		}
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Stored{Version: 1, AdminHash: HashToken(adminTok), Repos: []string{repoApp}, Paused: true, Users: []User{
		{Name: "ana", TokenHash: HashToken(anaTok)},
		{Name: "beto", TokenHash: HashToken(betoTok), Revoked: true},
		{Name: "carla", TokenHash: HashToken(carlaTok), Revoked: true},
		{Name: "dani", TokenHash: HashToken(tok)},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded state:\n got %+v\nwant %+v", got, want)
	}

	// A new serve on the reloaded file takes dani's token and not beto's.
	s2 := New()
	s2.UseState(path, got)
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(func() { srv2.CloseClientConnections(); srv2.Close() })
	if code, _ := as(t, srv2, tok).do(http.MethodGet, "/v1/state", "", nil); code != http.StatusOK {
		t.Fatalf("dani after a restart: status %d", code)
	}
	if code, _ := as(t, srv2, betoTok).do(http.MethodGet, "/v1/state", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("beto after a restart: status %d, want 401", code)
	}
}

// TestAdminSaveFailureChangesNothing: a change that cannot be saved is not
// made: it would be lost on the next restart.
func TestAdminSaveFailureChangesNothing(t *testing.T) {
	_, srv, path := stateServer(t, "r")
	admin := as(t, srv, adminTok)
	writeFile = func(string, []byte) error { return errors.New("disk full") }
	defer func() { writeFile = defaultWriteFile }()
	if code, _ := admin.do(http.MethodPost, "/v1/admin/tokens", "application/json", []byte(`{"name":"dani"}`)); code != http.StatusInternalServerError {
		t.Fatalf("issue: status %d, want 500", code)
	}
	if code, _ := admin.do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusInternalServerError {
		t.Fatalf("revoke: status %d, want 500", code)
	}
	writeFile = defaultWriteFile
	if code, _ := as(t, srv, anaTok).do(http.MethodGet, "/v1/state", "", nil); code != http.StatusOK {
		t.Fatalf("ana after a failed revoke: status %d", code)
	}
	if code, b := admin.do(http.MethodGet, "/v1/admin/tokens", "", nil); strings.Contains(string(b), "dani") {
		t.Fatalf("a failed issue was kept: status %d %s", code, b)
	}
	if st, err := LoadState(path); err != nil || len(st.Users) != 3 {
		t.Fatalf("file: %+v %v", st, err)
	}
}

// TestAdminTokenNeverLogged: an issued token goes to its response and
// nowhere else: not to a log, not to an error.
func TestAdminTokenNeverLogged(t *testing.T) {
	s, srv, _ := stateServer(t, "r")
	logs := &lockedBuf{}
	s.SetLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	admin := as(t, srv, adminTok)
	tok := issue(t, admin, "dani")
	dani := as(t, srv, tok)
	dani.fullState("d5", 0, 0, w("d5:ws", "x.go"))
	as(t, srv, betoTok).fullState("d5", 0, 0) // 403, d5 is dani's
	admin.do(http.MethodDelete, "/v1/admin/tokens/dani", "", nil)
	code, body := dani.do(http.MethodGet, "/v1/state", "", nil)
	if code != http.StatusUnauthorized || strings.Contains(string(body), tok) {
		t.Fatalf("revoked token: status %d body %q", code, body)
	}
	if !strings.Contains(logs.String(), "dani") {
		t.Fatalf("the log does not show the admin changes:\n%s", logs)
	}
	for _, secret := range []string{tok, adminTok, betoTok} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log has a token:\n%s", logs)
		}
	}
}
