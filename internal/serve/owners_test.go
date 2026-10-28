package serve

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

// restart is a new serve on the state file at path, as after a restart.
func restart(t *testing.T, path string) *httptest.Server {
	t.Helper()
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	s.UseState(path, st)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

// TestDaemonIDBindingSurvivesRestart: a daemon_id stays bound to its person
// across a restart, so nobody can take it in the moment before its daemon
// reconnects.
func TestDaemonIDBindingSurvivesRestart(t *testing.T) {
	_, srv, path := stateServer(t, "r")
	if code := as(t, srv, anaTok).fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("ana's full_state: status %d", code)
	}
	srv2 := restart(t, path)
	beto := as(t, srv2, betoTok)
	if code, _ := beto.stream("d1"); code != http.StatusForbidden {
		t.Fatalf("beto's stream as d1 after a restart: status %d, want 403", code)
	}
	if code := beto.fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusForbidden {
		t.Fatalf("beto's full_state as d1 after a restart: status %d, want 403", code)
	}
	if code := as(t, srv2, anaTok).fullState("d1", 1, 0, w("d1:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("ana's full_state after a restart: status %d", code)
	}
}

// TestNewBindingWrittenOnce: a binding goes to the file once, on the
// daemon's first request, and not on the ones after.
func TestNewBindingWrittenOnce(t *testing.T) {
	_, srv, path := stateServer(t, "r")
	var writes atomic.Int32
	writeFile = func(p string, b []byte) error { writes.Add(1); return defaultWriteFile(p, b) }
	defer func() { writeFile = defaultWriteFile }()
	ana := as(t, srv, anaTok)
	for seq := range uint64(3) {
		ana.fullState("d1", 1, seq, w("d1:ws", "x.go"))
	}
	if code, _ := ana.stream("d1"); code != http.StatusOK {
		t.Fatalf("ana's stream: status %d", code)
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("%d writes, want 1", n)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Owners["d1"] != "ana" {
		t.Fatalf("owners in the file = %v", st.Owners)
	}
}

// TestBindingNotSavedNotTaken: a binding that cannot be saved is refused
// and not kept, since a restart would forget it.
func TestBindingNotSavedNotTaken(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	writeFile = func(string, []byte) error { return errors.New("disk full") }
	defer func() { writeFile = defaultWriteFile }()
	if code := as(t, srv, anaTok).fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusInternalServerError {
		t.Fatalf("ana's first full_state: status %d, want 500", code)
	}
	writeFile = defaultWriteFile
	if code := as(t, srv, betoTok).fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("d1 was kept for ana: status %d", code)
	}
}

// TestRevokeFreesDaemonIDs: a revoke frees the person's daemon_ids, in the
// file too, so they are not stuck on a person who cannot use them.
func TestRevokeFreesDaemonIDs(t *testing.T) {
	_, srv, path := stateServer(t, "r")
	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	ana.fullState("d1", 0, 0, w("d1:ws", "x.go"))
	beto.fullState("d2", 0, 0, w("d2:ws", "x.go"))
	if code, b := as(t, srv, adminTok).do(http.MethodDelete, "/v1/admin/tokens/ana", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d %s", code, b)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Owners["d1"]; ok || st.Owners["d2"] != "beto" {
		t.Fatalf("owners in the file after the revoke = %v", st.Owners)
	}
	srv2 := restart(t, path)
	if code := as(t, srv2, betoTok).fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("d1 after ana's revoke: status %d, want it free", code)
	}
}

// TestStateWithoutOwnersLoads: a serve.json written before the bindings were
// saved loads, and its first binding goes to the file.
func TestStateWithoutOwnersLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	old := `{"version": 1, "admin_hash": "` + HashToken(adminTok) + `", "users": [{"name": "ana", "token_hash": "` +
		HashToken(anaTok) + `", "revoked": false}], "repos": ["r"], "paused": false}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := restart(t, path)
	if code := as(t, srv, anaTok).fullState("d1", 0, 0, w("d1:ws", "x.go")); code != http.StatusNoContent {
		t.Fatalf("ana's full_state: status %d", code)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[core.DaemonID]string{"d1": "ana"}; !maps.Equal(st.Owners, want) {
		t.Fatalf("owners = %v, want %v", st.Owners, want)
	}
}
