package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/snapshot"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

func TestNeedGC(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name         string
		last         time.Time
		loose, packs int
		want         bool
	}{
		{"never collected", time.Time{}, 0, 0, true},
		{"1 h ago, 10 loose, 1 pack", now.Add(-time.Hour), 10, 1, false},
		{"25 h ago", now.Add(-25 * time.Hour), 10, 1, true},
		{"1 h ago, 5001 loose", now.Add(-time.Hour), 5001, 1, true},
		{"1 h ago, 50 packs", now.Add(-time.Hour), 10, 50, false},
		{"1 h ago, 51 packs", now.Add(-time.Hour), 10, 51, true},
	} {
		if got := needGC(c.last, now, c.loose, c.packs); got != c.want {
			t.Errorf("%s: needGC = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFormatLines(t *testing.T) {
	for _, c := range []struct {
		in   []core.Range
		want string
	}{
		{nil, ""},
		{[]core.Range{{Start: 3, Len: 1}}, "3"},
		{[]core.Range{{Start: 3, Len: 3}}, "3-5"},
		{[]core.Range{{Start: 4, Len: 0}}, "4"},
		{[]core.Range{{Start: 1, Len: 1}, {Start: 7, Len: 2}}, "1,7-8"},
	} {
		if got := formatLines(c.in); got != c.want {
			t.Errorf("formatLines(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// newLogged creates a daemon that keeps every log record, without running it.
func newLogged(t *testing.T, server string) (*Daemon, *recorder, string) {
	t.Helper()
	rec := &recorder{}
	dir := shortDir(t)
	d, err := New(Config{StateDir: dir, Server: server, Poll: 200 * time.Millisecond, Resend: time.Second, Log: slog.New(rec)})
	if err != nil {
		t.Fatal(err)
	}
	return d, rec, dir
}

// runDaemon runs d until the test ends, and waits for it to stop.
func runDaemon(t *testing.T, d *Daemon, dir string) running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop within 5s")
		}
	})
	r := running{d: d, dir: dir, sock: statedir.Socket(dir)}
	eventually(t, 2*time.Second, "socket accepts", func() bool {
		c, err := net.Dial("unix", r.sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return r
}

func startLogged(t *testing.T, server string) (running, *recorder) {
	t.Helper()
	d, rec, dir := newLogged(t, server)
	return runDaemon(t, d, dir), rec
}

// countWith counts the records with msg whose attrs include want.
func countWith(rec *recorder, msg string, want map[string]string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
next:
	for i, m := range rec.msgs {
		if m != msg {
			continue
		}
		for k, v := range want {
			if rec.attrs[i][k] != v {
				continue next
			}
		}
		n++
	}
	return n
}

// waitRuns waits until the detector has finished n more runs.
func waitRuns(t *testing.T, rec *recorder, n int) {
	t.Helper()
	target := rec.count("detection done") + n
	eventually(t, 20*time.Second, fmt.Sprintf("%d more detector runs", n), func() bool {
		return rec.count("detection done") >= target
	})
}

func hasPath(ws []core.Warning, path string) bool {
	return slices.ContainsFunc(ws, func(w core.Warning) bool { return w.Path == path })
}

func repoOf(t *testing.T, dir string) string {
	t.Helper()
	scan, err := gitscan.Scan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return scan.Repo
}

// between is the warning serve would send about path between a and b.
func between(repo, path string, a, b core.Side) core.Warning {
	if b.Workstream < a.Workstream {
		a, b = b, a
	}
	return core.Warning{ID: core.WarningID(repo, path, a.Workstream, b.Workstream), Repo: repo, Path: path, Sides: [2]core.Side{a, b}}
}

// fakeServe accepts every post, has no snapshot to hand out and, on each
// stream, sends the warnings that sent returns once and stays open.
func fakeServe(t *testing.T, sent func() []core.Warning) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/snapshot" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/stream" {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		b, _ := json.Marshal(sent())
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: warnings\ndata: %s\n\n", b)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

// startConflicted starts a serverless daemon, opens sessions in the clone
// (feat-a) and wt (feat-b), and waits for the shared.go conflict of overlap().
// Each setup runs on the daemon before it starts.
func startConflicted(t *testing.T, f testgit.Fixture, wt string, setup ...func(*Daemon)) (running, *recorder) {
	t.Helper()
	d, rec, dir := newLogged(t, "")
	for _, s := range setup {
		s(d)
	}
	r := runDaemon(t, d, dir)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 15*time.Second, "conflict detected on shared.go", func() bool {
		_, ok := rec.find("conflict detected", map[string]string{"path": "shared.go"})
		return ok
	})
	return r, rec
}

// stubGit returns a dir holding a git script that runs body, shell, for
// subcommand sub, and the real git otherwise or when body falls through.
// Prepend the dir to PATH with t.Setenv.
func stubGit(t *testing.T, sub, body string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The subcommand is the first argument that is not a global option;
	// -c and -C take a value.
	script := "#!/bin/sh\n" +
		"sub=; skip=0\n" +
		"for a in \"$@\"; do\n" +
		"  if [ $skip = 1 ]; then skip=0; continue; fi\n" +
		"  case \"$a\" in -c|-C) skip=1 ;; -*) ;; *) sub=$a; break ;; esac\n" +
		"done\n" +
		"if [ \"$sub\" = " + sub + " ]; then\n" + body + "\nfi\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// countingGit returns a dir holding a git script that, for subcommand sub,
// appends a line to the calls file and exits 129, and runs the real git
// otherwise. Prepend the dir to PATH with t.Setenv.
func countingGit(t *testing.T, sub string) (dir, calls string) {
	t.Helper()
	calls = filepath.Join(t.TempDir(), "calls")
	return stubGit(t, sub, "echo call >> '"+calls+"'; exit 129"), calls
}

func callCount(t *testing.T, calls string) int {
	t.Helper()
	b, err := os.ReadFile(calls)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// atomicWrite replaces path with body in one rename, so no scan sees it half
// written.
func atomicWrite(t *testing.T, tmpDir, path, body string) {
	t.Helper()
	tmp := filepath.Join(tmpDir, "atomic-"+filepath.Base(path))
	testgit.WriteFile(t, tmp, body)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestDetectsLocalConflictWithoutServer(t *testing.T) {
	f, wt := overlap(t)
	r, rec := startLogged(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	want := map[string]string{"path": "shared.go", "kind": "content", "lines": "1"}
	eventually(t, 15*time.Second, "conflict detected on shared.go, line 1", func() bool {
		_, ok := rec.find("conflict detected", want)
		return ok
	})
	got, _ := rec.find("conflict detected", want)
	if branches := []string{got["branch_a"], got["branch_b"]}; !slices.Contains(branches, "feat-a") || !slices.Contains(branches, "feat-b") {
		t.Errorf("conflict detected branches = %v, want feat-a and feat-b", branches)
	}
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	if cs := r.d.conflictsFor(wsA); len(cs) != 1 || cs[0].ID != got["id"] || cs[0].Path != "shared.go" {
		t.Errorf("conflictsFor(wsA) = %+v, want the logged shared.go conflict %s", cs, got["id"])
	}
	if n := rec.count("warnings stream ended") + rec.count("post full_state"); n != 0 {
		t.Errorf("a daemon without a server ran the uplink (%d records)", n)
	}
	for _, msg := range []string{"snapshot failed", "check failed", "store gc failed", "stage 2 disabled"} {
		if got, ok := rec.find(msg, nil); ok {
			t.Errorf("%s: %v", msg, got)
		}
	}
}

// differentRegions leaves multi.txt, ten lines, edited at line 2 in the
// clone (branch feat-a) and at line 9 in a linked worktree on feat-b: a
// shared file that merges cleanly.
func differentRegions(t *testing.T) (testgit.Fixture, string) {
	t.Helper()
	f := testgit.NewFixture(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	testgit.WriteFile(t, filepath.Join(f.Seed, "multi.txt"), strings.Join(lines, "\n")+"\n")
	testgit.Git(t, f.Seed, "add", "multi.txt")
	testgit.Git(t, f.Seed, "commit", "-q", "-m", "multi")
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, f.Clone, "pull", "-q", "--ff-only")
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	wt := f.AddWorktree(t, "wtb", "feat-b")
	edit := func(dir string, n int) {
		changed := slices.Clone(lines)
		changed[n-1] = fmt.Sprintf("line %d, edited", n)
		testgit.WriteFile(t, filepath.Join(dir, "multi.txt"), strings.Join(changed, "\n")+"\n")
	}
	edit(f.Clone, 2)
	edit(wt, 9)
	return f, wt
}

func TestSameFileDifferentRegionsNoConflict(t *testing.T) {
	f, wt := differentRegions(t)
	r, rec := startLogged(t, "")
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	wsB := WorkstreamIDFor(r.d.ID(), wt, "feat-b")
	eventually(t, 15*time.Second, "overlap on multi.txt", func() bool { return hasPath(r.d.overlapsFor(wsA), "multi.txt") })
	eventually(t, 15*time.Second, "both sides snapshotted", func() bool {
		return countWith(rec, "snapshot", map[string]string{"ws": string(wsA)}) > 0 &&
			countWith(rec, "snapshot", map[string]string{"ws": string(wsB)}) > 0
	})
	waitRuns(t, rec, 3)
	if got, ok := rec.find("conflict detected", nil); ok {
		t.Fatalf("edits to different regions of multi.txt were reported as a conflict: %v", got)
	}
	if cs := r.d.conflictsFor(wsA); len(cs) != 0 {
		t.Fatalf("conflictsFor(wsA) = %+v, want none", cs)
	}
	// No conflict proves nothing unless the check ran and worked.
	for _, msg := range []string{"snapshot failed", "check failed", "stage 2 disabled"} {
		if got, ok := rec.find(msg, nil); ok {
			t.Errorf("%s: %v", msg, got)
		}
	}
	if !rec.any("detection done", func(a map[string]string) bool { return a["checks"] != "" && a["checks"] != "0" }) {
		t.Error("no detector run checked the pair")
	}
}

// TestLocalPairsIgnoreServe: serve's view of two workstreams of this
// daemon is older than the daemon's own core, so it never counts.
func TestLocalPairsIgnoreServe(t *testing.T) {
	f, wt := overlap(t)
	var mu sync.Mutex
	var sent []core.Warning
	srv := fakeServe(t, func() []core.Warning { mu.Lock(); defer mu.Unlock(); return sent })
	d, rec, dir := newLogged(t, srv.URL)
	wsA := WorkstreamIDFor(d.ID(), f.Clone, "feat-a")
	wsB := WorkstreamIDFor(d.ID(), wt, "feat-b")
	a := core.Side{Daemon: d.ID(), Workstream: wsA, Branch: "feat-a"}
	b := core.Side{Daemon: d.ID(), Workstream: wsB, Branch: "feat-b"}
	mu.Lock()
	sent = []core.Warning{between(repoOf(t, f.Clone), "ghost.go", a, b)}
	mu.Unlock()

	r := runDaemon(t, d, dir)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 10*time.Second, "serve's ghost.go warning received", func() bool {
		_, ok := rec.find("warning received", map[string]string{"path": "ghost.go"})
		return ok
	})
	eventually(t, 15*time.Second, "local overlap on shared.go", func() bool { return hasPath(r.d.overlapsFor(wsA), "shared.go") })
	waitRuns(t, rec, 3)
	if ws := r.d.overlapsFor(wsA); hasPath(ws, "ghost.go") {
		t.Fatalf("overlapsFor(wsA) took serve's local pair: %+v", ws)
	}
}

// TestUploadOnlyPairedPaths: the bundle that goes up for a remote pair holds
// the pair's paths and nothing else of the workstream: a teammate who pairs
// with shared.go does not get private.txt, untracked and in flight too.
func TestUploadOnlyPairedPaths(t *testing.T) {
	f, _ := overlap(t)
	testgit.WriteFile(t, filepath.Join(f.Clone, "private.txt"), "mine alone\n")
	var mu sync.Mutex
	var bundle []byte
	var commit string
	var warn []core.Warning // set once the daemon, and so its id, exists
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/snapshot":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bundle, commit = b, r.Header.Get("X-Dugout-Commit")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/stream":
			mu.Lock()
			b, _ := json.Marshal(warn)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: warnings\ndata: %s\n\n", b)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodGet:
			http.NotFound(w, r)
		default:
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(func() { closeServer(srv) })
	d, _, dir := newLogged(t, srv.URL)
	a := core.Side{Daemon: d.ID(), Workstream: WorkstreamIDFor(d.ID(), f.Clone, "feat-a"), Branch: "feat-a"}
	remote := core.Side{Daemon: "d2", Workstream: "d2:x", Branch: "x"}
	mu.Lock()
	warn = []core.Warning{between(repoOf(t, f.Clone), "shared.go", a, remote)}
	mu.Unlock()
	r := runDaemon(t, d, dir)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	eventually(t, 15*time.Second, "A's side uploaded", func() bool { mu.Lock(); defer mu.Unlock(); return bundle != nil })

	mu.Lock()
	file := filepath.Join(t.TempDir(), "got.bundle")
	if err := os.WriteFile(file, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	// The clone has the base the bundle needs.
	testgit.Git(t, f.Clone, "fetch", "-q", file, "refs/dugout/outgoing:refs/test/got")
	out := testgit.Git(t, f.Clone, "diff-tree", "-r", "--name-only", "refs/test/got^", "refs/test/got")
	if got := strings.Fields(out); !slices.Equal(got, []string{"shared.go"}) {
		t.Fatalf("uploaded snapshot changes %v, want only shared.go", got)
	}
	if head := strings.TrimSpace(testgit.Git(t, f.Clone, "rev-parse", "refs/test/got")); head != commit {
		t.Fatalf("bundle head %s, announced %s", head, commit)
	}
}

// TestRemoteOverlapFromServe: serve's warning that pairs a local workstream
// with one of another daemon's goes through the remote side: this daemon
// uploads its own snapshot, and, while serve has none of the other side's,
// the pair waits, unchecked and without a warning, as the local pair is
// checked.
func TestRemoteOverlapFromServe(t *testing.T) {
	f, wt := overlap(t)
	var mu sync.Mutex
	var sent []core.Warning
	srv := fakeServe(t, func() []core.Warning { mu.Lock(); defer mu.Unlock(); return sent })
	d, rec, dir := newLogged(t, srv.URL)
	wsA := WorkstreamIDFor(d.ID(), f.Clone, "feat-a")
	a := core.Side{Daemon: d.ID(), Workstream: wsA, Branch: "feat-a"}
	remote := core.Side{Daemon: "d2", Workstream: "d2:x", Branch: "x"}
	mu.Lock()
	sent = []core.Warning{between(repoOf(t, f.Clone), "remote.go", a, remote)}
	mu.Unlock()

	r := runDaemon(t, d, dir)
	hook(t, r.sock, "SessionStart", "sA", f.Clone, "", "")
	hook(t, r.sock, "SessionStart", "sB", wt, "", "")
	eventually(t, 10*time.Second, "remote overlap on remote.go", func() bool { return hasPath(r.d.overlapsFor(wsA), "remote.go") })
	eventually(t, 15*time.Second, "local conflict on shared.go", func() bool {
		_, ok := rec.find("conflict detected", map[string]string{"path": "shared.go"})
		return ok
	})
	eventually(t, 15*time.Second, "A's side of the remote pair uploaded", func() bool {
		_, ok := rec.find("snapshot uploaded", map[string]string{"ws": string(wsA)})
		return ok
	})
	waitRuns(t, rec, 3)
	if got, ok := rec.find("conflict detected", map[string]string{"path": "remote.go"}); ok {
		t.Fatalf("the remote pair was checked without the other side's snapshot: %v", got)
	}
	if w := warnsOf(rec); len(w) != 0 {
		t.Errorf("warned while serve has no snapshot of the other side: %v", w)
	}
	if !hasPath(r.d.overlapsFor(wsA), "remote.go") {
		t.Fatal("remote overlap on remote.go went away")
	}
}

// TestCheckErrorKeepsConflict: a check that fails keeps the pair's
// last result instead of resolving its conflicts.
func TestCheckErrorKeepsConflict(t *testing.T) {
	f, wt := overlap(t)
	stub := testgit.FailingMergeTree(t)
	r, rec := startConflicted(t, f, wt)
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")

	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	atomicWrite(t, f.Base, filepath.Join(wt, "shared.go"), "from b, again\n")
	eventually(t, 15*time.Second, "check failed", func() bool {
		_, ok := rec.find("check failed", nil)
		return ok
	})
	waitRuns(t, rec, 3)
	if n := rec.count("conflict resolved"); n != 0 {
		t.Fatalf("a failed check resolved the conflict (%d records)", n)
	}
	if n := rec.count("check failed"); n != 1 {
		t.Errorf("check failed logged %d times, want once per pair and message", n)
	}
	if cs := r.d.conflictsFor(wsA); len(cs) != 1 || cs[0].Path != "shared.go" {
		t.Fatalf("conflictsFor(wsA) = %+v, want the shared.go conflict", cs)
	}
}

// TestFailedCheckBacksOff: a pair whose check failed is not checked again
// while its snapshots stay the same, and is as soon as one of them changes.
func TestFailedCheckBacksOff(t *testing.T) {
	f, wt := overlap(t)
	stub, calls := countingGit(t, "merge-tree")
	_, rec := startConflicted(t, f, wt)

	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	atomicWrite(t, f.Base, filepath.Join(wt, "shared.go"), "from b, again\n")
	eventually(t, 15*time.Second, "check failed", func() bool {
		_, ok := rec.find("check failed", nil)
		return ok
	})
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("merge-tree ran %d times for one failed check, want 1", n)
	}
	waitRuns(t, rec, 3)
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("merge-tree ran %d times, want 1: the failed pair was checked again on the same snapshots", n)
	}
	atomicWrite(t, f.Base, filepath.Join(wt, "shared.go"), "from b, a third time\n")
	eventually(t, 15*time.Second, "a new snapshot of B checks the pair again", func() bool { return callCount(t, calls) == 2 })
}

// TestFailedTakeBacksOff: a workstream whose Take failed is not snapshotted
// again while its scan stays the same, and is as soon as the scan changes.
func TestFailedTakeBacksOff(t *testing.T) {
	f, wt := overlap(t)
	stub, calls := countingGit(t, "write-tree")
	_, rec := startConflicted(t, f, wt)

	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Unchanged worktrees are not snapshotted, so change both.
	testgit.WriteFile(t, filepath.Join(f.Clone, "other.txt"), "other\n")
	testgit.WriteFile(t, filepath.Join(wt, "other.txt"), "other\n")
	eventually(t, 15*time.Second, "both Takes failed", func() bool { return rec.count("snapshot failed") == 2 })
	if n := callCount(t, calls); n != 2 {
		t.Fatalf("write-tree ran %d times for two failed Takes, want 2", n)
	}
	waitRuns(t, rec, 3)
	if n := callCount(t, calls); n != 2 {
		t.Fatalf("write-tree ran %d times, want 2: a failed Take ran again on the same scan", n)
	}
	testgit.WriteFile(t, filepath.Join(wt, "new.txt"), "new\n")
	eventually(t, 15*time.Second, "a new scan of B takes it again", func() bool { return callCount(t, calls) == 3 })
}

// TestFailedCheckRetriesAfterBackoff: once the backoff is over, the same
// snapshots are checked again, and the unchanged error is not logged again.
func TestFailedCheckRetriesAfterBackoff(t *testing.T) {
	f, wt := overlap(t)
	stub, calls := countingGit(t, "merge-tree")
	_, rec := startConflicted(t, f, wt, func(d *Daemon) { d.checkRetry = 0 })

	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	atomicWrite(t, f.Base, filepath.Join(wt, "shared.go"), "from b, again\n")
	eventually(t, 15*time.Second, "check failed", func() bool {
		_, ok := rec.find("check failed", nil)
		return ok
	})
	n := callCount(t, calls)
	waitRuns(t, rec, 3)
	if got := callCount(t, calls); got <= n {
		t.Fatalf("merge-tree ran %d times after the backoff, want more than %d", got, n)
	}
	if got := rec.count("check failed"); got != 1 {
		t.Errorf("check failed logged %d times, want once per pair and message", got)
	}
}

// TestFailedGCBacksOff: a store whose gc failed does not run gc again on
// the next runs.
func TestFailedGCBacksOff(t *testing.T) {
	f, wt := overlap(t)
	stub, calls := countingGit(t, "gc")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, rec := startLogged(t, "")
	// Tracked, not through hooks: the stub's shell can outlast a hook's
	// 100 ms budget for finding the worktree.
	for _, dir := range []string{f.Clone, wt} {
		if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: dir}); resp.Error != "" {
			t.Fatalf("track %s: %s", dir, resp.Error)
		}
	}
	eventually(t, 15*time.Second, "store gc failed", func() bool {
		_, ok := rec.find("store gc failed", nil)
		return ok
	})
	waitRuns(t, rec, 3)
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("gc ran %d times, want 1: a failed gc was tried again right away", n)
	}
	if n := rec.count("store gc failed"); n != 1 {
		t.Errorf("store gc failed logged %d times, want once", n)
	}
}

// TestIndexPruneFailureKeepsGC: indexes that cannot be pruned do not fail
// the gc, which still counts as done; the prune error is logged once per
// message.
func TestIndexPruneFailureKeepsGC(t *testing.T) {
	d, rec, dir := newLogged(t, "")
	det := newDetector(d, dir)
	st, err := snapshot.Open(dir, "github.com/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(st.Dir, "index-blocked") // a dir with a file: os.Remove fails
	testgit.WriteFile(t, filepath.Join(blocker, "x"), "x\n")
	rs := &repoStore{store: st}
	det.repos["github.com/acme/app"] = rs
	ctx := context.Background()
	for n := range 2 {
		rs.lastGC = time.Time{} // due
		det.gc(ctx, nil)
		if rs.lastGC.IsZero() || !rs.gcFailed.IsZero() {
			t.Fatalf("gc %d: a failed index prune failed the gc", n+1)
		}
	}
	if got := rec.count("store gc"); got != 2 {
		t.Errorf("store gc logged %d times, want 2", got)
	}
	if got, ok := rec.find("store gc failed", nil); ok {
		t.Errorf("store gc failed: %v", got)
	}
	if got := rec.count("index prune failed"); got != 1 {
		t.Errorf("index prune failed logged %d times, want once per message", got)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	rs.lastGC = time.Time{}
	det.gc(ctx, nil)
	if rs.pruneErr != "" {
		t.Errorf("a prune that worked kept the error %q", rs.pruneErr)
	}
}

func TestStage2DisabledWhenProbeFails(t *testing.T) {
	f, wt := overlap(t)
	t.Setenv("PATH", testgit.FailingMergeTree(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, rec := startLogged(t, "")
	// Tracked, not through hooks: the stub's shell can outlast a hook's
	// 100 ms budget for finding the worktree.
	for _, dir := range []string{f.Clone, wt} {
		if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: dir}); resp.Error != "" {
			t.Fatalf("track %s: %s", dir, resp.Error)
		}
	}
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	eventually(t, 15*time.Second, "stage 2 disabled", func() bool {
		_, ok := rec.find("stage 2 disabled", map[string]string{"repo": repoOf(t, f.Clone)})
		return ok
	})
	eventually(t, 10*time.Second, "overlap on shared.go", func() bool { return hasPath(r.d.overlapsFor(wsA), "shared.go") })
	waitRuns(t, rec, 3)
	if got, ok := rec.find("conflict detected", nil); ok {
		t.Fatalf("conflict detected with stage 2 disabled: %v", got)
	}
	if n := rec.count("stage 2 disabled"); n != 1 {
		t.Errorf("stage 2 disabled logged %d times, want once", n)
	}
}

// TestProbeTimeoutRetried: a probe that times out says nothing about git,
// so it does not disable stage 2: the next run probes again.
func TestProbeTimeoutRetried(t *testing.T) {
	f, wt := overlap(t)
	slowed := filepath.Join(t.TempDir(), "slowed")
	stub := stubGit(t, "merge-tree", "if [ ! -e '"+slowed+"' ]; then : > '"+slowed+"'; exec sleep 10; fi")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	d, rec, dir := newLogged(t, "")
	d.probeTimeout = 300 * time.Millisecond
	r := runDaemon(t, d, dir)
	// Tracked, not through hooks: the stub's shell can outlast a hook's
	// 100 ms budget for finding the worktree.
	for _, dir := range []string{f.Clone, wt} {
		if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: dir}); resp.Error != "" {
			t.Fatalf("track %s: %s", dir, resp.Error)
		}
	}
	eventually(t, 15*time.Second, "conflict detected on shared.go", func() bool {
		_, ok := rec.find("conflict detected", map[string]string{"path": "shared.go"})
		return ok
	})
	if _, ok := rec.find("store probe failed", map[string]string{"repo": repoOf(t, f.Clone)}); !ok {
		t.Error("the first probe did not time out: the test proves nothing")
	}
	if got, ok := rec.find("stage 2 disabled", nil); ok {
		t.Fatalf("a timed-out probe disabled stage 2: %v", got)
	}
}

// TestDetectorLogsCost: for B, a run with pairs logs at Info what its Takes
// and checks cost, a new conflict its check's time, and a gc that ran its
// time.
func TestDetectorLogsCost(t *testing.T) {
	f, wt := overlap(t)
	_, rec := startConflicted(t, f, wt)
	if got, _ := rec.find("conflict detected", map[string]string{"path": "shared.go"}); got["check_ms"] == "" {
		t.Errorf("conflict detected without check_ms: %v", got)
	}
	eventually(t, 15*time.Second, "an Info store gc record with ms", func() bool {
		return rec.any("store gc", func(a map[string]string) bool { return a["level"] == "INFO" && a["ms"] != "" })
	})
	eventually(t, 15*time.Second, "an Info run summary with Takes and a check", func() bool {
		return rec.any("detection done", func(a map[string]string) bool {
			return a["level"] == "INFO" && a["takes"] == "2" && a["checks"] == "1" &&
				a["take_ms"] != "" && a["check_ms"] != "" && a["ms"] != ""
		})
	})
}

// TestInProgressKeepsSnapshot: a worktree half-way through a merge is
// not snapshotted; its last snapshot, and the conflict, stay.
func TestInProgressKeepsSnapshot(t *testing.T) {
	f, wt := overlap(t)
	// B commits its own c.txt, and branch other changes c.txt too, so
	// merging other into B conflicts there.
	testgit.WriteFile(t, filepath.Join(wt, "c.txt"), "from b\n")
	testgit.Git(t, wt, "commit", "-q", "-m", "c from b", "--", "c.txt")
	other := f.AddWorktree(t, "wto", "other")
	testgit.WriteFile(t, filepath.Join(other, "c.txt"), "from other\n")
	testgit.Git(t, other, "commit", "-q", "-am", "c from other")

	r, rec := startConflicted(t, f, wt)
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	wsB := WorkstreamIDFor(r.d.ID(), wt, "feat-b")
	snapsB := countWith(rec, "snapshot", map[string]string{"ws": string(wsB)})
	if snapsB == 0 {
		t.Fatal("no snapshot of B before the merge")
	}
	if out, err := exec.Command("git", "-C", wt, "merge", "-q", "--no-edit", "other").CombinedOutput(); err == nil {
		t.Fatalf("merging other into B did not conflict: %s", out)
	}
	eventually(t, 10*time.Second, "B's scan sees the merge in progress", func() bool {
		r.d.mu.Lock()
		defer r.d.mu.Unlock()
		w := r.d.worktrees[wt]
		return w != nil && w.snap != nil && w.snap.InProgress
	})
	waitRuns(t, rec, 3)
	if n := rec.count("conflict resolved"); n != 0 {
		t.Fatalf("the merge in progress resolved the conflict (%d records)", n)
	}
	if n := countWith(rec, "snapshot", map[string]string{"ws": string(wsB)}); n != snapsB {
		t.Fatalf("B was snapshotted mid-merge (%d snapshot records, had %d)", n, snapsB)
	}
	if cs := r.d.conflictsFor(wsA); len(cs) != 1 || cs[0].Path != "shared.go" {
		t.Fatalf("conflictsFor(wsA) = %+v, want the shared.go conflict", cs)
	}
}

// takesLogged sums the Takes of every "detection done" record.
func takesLogged(rec *recorder) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for i, m := range rec.msgs {
		if m == "detection done" {
			v, _ := strconv.Atoi(rec.attrs[i]["takes"])
			n += v
		}
	}
	return n
}

// TestTakeSkippedWhenUnchanged: once both sides of a stable pair are
// snapshotted, later runs do not Take them again.
func TestTakeSkippedWhenUnchanged(t *testing.T) {
	f, wt := overlap(t)
	_, rec := startConflicted(t, f, wt)
	waitRuns(t, rec, 2) // let the first runs settle
	before := takesLogged(rec)
	if before < 2 {
		t.Fatalf("%d Takes before the stable runs, want at least 2", before)
	}
	waitRuns(t, rec, 5)
	if got := takesLogged(rec); got != before {
		t.Fatalf("%d Takes over 5 runs of an unchanged pair, want none", got-before)
	}
}

// TestTakeAgainOnContentChange: a rewrite with the same size and other
// content is a new Take and a new commit.
func TestTakeAgainOnContentChange(t *testing.T) {
	f, wt := overlap(t)
	r, rec := startConflicted(t, f, wt)
	wsB := WorkstreamIDFor(r.d.ID(), wt, "feat-b")
	waitRuns(t, rec, 2)
	takes := takesLogged(rec)
	snaps := countWith(rec, "snapshot", map[string]string{"ws": string(wsB)})
	path := filepath.Join(wt, "shared.go")
	atomicWrite(t, f.Base, path, "from c\n") // "from b\n" is as long
	eventually(t, 15*time.Second, "a new commit of B", func() bool {
		return countWith(rec, "snapshot", map[string]string{"ws": string(wsB)}) > snaps
	})
	eventually(t, 15*time.Second, "the run that took B logs its Take", func() bool { return takesLogged(rec) > takes })
}

// TestDetectionDoneLevel: a run with no Take and no check logs at Debug, one
// with a Take or a check at Info, even a Take that wrote the same commit.
func TestDetectionDoneLevel(t *testing.T) {
	f, wt := overlap(t)
	_, rec := startConflicted(t, f, wt)
	waitRuns(t, rec, 3)
	// B gets a file too big for a snapshot: its Take writes the commit B
	// already has.
	takes := takesLogged(rec)
	testgit.WriteFile(t, filepath.Join(wt, "big.bin"), strings.Repeat("x", 1<<20+1))
	eventually(t, 15*time.Second, "B taken again", func() bool { return takesLogged(rec) > takes })
	if !rec.any("detection done", func(a map[string]string) bool {
		return a["level"] == "INFO" && a["takes"] != "0" && a["checks"] == "0"
	}) {
		t.Error("no Info record for the run whose Take wrote the same commit")
	}
	if !rec.any("detection done", func(a map[string]string) bool { return a["level"] == "INFO" && a["checks"] != "0" }) {
		t.Error("no Info record for the run that checked")
	}
	if !rec.any("detection done", func(a map[string]string) bool {
		return a["level"] == "DEBUG" && a["takes"] == "0" && a["checks"] == "0"
	}) {
		t.Error("no Debug record for an idle run")
	}
	if rec.any("detection done", func(a map[string]string) bool {
		return a["level"] == "INFO" && a["takes"] == "0" && a["checks"] == "0"
	}) {
		t.Error("an idle run logged at Info")
	}
	if rec.any("detection done", func(a map[string]string) bool {
		return a["level"] == "DEBUG" && (a["takes"] != "0" || a["checks"] != "0")
	}) {
		t.Error("a run that took or checked logged at Debug")
	}
}

// fingerprintRig is a detector over the clone of overlap, with its store
// open, to drive take by hand.
func fingerprintRig(t *testing.T) (det *detector, st *snapshot.Store, scan gitscan.Snapshot, path string) {
	t.Helper()
	f, _ := overlap(t)
	d, _, dir := newLogged(t, "")
	det = newDetector(d, dir)
	scan, err := gitscan.Scan(context.Background(), f.Clone)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(scan.AllFiles, "shared.go") {
		t.Fatalf("AllFiles = %v, want shared.go", scan.AllFiles)
	}
	st, err = snapshot.Open(dir, scan.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlternates([]string{filepath.Join(scan.CommonDir, "objects")}); err != nil {
		t.Fatal(err)
	}
	return det, st, scan, filepath.Join(scan.Root, "shared.go")
}

// TestTakeSkippedWhileWritingSameContent: rewriting a changed file with the
// same bytes (a new mtime, well within 2 s) is no new Take.
func TestTakeSkippedWhileWritingSameContent(t *testing.T) {
	det, st, scan, path := fingerprintRig(t)
	ctx := context.Background()
	ws := core.WorkstreamID("d1:ws")
	det.take(ctx, st, ws, scan)
	if det.cost.takes != 1 {
		t.Fatalf("%d Takes after the first, want 1", det.cost.takes)
	}
	for i := 0; i < 3; i++ {
		testgit.WriteFile(t, path, "from a\n")
		det.take(ctx, st, ws, scan)
	}
	if det.cost.takes != 1 {
		t.Fatalf("%d Takes in all after rewrites of the same content, want 1", det.cost.takes)
	}
}

// TestTakeAgainOnSameSizeSameMtime: other bytes of the same length, with the
// old mtime put back, are a new Take: the fingerprint reads content, not stat.
func TestTakeAgainOnSameSizeSameMtime(t *testing.T) {
	det, st, scan, path := fingerprintRig(t)
	ctx := context.Background()
	ws := core.WorkstreamID("d1:ws")
	det.take(ctx, st, ws, scan)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	testgit.WriteFile(t, path, "from c\n") // "from a\n" is as long
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	det.take(ctx, st, ws, scan)
	if det.cost.takes != 2 {
		t.Fatalf("%d Takes in all, want 2", det.cost.takes)
	}
	if det.snaps[ws].Commit == "" {
		t.Fatal("no snapshot after the Takes")
	}
}

// TestTakeChangedUnderItTakesAgain: a file written while a Take reads it may
// be in the snapshot either way, so that Take cannot vouch for the content
// its fingerprint read first. When the file goes back to that content, the
// next run takes it again instead of keeping the snapshot of what was in
// between, whose conflict would never clear.
func TestTakeChangedUnderItTakesAgain(t *testing.T) {
	det, st, scan, path := fingerprintRig(t)
	marker := filepath.Join(t.TempDir(), "written")
	git := stubGit(t, "add", "[ -e '"+marker+"' ] || { printf 'from z\\n' > '"+path+"'; touch '"+marker+"'; }")
	t.Setenv("PATH", git+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	ws := core.WorkstreamID("d1:ws")
	det.take(ctx, st, ws, scan)
	between := det.snaps[ws].Commit
	if between == "" {
		t.Fatal("no snapshot after the first Take")
	}
	testgit.WriteFile(t, path, "from a\n") // what the first Take's fingerprint read
	det.take(ctx, st, ws, scan)
	if det.cost.takes != 2 || det.snaps[ws].Commit == between {
		t.Fatalf("%d Takes in all, snapshot %s; want a second Take, off the content in between", det.cost.takes, short(det.snaps[ws].Commit))
	}
}

// TestFingerprintTooBigAlwaysTakes: past the hash budget a fingerprint is
// not stable, so every run takes.
func TestFingerprintTooBigAlwaysTakes(t *testing.T) {
	det, st, scan, _ := fingerprintRig(t)
	if _, stable := fingerprint(scan, det.hashBudget); !stable {
		t.Fatal("a fingerprint under the default budget is not stable")
	}
	if _, stable := fingerprint(scan, 1); stable {
		t.Fatal("a fingerprint over the budget is stable")
	}
	det.hashBudget = 1
	ctx := context.Background()
	ws := core.WorkstreamID("d1:ws")
	for n := 1; n <= 3; n++ {
		det.take(ctx, st, ws, scan)
		if det.cost.takes != n {
			t.Fatalf("run %d over the budget: %d Takes in all, want %d", n, det.cost.takes, n)
		}
	}
}

// TestFingerprintFIFO: a FIFO among the files is recorded by its mode, never
// opened: an open would block the detector until something wrote to it.
func TestFingerprintFIFO(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	testgit.WriteFile(t, filepath.Join(root, "a.go"), "package a\n")
	scan := gitscan.Snapshot{Root: root, AllFiles: []string{"a.go", "pipe"}}
	done := make(chan struct{})
	var key takeKey
	var stable bool
	go func() { key, stable = fingerprint(scan, hashBudget); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fingerprint blocked on the FIFO")
	}
	if !stable || !strings.HasSuffix(key.state, ";x:"+(os.ModeNamedPipe|0o644).String()+";") {
		t.Fatalf("fingerprint = %q, stable %v; want the FIFO by its mode, stable", key.state, stable)
	}
}

// TestMissingObjectsRetakesBothSides: a check that misses objects takes both
// sides again, even though neither worktree changed since its last Take, and
// the pair ends up checked.
func TestMissingObjectsRetakesBothSides(t *testing.T) {
	f, wt := overlap(t)
	r, rec := startConflicted(t, f, wt)
	waitRuns(t, rec, 2)

	// The first merge-tree after this fails as if the repo had dropped objects.
	marker := filepath.Join(t.TempDir(), "failed-once")
	stub := stubGit(t, "merge-tree", "if [ ! -e '"+marker+"' ]; then touch '"+marker+"'; echo 'fatal: missing blob' >&2; exit 128; fi")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	before := takesLogged(rec)
	// B changes: its Take, then the failed check, then both sides again.
	atomicWrite(t, f.Base, filepath.Join(wt, "shared.go"), "from c\n")
	eventually(t, 15*time.Second, "the failed check and its retry", func() bool {
		_, err := os.Stat(marker)
		return err == nil && takesLogged(rec) >= before+3
	})
	waitRuns(t, rec, 2)
	if got := takesLogged(rec) - before; got != 3 {
		t.Fatalf("%d Takes after B changed, want 3 (B, then both sides again)", got)
	}
	if n := rec.count("check failed"); n != 0 {
		t.Errorf("the retry failed too (%d check failed records)", n)
	}
	if _, ok := rec.find("conflict resolved", nil); ok {
		t.Error("the pair was not checked again: its conflict went away")
	}
	// The retried check is the one published: B's section is its new line.
	wsA := WorkstreamIDFor(r.d.ID(), f.Clone, "feat-a")
	cs := r.d.conflictsFor(wsA)
	if len(cs) != 1 {
		t.Fatalf("conflictsFor(wsA) = %+v, want the shared.go conflict", cs)
	}
	b := 1 - mineOf(cs[0].Sides, wsA)
	want := [2][]core.Snippet{}
	want[1-b], want[b] = []core.Snippet{{Lines: []string{"from a"}}}, []core.Snippet{{Lines: []string{"from c"}}}
	if !reflect.DeepEqual(cs[0].Text, want) {
		t.Fatalf("conflictsFor(wsA)[0].Text = %+v, want %+v", cs[0].Text, want)
	}
}
