package peers

import (
	"os"
	"path/filepath"
	"testing"
)

// entry is a registry file as Claude Code 2.1.311 writes it, trimmed.
const entry = `{"pid":7984,"sessionId":"s-1","cwd":"/w","startedAt":1793126736750,"version":"2.1.311","kind":"interactive",` +
	`"messagingSocketPath":"/tmp/cc-socks/7984.sock","name":"work-51","nameSource":"derived","status":"idle"}`

func registry(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, "sessions", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLookupByPID(t *testing.T) {
	dir := registry(t, map[string]string{"7984.json": entry})
	p, ok := Lookup(dir, "s-1", 7984)
	if !ok || p.Name != "work-51" || p.PID != 7984 || p.StartedAt != 1793126736750 {
		t.Fatalf("Lookup = %+v, %v", p, ok)
	}
}

// TestLookupByScan: a session the daemon registered without its PID, after a
// restart, is found by its id among every entry.
func TestLookupByScan(t *testing.T) {
	other := `{"pid":1,"sessionId":"s-0","name":"other-1"}`
	dir := registry(t, map[string]string{"1.json": other, "7984.json": entry, "7984.abc.key": "x", "junk.json": "{"})
	if p, ok := Lookup(dir, "s-1", 0); !ok || p.Name != "work-51" || p.PID != 7984 {
		t.Fatalf("Lookup = %+v, %v", p, ok)
	}
}

// TestLookupMismatch: an entry of another session (a reused PID, or a /clear
// that started a new session in the process), one without a name, a missing
// one or no dir at all name nobody.
func TestLookupMismatch(t *testing.T) {
	dir := registry(t, map[string]string{"7984.json": entry, "5.json": `{"pid":5,"sessionId":"s-5","name":" "}`})
	for _, tc := range []struct {
		dir, session string
		pid          int
	}{
		{dir, "s-2", 7984},
		{dir, "s-5", 5},
		{dir, "s-1", 1234},
		{dir, "s-9", 0},
		{"", "s-1", 7984},
		{dir, "", 7984},
		{filepath.Join(dir, "nope"), "s-1", 0},
	} {
		if p, ok := Lookup(tc.dir, tc.session, tc.pid); ok {
			t.Errorf("Lookup(%q, %q, %d) = %+v, want none", tc.dir, tc.session, tc.pid, p)
		}
	}
}

func TestDir(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if got := Dir(env("/c"), "/h"); got != "/c" {
		t.Fatalf("Dir with CLAUDE_CONFIG_DIR = %q", got)
	}
	if got := Dir(env(""), "/h"); got != filepath.Join("/h", ".claude") {
		t.Fatalf("Dir without it = %q", got)
	}
	if got := Dir(env(""), ""); got != "" {
		t.Fatalf("Dir with nothing = %q", got)
	}
}
