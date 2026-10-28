package serve

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

// addRepo allows repo through the admin API, for members only if any.
func addRepo(t *testing.T, admin client, repo string, members ...string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"repo": repo, "members": members})
	if code, b := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", body); code != http.StatusNoContent {
		t.Fatalf("add %s %v: status %d %s", repo, members, code, b)
	}
}

// membership grants or ungrants name on repo and returns the status and body.
func membership(admin client, verb, repo, name string) (int, string) {
	body, _ := json.Marshal(map[string]string{"repo": repo, "name": name})
	code, b := admin.do(http.MethodPost, "/v1/admin/repos/"+verb, "application/json", body)
	return code, strings.TrimSpace(string(b))
}

// membersWorld has repoApp for ana and dani only, and repoAPI for everyone.
// ana (d1) and dani (d5) overlap on repoApp's x.go, and ana reported the
// conflict; beto (d2) touches x.go in repoApp too, and everyone y.go in
// repoAPI.
func membersWorld(t *testing.T) (s *Server, ana, beto, dani, admin client, path string) {
	t.Helper()
	s, srv, path := stateServer(t)
	admin = as(t, srv, adminTok)
	addRepo(t, admin, repoApp, "ana", "dani")
	addRepo(t, admin, repoAPI)
	ana, beto, dani = as(t, srv, anaTok), as(t, srv, betoTok), as(t, srv, issue(t, admin, "dani"))
	ana.fullState("d1", 0, 0, wr("d1:app", repoApp, "x.go"), wr("d1:api", repoAPI, "y.go"))
	dani.fullState("d5", 0, 0, wr("d5:app", repoApp, "x.go"))
	beto.fullState("d2", 0, 0, wr("d2:app", repoApp, "x.go"), wr("d2:api", repoAPI, "y.go"))
	if code := ana.upload("d1:app", "d1", []byte("ana's app")); code != http.StatusNoContent {
		t.Fatalf("ana's upload: status %d", code)
	}
	c := core.Conflict{ID: "c-app", Repo: repoApp, Path: "x.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d1", Workstream: "d1:app"}, {Daemon: "d5", Workstream: "d5:app"}}}
	if code := ana.conflicts("d1", c); code != http.StatusNoContent {
		t.Fatalf("ana's conflicts: status %d", code)
	}
	return s, ana, beto, dani, admin, path
}

func warningRepos(ws []core.Warning) string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Repo)
	}
	return strings.Join(out, ",")
}

// TestMembersOnlyRepoDropsOthers: in a repo with members, serve keeps only
// the members' workstreams, and a non-member's are dropped like a repo off
// the allowlist, warned about once.
func TestMembersOnlyRepoDropsOthers(t *testing.T) {
	s, ana, beto, _, _, _ := membersWorld(t)
	rec := &recorder{}
	s.SetLogger(slog.New(rec))
	// A second laptop of beto's, sending twice.
	for seq := range uint64(2) {
		beto.fullState("d4", 0, seq, wr("d4:app", repoApp, "x.go"))
	}
	d := ana.state()
	if wsIDs(d.Workstreams) != "d1:api,d1:app,d2:api,d5:app" {
		t.Fatalf("ana's state = %s, want beto's d2:app and d4:app dropped", wsIDs(d.Workstreams))
	}
	if warningRepos(d.Warnings) != repoAPI+","+repoApp || len(d.Conflicts) != 1 {
		t.Fatalf("ana's warnings = %+v, conflicts = %+v", d.Warnings, d.Conflicts)
	}
	for _, w := range d.Warnings {
		if w.Repo == repoApp && (w.Sides[0].Daemon == "d2" || w.Sides[1].Daemon == "d2") {
			t.Fatalf("a warning pairs beto in %s: %+v", repoApp, w)
		}
	}
	if n := rec.count("workstream in a repo for members only"); n != 1 {
		t.Fatalf("the drop was logged %d times, want once", n)
	}
}

// TestMembersOnlyRepoHiddenFromOthers: a non-member's /v1/state lists none
// of the repo's workstreams, warnings or conflicts, and their stream gets no
// warning there.
func TestMembersOnlyRepoHiddenFromOthers(t *testing.T) {
	_, _, beto, _, _, _ := membersWorld(t)
	d := beto.state()
	if wsIDs(d.Workstreams) != "d1:api,d2:api" {
		t.Fatalf("beto's state = %s, want only %s", wsIDs(d.Workstreams), repoAPI)
	}
	if warningRepos(d.Warnings) != repoAPI || len(d.Conflicts) != 0 {
		t.Fatalf("beto sees warnings %+v and conflicts %+v", d.Warnings, d.Conflicts)
	}
	_, ch := beto.stream("d2")
	if ws := next(t, ch); warningRepos(ws) != repoAPI {
		t.Fatalf("beto's stream = %+v, want only %s", ws, repoAPI)
	}
}

