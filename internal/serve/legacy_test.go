package serve

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// legacyRating is a rating as a daemon before ratings were dropped sends it.
const legacyRating = `{"type":"rating","daemon_id":"d1","protocol_version":2,"rating":{"id":"abc","kind":"overlap","verdict":"util","short":1}}`

// TestLegacyRatingIgnored: serve answers an older daemon's rating 204, so
// the daemon stops resending it, and keeps nothing of it; a ratings.jsonl an
// older serve left next to the state file is neither read nor written.
func TestLegacyRatingIgnored(t *testing.T) {
	_, srv, path := stateServer(t, "r")
	old := filepath.Join(filepath.Dir(path), "ratings.jsonl")
	if err := os.WriteFile(old, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := as(t, srv, anaTok).do(http.MethodPost, "/v1/msg", "application/json", []byte(legacyRating)); code != http.StatusNoContent {
		t.Fatalf("legacy rating: status %d %s", code, body)
	}
	if raw, err := os.ReadFile(old); err != nil || string(raw) != "not json\n" {
		t.Fatalf("ratings.jsonl = %q (%v), want it untouched", raw, err)
	}
	if c := as(t, srv, anaTok).fullState("d1", 0, 0, w("d1:ws", "x.go")); c != http.StatusNoContent {
		t.Fatalf("full state after a legacy rating: %d", c)
	}
}
