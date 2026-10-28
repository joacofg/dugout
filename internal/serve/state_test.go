package serve

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
)

func sampleState() Stored {
	return Stored{
		Version:   1,
		AdminHash: HashToken("admin"),
		Users:     []User{{Name: "ana", TokenHash: HashToken("a")}, {Name: "beto", TokenHash: HashToken("b"), Revoked: true}},
		Repos:     []string{"github.com/acme/api"},
		Paused:    true,
		Owners:    map[core.DaemonID]string{"d1": "ana"},
		Members:   map[string][]string{"github.com/acme/api": {"ana"}},
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	want := sampleState()
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestStateMissingIsNotExist(t *testing.T) {
	_, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestStateAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.json")
	old := sampleState()
	if err := SaveState(path, old); err != nil {
		t.Fatal(err)
	}
	// A crash between write and rename leaves a half-written temp file and
	// the old file untouched.
	if err := os.WriteFile(filepath.Join(dir, ".serve.json.tmp-123"), []byte(`{"version":1,"adm`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile = func(string, []byte) error { return errors.New("crash before rename") }
	defer func() { writeFile = defaultWriteFile }()
	next := old
	next.Paused = false
	if err := SaveState(path, next); err == nil {
		t.Fatal("want the write error")
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, old) {
		t.Fatalf("old file changed: %+v", got)
	}
}

func TestStateMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := SaveState(path, sampleState()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestStateBadVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("want a version error")
	}
}

func TestNewTokenFormat(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^dgt_[A-Za-z0-9_-]{43}$`).MatchString(tok) || len(tok) != 47 {
		t.Fatalf("token %q has the wrong shape", tok)
	}
	if hash != HashToken(tok) {
		t.Fatal("hash is not HashToken(token)")
	}
	tok2, hash2, _ := NewToken()
	if tok == tok2 || hash == hash2 {
		t.Fatal("two tokens collided")
	}
}

func TestHashTokenStable(t *testing.T) {
	// sha256("abc"), lowercase hex
	if got, want := HashToken("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("HashToken = %s, want %s", got, want)
	}
}

// TestStateEmptyListsAreArrays: a fresh state shows its users and repos as
// [] in the file, not null, for whoever reads it by hand.
func TestStateEmptyListsAreArrays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := SaveState(path, Stored{Version: 1, AdminHash: HashToken("admin")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"users": []`, `"repos": []`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("file lacks %s:\n%s", want, b)
		}
	}
}
