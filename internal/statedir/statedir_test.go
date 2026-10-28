package statedir

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var wantUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// onPlatform makes Dir and SocketPath take platform p's paths for the test.
func onPlatform(t *testing.T, p string) {
	t.Helper()
	old := goos
	goos = p
	t.Cleanup(func() { goos = old })
}

func TestDirFromEnv(t *testing.T) {
	for _, tc := range []struct{ goos, suffix string }{
		{"darwin", filepath.Join("Library", "Application Support", "dugout")},
		{"linux", filepath.Join(".local", "state", "dugout")},
	} {
		onPlatform(t, tc.goos)
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("DUGOUT_HOME", "/x")
		if d, err := Dir(); err != nil || d != "/x" {
			t.Fatalf("%s: Dir() = %q, %v", tc.goos, d, err)
		}
		t.Setenv("DUGOUT_HOME", "")
		d, err := Dir()
		if err != nil || !strings.HasSuffix(d, tc.suffix) {
			t.Fatalf("%s: default Dir() = %q, %v", tc.goos, d, err)
		}
	}
}

// env is a getenv over a fixed map.
func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestDefaultDir: on Linux the state home is $XDG_STATE_HOME when it is
// absolute, else ~/.local/state; macOS ignores the XDG vars.
func TestDefaultDir(t *testing.T) {
	for _, tc := range []struct {
		goos, xdg, want string
	}{
		{"darwin", "", "/h/Library/Application Support/dugout"},
		{"darwin", "/xdg/state", "/h/Library/Application Support/dugout"},
		{"linux", "", "/h/.local/state/dugout"},
		{"linux", "/xdg/state", "/xdg/state/dugout"},
		{"linux", "relative/state", "/h/.local/state/dugout"},
	} {
		if got := DefaultDir(tc.goos, "/h", env(map[string]string{"XDG_STATE_HOME": tc.xdg})); got != tc.want {
			t.Errorf("DefaultDir(%s, XDG_STATE_HOME=%q) = %q, want %q", tc.goos, tc.xdg, got, tc.want)
		}
	}
}

func TestSocket(t *testing.T) {
	if got := Socket("/s"); got != "/s/dugout.sock" {
		t.Fatalf("Socket = %q", got)
	}
}

// TestSocketFor: on Linux the socket goes to $XDG_RUNTIME_DIR unless
// DUGOUT_HOME is set, the runtime dir is relative or the path would not fit
// in sun_path; macOS always keeps it in the state dir.
func TestSocketFor(t *testing.T) {
	long := "/run/" + strings.Repeat("u", MaxSocketPath-len("/run//dugout.sock")+1)
	fits := long[:len(long)-1]
	for _, tc := range []struct {
		name, goos, home, runtime, want string
	}{
		{"mac", "darwin", "", "/run/user/1000", "/st/dugout.sock"},
		{"linux runtime dir", "linux", "", "/run/user/1000", "/run/user/1000/dugout.sock"},
		{"linux without runtime dir", "linux", "", "", "/st/dugout.sock"},
		{"linux DUGOUT_HOME", "linux", "/st", "/run/user/1000", "/st/dugout.sock"},
		{"linux relative runtime dir", "linux", "", "run/user/1000", "/st/dugout.sock"},
		{"linux runtime dir just fits", "linux", "", fits, fits + "/dugout.sock"},
		{"linux runtime dir too long", "linux", "", long, "/st/dugout.sock"},
	} {
		got := SocketFor(tc.goos, "/st", env(map[string]string{EnvHome: tc.home, "XDG_RUNTIME_DIR": tc.runtime}))
		if got != tc.want {
			t.Errorf("%s: SocketFor = %q, want %q", tc.name, got, tc.want)
		}
		if len(got) > MaxSocketPath && tc.want != "/st/dugout.sock" {
			t.Errorf("%s: %d bytes, over %d", tc.name, len(got), MaxSocketPath)
		}
	}
}

// TestSocketPath: DUGOUT_HOME puts the socket inside it on every platform,
// which every test and script that sets it relies on.
func TestSocketPath(t *testing.T) {
	for _, p := range []string{"darwin", "linux"} {
		onPlatform(t, p)
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		t.Setenv(EnvHome, "/x")
		if got, err := SocketPath(); err != nil || got != "/x/dugout.sock" {
			t.Fatalf("%s with DUGOUT_HOME: SocketPath = %q, %v", p, got, err)
		}
	}
	t.Setenv(EnvHome, "")
	if got, err := SocketPath(); err != nil || got != "/run/user/1000/dugout.sock" {
		t.Fatalf("linux without DUGOUT_HOME: SocketPath = %q, %v", got, err)
	}
}

func TestDaemonIDPersists(t *testing.T) {
	dir := t.TempDir()
	id, created, err := LoadOrCreateID(dir)
	if err != nil || !created || !wantUUIDv4.MatchString(string(id)) {
		t.Fatalf("first call: id %q created %v err %v", id, created, err)
	}
	again, created, err := LoadOrCreateID(dir)
	if err != nil || created || again != id {
		t.Fatalf("second call: id %q created %v err %v, want %q", again, created, err, id)
	}
	st, err := os.Stat(filepath.Join(dir, "daemon_id"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("daemon_id file: %v %v", st, err)
	}
}

func TestDaemonIDCorruptRegenerates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "daemon_id"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, created, err := LoadOrCreateID(dir)
	if err != nil || !created || !wantUUIDv4.MatchString(string(id)) {
		t.Fatalf("corrupt file: id %q created %v err %v", id, created, err)
	}
}

func TestWorktreesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadWorktrees(dir); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
	if err := SaveWorktrees(dir, []string{"/b", "/a"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWorktrees(dir)
	if err != nil || !slices.Equal(got, []string{"/a", "/b"}) {
		t.Fatalf("LoadWorktrees = %v, %v", got, err)
	}
}

func TestEnsure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("Ensure: %v %v", st, err)
	}
}

// epochClock is a fixed wall clock for NextEpoch; its floor is its time in
// milliseconds.
var epochClock = time.UnixMilli(1_700_000_000_000)

const epochFloor = uint64(1_700_000_000_000)

func nextEpoch(t *testing.T, dir string, now time.Time) uint64 {
	t.Helper()
	e, reset, err := NextEpoch(dir, now)
	if err != nil || reset {
		t.Fatalf("NextEpoch = %d, reset %v, %v", e, reset, err)
	}
	return e
}

// TestNextEpochGrows: with the clock still, each run's epoch is one more
// than the last, starting at the clock's floor.
func TestNextEpochGrows(t *testing.T) {
	dir := t.TempDir()
	for i := range uint64(3) {
		if got := nextEpoch(t, dir, epochClock); got != epochFloor+i {
			t.Fatalf("run %d: epoch %d, want %d", i, got, epochFloor+i)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "epoch")); err != nil || string(b) != "1700000000002\n" {
		t.Fatalf("epoch file = %q, %v", b, err)
	}
}

// TestNextEpochLostFileStartsFromClock: with its file lost, a daemon's next
// epoch still comes above every earlier one, from a clock that moved on.
func TestNextEpochLostFileStartsFromClock(t *testing.T) {
	dir := t.TempDir()
	nextEpoch(t, dir, epochClock)
	last := nextEpoch(t, dir, epochClock)
	if err := os.Remove(filepath.Join(dir, "epoch")); err != nil {
		t.Fatal(err)
	}
	if got := nextEpoch(t, dir, epochClock.Add(time.Second)); got <= last {
		t.Fatalf("after the file was lost: epoch %d, want above %d", got, last)
	}
}

// TestNextEpochClockBehind: a clock behind the stored epoch never takes it
// back: the epoch grows by one.
func TestNextEpochClockBehind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "epoch"), []byte("1700000000500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := nextEpoch(t, dir, epochClock); got != 1_700_000_000_501 {
		t.Fatalf("epoch %d, want 1700000000501", got)
	}
}

// TestNextEpochNegativeClock: a clock before 1970 gives no floor, and the
// epoch still starts at 1, never 0.
func TestNextEpochNegativeClock(t *testing.T) {
	dir := t.TempDir()
	before := time.UnixMilli(-5)
	if a, b := nextEpoch(t, dir, before), nextEpoch(t, dir, before); a != 1 || b != 2 {
		t.Fatalf("epochs %d, %d; want 1, 2", a, b)
	}
}

// TestNextEpochWritesAtomically: the new epoch replaces the file whole, by
// rename, with mode 0600 and nothing left beside it; a write that cannot
// happen leaves the old epoch and fails.
func TestNextEpochWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epoch")
	if err := os.WriteFile(path, []byte("41\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	zero := time.UnixMilli(0)
	if got := nextEpoch(t, dir, zero); got != 42 {
		t.Fatalf("NextEpoch = %d, want 42", got)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		t.Fatalf("epoch file %v (%v): want a new file, by rename, with mode 0600", after, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("state dir holds %v, want only epoch", entries)
	}

	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only dir")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if got, _, err := NextEpoch(dir, zero); err == nil {
		t.Fatalf("NextEpoch in a read-only dir = %d, want an error", got)
	}
	if b, _ := os.ReadFile(path); string(b) != "42\n" {
		t.Fatalf("a failed write left epoch = %q, want \"42\\n\"", b)
	}
}

// TestNextEpochCorruptStartsFromClock: an epoch file that is not a number
// does not keep the daemon from starting: NextEpoch reports it and starts
// from the clock's floor.
func TestNextEpochCorruptStartsFromClock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epoch")
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, reset, err := NextEpoch(dir, epochClock)
	if err != nil || !reset || got != epochFloor {
		t.Fatalf("NextEpoch = %d, reset %v, %v; want %d, reset", got, reset, err, epochFloor)
	}
	if again := nextEpoch(t, dir, epochClock); again != epochFloor+1 {
		t.Fatalf("next run: epoch %d, want %d", again, epochFloor+1)
	}
}

func TestWriteAtomicKeepsOldFileWhenRenameFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := WriteAtomic(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	rename = func(string, string) error { return os.ErrPermission }
	defer func() { rename = os.Rename }()
	if err := WriteAtomic(path, []byte("new")); err == nil {
		t.Fatal("want the rename error")
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("file = %q, want old", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".f.json.tmp-*")); len(left) != 0 {
		t.Fatalf("temp left behind: %v", left)
	}
}

func TestModesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if m, err := LoadModes(dir); err != nil || len(m) != 0 {
		t.Fatalf("no modes.json = %v, %v", m, err)
	}
	want := map[string]string{"github.com/acme/app": "supervised", "github.com/acme/lib": "auto"}
	if err := SaveModes(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadModes(dir)
	if err != nil || len(got) != 2 || got["github.com/acme/app"] != "supervised" || got["github.com/acme/lib"] != "auto" {
		t.Fatalf("LoadModes = %v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modes.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModes(dir); err == nil {
		t.Fatal("a broken modes.json read fine")
	}
}