// TestMembersOnlyRepoNoBundle: a non-member gets no bundle of the repo, not
// even after resending a workstream that overlaps it.
func TestMembersOnlyRepoNoBundle(t *testing.T) {
	_, _, beto, dani, _, _ := membersWorld(t)
	beto.fullState("d2", 0, 1, wr("d2:app", repoApp, "x.go"), wr("d2:api", repoAPI, "y.go"))
	if code, b := beto.download("d1:app", "d2"); code != http.StatusNotFound {
		t.Fatalf("beto's download of ana's d1:app: status %d %q, want 404", code, b)
	}
	if code, b := dani.download("d1:app", "d5"); code != http.StatusOK || string(b) != "ana's app" {
		t.Fatalf("dani's download: status %d %q", code, b)
	}
}

// TestMembersOnlyRepoConflictFromOthers: a non-member's report of a conflict
// in the repo is not kept.
func TestMembersOnlyRepoConflictFromOthers(t *testing.T) {
	_, ana, beto, _, _, _ := membersWorld(t)
	c := core.Conflict{ID: "c-beto", Repo: repoApp, Path: "x.go", Kind: "content",
		Sides: [2]core.Side{{Daemon: "d2", Workstream: "d2:app"}, {Daemon: "d1", Workstream: "d1:app"}}}
	beto.conflicts("d2", c)
	if d := ana.state(); len(d.Conflicts) != 1 || d.Conflicts[0].ID != "c-app" {
		t.Fatalf("conflicts = %+v, want only ana's", d.Conflicts)
	}
}

// TestRepoWithoutMembersIsEveryones: a repo allowed with no members is every
// participant's, as before members existed.
func TestRepoWithoutMembersIsEveryones(t *testing.T) {
	_, ana, beto, _, _, _ := membersWorld(t)
	for name, c := range map[string]client{"ana": ana, "beto": beto} {
		d := c.state()
		n := 0
		for _, w := range d.Workstreams {
			if w.Repo == repoAPI {
				n++
			}
		}
		if n != 2 || !strings.Contains(warningRepos(d.Warnings), repoAPI) {
			t.Fatalf("%s's state = %+v, want both %s workstreams and their warning", name, d, repoAPI)
		}
	}
	if code := ana.upload("d1:api", "d1", []byte("ana's api")); code != http.StatusNoContent {
		t.Fatalf("ana's upload: status %d", code)
	}
	if code, b := beto.download("d1:api", "d2"); code != http.StatusOK || string(b) != "ana's api" {
		t.Fatalf("beto's download in an open repo: status %d %q", code, b)
	}
}

// TestGrantRefilters: granting a person shows their workstreams at once,
// without a resend, and ungranting drops them again.
func TestGrantRefilters(t *testing.T) {
	_, ana, beto, _, admin, _ := membersWorld(t)
	if code, b := membership(admin, "grant", repoApp, "beto"); code != http.StatusOK ||
		b != `{"repo":"github.com/acme/app","members":["ana","beto","dani"]}` {
		t.Fatalf("grant: status %d %s", code, b)
	}
	if d := beto.state(); wsIDs(d.Workstreams) != "d1:api,d1:app,d2:api,d2:app,d5:app" || len(d.Conflicts) != 1 {
		t.Fatalf("beto's state after the grant = %s %+v", wsIDs(d.Workstreams), d.Conflicts)
	}
	if code, b := beto.download("d1:app", "d2"); code != http.StatusOK || string(b) != "ana's app" {
		t.Fatalf("beto's download after the grant: status %d %q", code, b)
	}
	if code, b := membership(admin, "ungrant", repoApp, "beto"); code != http.StatusOK ||
		b != `{"repo":"github.com/acme/app","members":["ana","dani"]}` {
		t.Fatalf("ungrant: status %d %s", code, b)
	}
	if d := ana.state(); wsIDs(d.Workstreams) != "d1:api,d1:app,d2:api,d5:app" {
		t.Fatalf("ana's state after the ungrant = %s", wsIDs(d.Workstreams))
	}
	if code, _ := beto.download("d1:app", "d2"); code != http.StatusNotFound {
		t.Fatalf("beto's download after the ungrant: status %d, want 404", code)
	}
}

// TestRevokeKeepsMembership: a revoke leaves the person in the repos they
// belong to, so a new token for the same name gets them back, and nobody
// else's.
func TestRevokeKeepsMembership(t *testing.T) {
	_, _, _, _, admin, path := membersWorld(t)
	if code, b := admin.do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d %s", code, b)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{repoApp: {"ana", "dani"}}; !reflect.DeepEqual(st.Members, want) {
		t.Fatalf("members after the revoke = %v, want %v", st.Members, want)
	}
}

// TestAdminRepoMembers: the admin API's members: add with them, list them,
// grant and ungrant, the refusals, and a repo removed takes its members.
func TestAdminRepoMembers(t *testing.T) {
	_, srv, path := stateServer(t)
	admin := as(t, srv, adminTok)
	list := func() string {
		t.Helper()
		code, b := admin.do(http.MethodGet, "/v1/admin/repos", "", nil)
		if code != http.StatusOK {
			t.Fatalf("list: status %d %s", code, b)
		}
		return strings.TrimSpace(string(b))
	}
	addRepo(t, admin, "git@github.com:Acme/App.git", "Beto ", "ana")
	addRepo(t, admin, repoAPI)
	if got := list(); got != `[{"repo":"github.com/acme/api","members":[]},{"repo":"github.com/acme/app","members":["ana","beto"]}]` {
		t.Fatalf("list = %s", got)
	}
	// Adding again adds members, and never opens the repo.
	addRepo(t, admin, repoApp)
	addRepo(t, admin, repoApp, "dani")
	if got := list(); !strings.Contains(got, `{"repo":"github.com/acme/app","members":["ana","beto","dani"]}`) {
		t.Fatalf("list after adding again = %s", got)
	}
	for _, c := range []struct {
		verb, repo, name string
		code             int
		msg              string
	}{
		{"grant", repoApp, "a b", http.StatusBadRequest, "invalid name"},
		{"grant", repoApp, "", http.StatusBadRequest, "missing name"},
		{"grant", "/", "ana", http.StatusBadRequest, "invalid repo"},
		{"grant", "github.com/acme/nope", "ana", http.StatusNotFound, "github.com/acme/nope is not allowed"},
		{"ungrant", repoApp, "eva", http.StatusNotFound, "eva is not a member of github.com/acme/app"},
		{"ungrant", repoAPI, "ana", http.StatusNotFound, "ana is not a member of github.com/acme/api"},
	} {
		if code, b := membership(admin, c.verb, c.repo, c.name); code != c.code || !strings.Contains(b, c.msg) {
			t.Errorf("%s %s %q: status %d %q, want %d %q", c.verb, c.repo, c.name, code, b, c.code, c.msg)
		}
	}
	body, _ := json.Marshal(map[string]any{"repo": repoApp, "members": []string{"ok", "no/pe"}})
	if code, b := admin.do(http.MethodPost, "/v1/admin/repos", "application/json", body); code != http.StatusBadRequest || !strings.Contains(string(b), "invalid name") {
		t.Fatalf("add with an invalid member: status %d %s", code, b)
	}
	// A grant of a member already there changes nothing; a grant on an open
	// repo makes it the grantee's alone.
	if code, b := membership(admin, "grant", repoApp, "ANA"); code != http.StatusOK || !strings.Contains(b, `["ana","beto","dani"]`) {
		t.Fatalf("grant again: status %d %s", code, b)
	}
	if code, b := membership(admin, "grant", repoAPI, "ana"); code != http.StatusOK || b != `{"repo":"github.com/acme/api","members":["ana"]}` {
		t.Fatalf("grant on an open repo: status %d %s", code, b)
	}
	// Ungranting the last member would open the repo to everyone.
	if code, b := membership(admin, "ungrant", repoAPI, "ana"); code != http.StatusConflict || !strings.Contains(b, "is the only member") {
		t.Fatalf("ungrant of the last member: status %d %s", code, b)
	}
	if code, b := membership(admin, "ungrant", repoApp, "beto"); code != http.StatusOK || b != `{"repo":"github.com/acme/app","members":["ana","dani"]}` {
		t.Fatalf("ungrant: status %d %s", code, b)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{repoApp: {"ana", "dani"}, repoAPI: {"ana"}}; !reflect.DeepEqual(st.Members, want) {
		t.Fatalf("members in the file = %v, want %v", st.Members, want)
	}
	if code, b := admin.do(http.MethodDelete, "/v1/admin/repos/"+repoAPI, "", nil); code != http.StatusNoContent {
		t.Fatalf("remove: status %d %s", code, b)
	}
	addRepo(t, admin, repoAPI)
	if got := list(); !strings.Contains(got, `{"repo":"github.com/acme/api","members":[]}`) {
		t.Fatalf("a repo removed and added again kept its members: %s", got)
	}
}

// TestStateWithoutMembersLoads: a serve.json written before members existed
// loads, and its repos are everyone's.
func TestStateWithoutMembersLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	old := `{"version": 1, "admin_hash": "` + HashToken(adminTok) + `", "users": [` +
		`{"name": "ana", "token_hash": "` + HashToken(anaTok) + `", "revoked": false},` +
		`{"name": "beto", "token_hash": "` + HashToken(betoTok) + `", "revoked": false}], "repos": ["` + repoApp + `"], "paused": false}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := restart(t, path)
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	ana.fullState("d1", 0, 0, wr("d1:app", repoApp, "x.go"))
	beto.fullState("d2", 0, 0, wr("d2:app", repoApp, "x.go"))
	if d := beto.state(); wsIDs(d.Workstreams) != "d1:app,d2:app" || len(d.Warnings) != 1 {
		t.Fatalf("beto's state = %+v, want both workstreams paired", d)
	}
}
