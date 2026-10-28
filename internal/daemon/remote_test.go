package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/snapshot"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

// multiLines is multi.txt as origin has it: ten numbered lines.
func multiLines() []string {
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	return lines
}

// setLine leaves multi.txt in dir as origin has it but for line n, text.
func setLine(t *testing.T, dir string, n int, text string) {
	t.Helper()
	lines := multiLines()
	lines[n-1] = text
	testgit.WriteFile(t, filepath.Join(dir, "multi.txt"), strings.Join(lines, "\n")+"\n")
}

// twoClones is an origin with multi.txt and two clones of it, as on two
// machines: f.Clone on feat-a and a second clone, returned, on feat-b.
func twoClones(t *testing.T) (testgit.Fixture, string) {
	t.Helper()
	f := testgit.NewFixture(t)
	testgit.WriteFile(t, filepath.Join(f.Seed, "multi.txt"), strings.Join(multiLines(), "\n")+"\n")
	testgit.Git(t, f.Seed, "add", "multi.txt")
	testgit.Git(t, f.Seed, "commit", "-q", "-m", "multi")
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, f.Clone, "pull", "-q", "--ff-only")
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	clone2 := filepath.Join(f.Base, "clone2")
	testgit.Git(t, f.Base, "clone", "-q", f.Origin, clone2)
	testgit.Git(t, clone2, "switch", "-q", "-c", "feat-b")
	return f, clone2
}

// advanceOrigin pushes a commit that adds file to origin's main and
// fast-forwards dir, on a branch with no commits of its own, onto it.
func advanceOrigin(t *testing.T, f testgit.Fixture, dir, file string) {
	t.Helper()
	testgit.WriteFile(t, filepath.Join(f.Seed, file), file+"\n")
	testgit.Git(t, f.Seed, "add", file)
	testgit.Git(t, f.Seed, "commit", "-q", "-m", file)
	testgit.Git(t, f.Seed, "push", "-q", "origin", "main")
	testgit.Git(t, dir, "fetch", "-q")
	testgit.Git(t, dir, "merge", "-q", "--ff-only", "origin/main")
}

// machine is a daemon on a machine of its own, tracking its clone, with a
// session there.
type machine struct {
	running
	rec     *recorder
	root    string
	session string
	ws      core.WorkstreamID
}

// runMachine runs d, tracks the clone at root, on branch, and opens session
// in it. The track goes over the socket, not through a hook: a stubbed git
// can outlast a hook's 100 ms budget for finding the worktree.
func runMachine(t *testing.T, d *Daemon, rec *recorder, dir, root, branch, session string) machine {
	t.Helper()
	r := runDaemon(t, d, dir)
	if resp := call(t, r.sock, wire.Request{Type: wire.TypeTrack, Path: root}); resp.Error != "" {
		t.Fatalf("track %s: %s", root, resp.Error)
	}
	hook(t, r.sock, "SessionStart", session, root, "", "")
	return machine{running: r, rec: rec, root: root, session: session, ws: WorkstreamIDFor(d.ID(), root, branch)}
}

// startPair runs a daemon on each clone (feat-a and feat-b) against server
// and waits until both see their overlap on multi.txt. Each setup runs on
// both daemons before they start.
func startPair(t *testing.T, server, rootA, rootB string, setup ...func(*Daemon)) (machine, machine) {
	t.Helper()
	da, recA, dirA := newLogged(t, server)
	db, recB, dirB := newLogged(t, server)
	for _, s := range setup {
		s(da)
		s(db)
	}
	a := runMachine(t, da, recA, dirA, rootA, "feat-a", "sA")
	b := runMachine(t, db, recB, dirB, rootB, "feat-b", "sB")
	waitOverlap(t, a, b)
	return a, b
}

func waitOverlap(t *testing.T, ms ...machine) {
	t.Helper()
	for _, m := range ms {
		eventually(t, 15*time.Second, m.session+" sees the overlap on multi.txt", func() bool {
			return hasPath(m.d.overlapsFor(m.ws), "multi.txt")
		})
	}
}

// waitConflicts waits until each machine has one conflict.
func waitConflicts(t *testing.T, ms ...machine) {
	t.Helper()
	for _, m := range ms {
		eventually(t, 20*time.Second, m.session+"'s daemon finds the conflict", func() bool {
			return len(m.d.conflictsFor(m.ws)) == 1
		})
	}
}

// warnsOf lists the messages rec got at Warn or above.
func warnsOf(rec *recorder) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []string
	for i, m := range rec.msgs {
		if l := rec.attrs[i]["level"]; l == "WARN" || l == "ERROR" {
			out = append(out, m)
		}
	}
	return out
}

// leaks reports whether a record of rec carries value in its message or an
// attr.
func leaks(rec *recorder, value string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, m := range rec.msgs {
		if strings.Contains(m, value) {
			return true
		}
		for _, v := range rec.attrs[i] {
			if strings.Contains(v, value) {
				return true
			}
		}
	}
	return false
}

// fakeClock is a detector clock that moves only when told.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// storeOf is the one store under the state dir of a daemon that tracks a
// single repo.
func storeOf(t *testing.T, stateDir string) string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(stateDir, "repos", "*.git"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("stores under %s = %v, %v; want one", stateDir, dirs, err)
	}
	return dirs[0]
}

func hasRef(store, ref string) bool {
	return exec.Command("git", "--git-dir="+store, "rev-parse", "--verify", "-q", ref).Run() == nil
}

// TestRemoteConflictBothDaemons: two daemons on two machines change line 2
// of multi.txt; each finds the same conflict, and each session's first edit
// of the file is stopped, naming the other branch and daemon.
func TestRemoteConflictBothDaemons(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	a, b := startPair(t, newServe(t).URL, f.Clone, clone2)
	waitConflicts(t, a, b)
	ca, cb := a.d.conflictsFor(a.ws)[0], b.d.conflictsFor(b.ws)[0]
	if !reflect.DeepEqual(ca, cb) {
		t.Fatalf("the daemons disagree on the conflict:\n a: %+v\n b: %+v", ca, cb)
	}
	if ca.Path != "multi.txt" || ca.Kind != "content" || formatLines(ca.Lines[0]) != "2" || formatLines(ca.Lines[1]) != "2" {
		t.Fatalf("conflict = %+v, want multi.txt at line 2 on both sides", ca)
	}
	for _, c := range []struct {
		me, other machine
		branch    string
	}{{a, b, "feat-b"}, {b, a, "feat-a"}} {
		edit := filepath.Join(c.me.root, "multi.txt")
		out := hook(t, c.me.sock, "PreToolUse", c.me.session, c.me.root, "Edit", edit)
		names := `branch \"` + c.branch + `\" (daemon ` + short(string(c.other.d.ID())) + `)`
		if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, names) {
			t.Fatalf("%s's first Edit of multi.txt = %s, want a deny naming %s", c.me.session, out, names)
		}
		if out := hook(t, c.me.sock, "PreToolUse", c.me.session, c.me.root, "Edit", edit); out != "{}" {
			t.Fatalf("%s's retried Edit = %s, want {}", c.me.session, out)
		}
	}
}

// TestRemoteNoConflictDifferentLines: lines 2 and 9 merge cleanly; both
// daemons exchanged snapshots and checked the pair.
func TestRemoteNoConflictDifferentLines(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 9, "line 9, from b")
	a, b := startPair(t, newServe(t).URL, f.Clone, clone2)
	for _, c := range []struct{ me, other machine }{{a, b}, {b, a}} {
		eventually(t, 20*time.Second, c.me.session+"'s daemon uploads and downloads", func() bool {
			_, up := c.me.rec.find("snapshot uploaded", map[string]string{"ws": string(c.me.ws)})
			_, down := c.me.rec.find("snapshot downloaded", map[string]string{"ws": string(c.other.ws)})
			return up && down
		})
	}
	for _, m := range []machine{a, b} {
		waitRuns(t, m.rec, 3)
		if got, ok := m.rec.find("conflict detected", nil); ok {
			t.Fatalf("lines 2 and 9 were reported as a conflict: %v", got)
		}
		if cs := m.d.conflictsFor(m.ws); len(cs) != 0 {
			t.Fatalf("conflictsFor(%s) = %+v, want none", m.session, cs)
		}
		// No conflict proves nothing unless the check ran and worked.
		if w := warnsOf(m.rec); len(w) != 0 {
			t.Errorf("%s's daemon warned: %v", m.session, w)
		}
		if !m.rec.any("detection done", func(a map[string]string) bool { return a["checks"] != "" && a["checks"] != "0" }) {
			t.Errorf("no run of %s's daemon checked the remote pair", m.session)
		}
		for _, msg := range []string{"snapshot uploaded", "snapshot downloaded"} {
			got, _ := m.rec.find(msg, nil)
			if got["commit"] == "" || got["bytes"] == "" || got["bytes"] == "0" || got["ms"] == "" {
				t.Errorf("%s record without commit, bytes or ms: %v", msg, got)
			}
		}
	}
}

// TestRemotePairWaitsForBundle: until the other daemon's bundle reaches
// serve, the pair is not checked, and that is no warning on every run.
func TestRemotePairWaitsForBundle(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	office := serve.New().Handler()
	var mu sync.Mutex
	var mute core.DaemonID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		m := mute
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/snapshot" && core.DaemonID(r.URL.Query().Get("daemon_id")) == m {
			io.Copy(io.Discard, r.Body)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		office.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	da, recA, dirA := newLogged(t, srv.URL)
	db, recB, dirB := newLogged(t, srv.URL)
	mu.Lock()
	mute = db.ID()
	mu.Unlock()
	a := runMachine(t, da, recA, dirA, f.Clone, "feat-a", "sA")
	b := runMachine(t, db, recB, dirB, clone2, "feat-b", "sB")
	waitOverlap(t, a, b)
	eventually(t, 15*time.Second, "A uploads its side", func() bool {
		_, ok := a.rec.find("snapshot uploaded", map[string]string{"ws": string(a.ws)})
		return ok
	})
	eventually(t, 15*time.Second, "B's upload is refused", func() bool {
		_, ok := b.rec.find("snapshot upload failed", map[string]string{"ws": string(b.ws)})
		return ok
	})
	waitRuns(t, a.rec, 5)
	waitRuns(t, b.rec, 2)
	if w := warnsOf(a.rec); len(w) != 0 {
		t.Errorf("A warned while waiting for B's bundle: %v", w)
	}
	if cs := a.d.conflictsFor(a.ws); len(cs) != 0 {
		t.Errorf("conflictsFor(A) = %+v without B's snapshot, want none", cs)
	}
	if n := b.rec.count("snapshot upload failed"); n != 1 {
		t.Errorf("B logged its refused upload %d times, want once per message", n)
	}
}

// gitCalls returns a dir holding a git that appends a line to calls for each
// call of subcommand sub whose arguments hold pattern, and runs the real git
// for every call. Prepend the dir to PATH with t.Setenv.
func gitCalls(t *testing.T, sub, pattern string) (dir, calls string) {
	t.Helper()
	calls = filepath.Join(t.TempDir(), "calls")
	return stubGit(t, sub, "case \"$*\" in *'"+pattern+"'*) echo call >> '"+calls+"' ;; esac"), calls
}

// baseFetches is gitCalls for each fetch into refs/dugout/base/.
func baseFetches(t *testing.T) (dir, calls string) {
	t.Helper()
	return gitCalls(t, "fetch", "refs/dugout/base/")
}

// TestRemoteMissingBaseFetchOncePerMinute: a snapshot on a base this
// daemon's store lacks has the base fetched from origin; a fetch that fails
// is logged once and not tried again before a minute, on the detector's
// clock.
func TestRemoteMissingBaseFetchOncePerMinute(t *testing.T) {
	f, clone2 := twoClones(t)
	// Origin moves on and only A follows it: B's clone lacks A's base.
	advanceOrigin(t, f, f.Clone, "news.txt")
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	stub, calls := baseFetches(t)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	clock := newFakeClock()
	a, b := startPair(t, newServe(t).URL, f.Clone, clone2, func(d *Daemon) { d.clock = clock.Now })
	repo := repoOf(t, clone2)
	eventually(t, 20*time.Second, "B fetches the base", func() bool {
		got, ok := b.rec.find("base fetched", map[string]string{"repo": repo})
		return ok && got["ms"] != ""
	})
	waitConflicts(t, a, b)
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("%d base fetches, want 1", n)
	}

	// Origin moves on again, A follows, and origin goes out of reach.
	advanceOrigin(t, f, f.Clone, "more.txt")
	if err := os.Rename(f.Origin, f.Origin+".gone"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "B has no base for A's new snapshot", func() bool { return b.rec.count("no base") == 1 })
	waitRuns(t, b.rec, 3)
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("%d base fetches, want 1: one ran within a minute of the last", n)
	}
	clock.Add(time.Minute)
	eventually(t, 20*time.Second, "a fetch once the minute is over", func() bool { return callCount(t, calls) == 2 })
	eventually(t, 10*time.Second, "base fetch failed", func() bool { return b.rec.count("base fetch failed") == 1 })
	waitRuns(t, b.rec, 3)
	if n := callCount(t, calls); n != 2 {
		t.Fatalf("%d base fetches, want 2: a failed one was tried again within the minute", n)
	}
	clock.Add(time.Minute)
	eventually(t, 20*time.Second, "another fetch a minute later", func() bool { return callCount(t, calls) == 3 })
	waitRuns(t, b.rec, 2)
	if n := b.rec.count("base fetch failed"); n != 1 {
		t.Errorf("base fetch failed logged %d times, want once per message", n)
	}
	if n := b.rec.count("no base"); n != 1 {
		t.Errorf("no base logged %d times, want once per commit", n)
	}
	// B still checks against the last snapshot of A it could import.
	if cs := b.d.conflictsFor(b.ws); len(cs) != 1 {
		t.Errorf("conflictsFor(B) = %+v, want the multi.txt conflict", cs)
	}
}

// TestFetchTargetUsesCloneSSH: the base fetch runs with the ssh command the
// clone's own config sets, which the store's git, on the store, never reads.
func TestFetchTargetUsesCloneSSH(t *testing.T) {
	f := testgit.NewFixture(t)
	for _, name := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		t.Setenv(name, "") // restored after the test
		os.Unsetenv(name)
	}
	const sshCmd = "ssh -i /nonexistent/dugout-key"
	testgit.Git(t, f.Clone, "config", "core.sshCommand", sshCmd)
	scan, err := gitscan.Scan(context.Background(), f.Clone)
	if err != nil {
		t.Fatal(err)
	}
	st, err := snapshot.Open(t.TempDir(), scan.Repo)
	if err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(t.TempDir(), "seen")
	stub := stubGit(t, "fetch", "case \"$*\" in *refs/dugout/base/*) printf '%s\\n' \"$GIT_SSH_COMMAND\" >> '"+seen+"' ;; esac")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := fetchTarget(context.Background(), st, scan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != sshCmd {
		t.Fatalf("the base fetch ran with GIT_SSH_COMMAND=%q, want the clone's %q", strings.TrimSpace(string(got)), sshCmd)
	}
}

// TestRemoteSecretNotUploaded: a key in a file outside the pair stays home
// and blocks nothing, since only the pair's paths go up; a snapshot whose
// paired file holds a key is not uploaded; that is logged once per commit,
// with the path and the rule, never the value, and serve keeps the bundle
// uploaded before.
func TestRemoteSecretNotUploaded(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	serveRec := &recorder{}
	office := serve.New()
	office.SetLogger(slog.New(serveRec))
	srv := httptest.NewServer(office.Handler())
	t.Cleanup(func() { closeServer(srv) })
	a, b := startPair(t, srv.URL, f.Clone, clone2)
	waitConflicts(t, a, b)
	clean, _ := a.rec.find("snapshot uploaded", map[string]string{"ws": string(a.ws)})

	secret := "AKIA" + strings.Repeat("Q", 16)
	testgit.WriteFile(t, filepath.Join(f.Clone, "creds.txt"), "aws_key = "+secret+"\n")
	waitRuns(t, a.rec, 3)
	if a.rec.count("snapshot not uploaded") != 0 {
		t.Fatal("a key outside the pair blocked the upload")
	}
	setLine(t, f.Clone, 5, "aws_key = "+secret)
	// At Warn: B checks A's stale snapshot while it lasts.
	want := map[string]string{"ws": string(a.ws), "path": "multi.txt", "rule": "aws-access-key", "level": "WARN"}
	eventually(t, 15*time.Second, "snapshot not uploaded, at Warn", func() bool {
		_, ok := a.rec.find("snapshot not uploaded", want)
		return ok
	})
	waitRuns(t, a.rec, 3)
	if n := a.rec.count("snapshot not uploaded"); n != 1 {
		t.Errorf("snapshot not uploaded logged %d times, want once per commit", n)
	}
	if a.rec.any("snapshot uploaded", func(r map[string]string) bool { return r["commit"] != clean["commit"] }) {
		t.Fatal("the snapshot with the key was uploaded")
	}
	held, _, _, err := b.d.uplink.GetSnapshot(context.Background(), a.ws, "")
	if err != nil || !strings.HasPrefix(held.Commit, clean["commit"]) {
		t.Fatalf("serve holds %+v (err %v), want the snapshot uploaded before, %s", held, err, clean["commit"])
	}
	for name, rec := range map[string]*recorder{"A": a.rec, "B": b.rec, "serve": serveRec} {
		if leaks(rec, secret) {
			t.Errorf("%s's log carries the key", name)
		}
	}
}

// TestRemoteReuploadOnReconnect: a serve that restarts without the
// bundles gets both snapshots again once the streams reconnect, and the
// conflict stays. The daemons' full states reach the new serve before their
// streams do, so the pair never goes away: only the reconnect uploads again.
func TestRemoteReuploadOnReconnect(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	var cur atomic.Pointer[serve.Server]
	cur.Store(serve.New())
	var mu sync.Mutex
	states := map[core.DaemonID][]byte{} // each daemon's last full state
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/msg" {
			body, _ := io.ReadAll(r.Body)
			var m wire.Message
			if json.Unmarshal(body, &m) == nil && m.Type == wire.TypeFullState {
				mu.Lock()
				states[m.Daemon] = body
				mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		cur.Load().Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	a, b := startPair(t, srv.URL, f.Clone, clone2)
	waitConflicts(t, a, b)
	id := a.d.conflictsFor(a.ws)[0].ID
	before := map[core.WorkstreamID]int{}
	for _, m := range []machine{a, b} {
		waitRuns(t, m.rec, 2) // past the first connect's own upload, if any
		before[m.ws] = countWith(m.rec, "snapshot uploaded", map[string]string{"ws": string(m.ws)})
	}

	fresh := serve.New()
	mu.Lock()
	for d, body := range states {
		req := httptest.NewRequest(http.MethodPost, "/v1/msg", bytes.NewReader(body))
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		fresh.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("replaying %s's full state: %d %s", d, rr.Code, rr.Body)
		}
	}
	mu.Unlock()
	cur.Store(fresh)
	srv.CloseClientConnections()

	for _, m := range []machine{a, b} {
		eventually(t, 15*time.Second, m.session+"'s daemon uploads again", func() bool {
			return countWith(m.rec, "snapshot uploaded", map[string]string{"ws": string(m.ws)}) > before[m.ws]
		})
	}
	eventually(t, 10*time.Second, "the new serve lists the conflict", func() bool {
		cs := warnings(t, srv).Conflicts
		return len(cs) == 1 && cs[0].ID == id
	})
	for _, m := range []machine{a, b} {
		if got, ok := m.rec.find("conflict resolved", nil); ok {
			t.Errorf("%s's daemon resolved the conflict across the restart: %v", m.session, got)
		}
		if cs := m.d.conflictsFor(m.ws); len(cs) != 1 || cs[0].ID != id {
			t.Errorf("conflictsFor(%s) = %+v, want %s", m.session, cs, id)
		}
	}
}

// TestRemotePeerGone: once the other workstream is gone, its conflict
// resolves and the next gc drops its peer ref from the store.
func TestRemotePeerGone(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	clock := newFakeClock()
	a, b := startPair(t, newServe(t).URL, f.Clone, clone2, func(d *Daemon) { d.clock = clock.Now })
	waitConflicts(t, a, b)
	id := a.d.conflictsFor(a.ws)[0].ID
	store, peer := storeOf(t, a.dir), snapshot.PeerRef(string(b.ws))
	if !hasRef(store, peer) {
		t.Fatalf("A's store has no %s before B goes: the test proves nothing", peer)
	}

	if err := os.RemoveAll(clone2); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "A resolves the conflict", func() bool {
		_, ok := a.rec.find("conflict resolved", map[string]string{"id": id})
		return ok
	})
	gcs := a.rec.count("store gc")
	clock.Add(gcEvery)
	eventually(t, 20*time.Second, "A's next gc", func() bool { return a.rec.count("store gc") > gcs })
	if hasRef(store, peer) {
		t.Fatalf("A's store still has %s after the gc", peer)
	}
}

// TestRemoteConflictsReported: serve's state lists the conflict, which both
// daemons reported.
func TestRemoteConflictsReported(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	office := serve.New().Handler()
	var mu sync.Mutex
	reported := map[core.DaemonID][]string{} // conflict IDs of each daemon's last report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/msg" {
			body, _ := io.ReadAll(r.Body)
			var m wire.Message
			if json.Unmarshal(body, &m) == nil && m.Type == wire.TypeConflicts {
				var ids []string
				for _, c := range m.Conflicts {
					ids = append(ids, c.ID)
				}
				mu.Lock()
				reported[m.Daemon] = ids
				mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		office.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { closeServer(srv) })
	a, b := startPair(t, srv.URL, f.Clone, clone2)
	waitConflicts(t, a, b)
	id := a.d.conflictsFor(a.ws)[0].ID
	eventually(t, 10*time.Second, "serve lists the conflict, reported by both daemons", func() bool {
		cs := warnings(t, srv).Conflicts
		mu.Lock()
		defer mu.Unlock()
		return len(cs) == 1 && cs[0].ID == id &&
			slices.Equal(reported[a.d.ID()], []string{id}) && slices.Equal(reported[b.d.ID()], []string{id})
	})
}

// localAndRemote sets d, not running, as if it had scanned f's clone
// (feat-a) and wt (feat-b), which overlap on shared.go, and serve had paired
// the clone with a workstream of another daemon's on shared.go too. It
// returns the workstreams of the clone and of wt.
func localAndRemote(t *testing.T, d *Daemon, f testgit.Fixture, wt string) (wsA, wsB core.WorkstreamID) {
	t.Helper()
	for _, root := range []string{f.Clone, wt} {
		scan, err := gitscan.Scan(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		d.worktrees[root] = &worktree{root: root, snap: &scan, ws: WorkstreamIDFor(d.ID(), root, scan.Branch)}
	}
	d.notifyIfChanged() // the local core sees the shared.go overlap
	wsA, wsB = WorkstreamIDFor(d.ID(), f.Clone, "feat-a"), WorkstreamIDFor(d.ID(), wt, "feat-b")
	mine := core.Side{Daemon: d.ID(), Workstream: wsA, Branch: "feat-a"}
	remote := core.Side{Daemon: "d2", Workstream: "d2:x", Branch: "x"}
	d.setWarnings([]core.Warning{between(repoOf(t, f.Clone), "shared.go", mine, remote)})
	return wsA, wsB
}

// TestRemoteHungServeKeepsLocalPairs: in a run with a local and a remote
// pair, the local pair's conflict is published before the run waits on
// serve, so a hung serve does not hold it up.
func TestRemoteHungServeKeepsLocalPairs(t *testing.T) {
	f, wt := overlap(t)
	var held atomic.Int32
	end := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read first: until the body is read, net/http does not see the
		// client go, and the request's context never ends.
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/snapshot" {
			held.Add(1)
			select {
			case <-r.Context().Done():
			case <-end:
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() { closeServer(srv) })
	t.Cleanup(func() { close(end) }) // runs first
	d, _, dir := newLogged(t, srv.URL)
	wsA, wsB := localAndRemote(t, d, f, wt)

	ctx, cancel := context.WithCancel(context.Background())
	det := newDetector(d, dir)
	done := make(chan struct{})
	go func() { defer close(done); det.run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, 20*time.Second, "the run waits on serve", func() bool { return held.Load() > 0 })
	if cs := d.conflictsFor(wsA); len(cs) != 1 || cs[0].Path != "shared.go" || !touches(cs[0].Sides, wsB) {
		t.Fatalf("while serve hangs, conflictsFor(A) = %+v, want the local shared.go conflict", cs)
	}
}

// TestRemoteUnreachableServePausesSync: once an uplink call finds serve not
// answering, the run makes no other, and neither do the next runs until a
// minute passes on the detector's clock or the stream connects again.
// Local pairs are checked meanwhile.
func TestRemoteUnreachableServePausesSync(t *testing.T) {
	f, wt := overlap(t)
	var calls atomic.Int32
	var hang atomic.Bool
	hang.Store(true)
	end := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // see TestRemoteHungServeKeepsLocalPairs
		if r.URL.Path != "/v1/snapshot" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		switch {
		case hang.Load():
			select {
			case <-r.Context().Done():
			case <-end:
			}
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { closeServer(srv) })
	t.Cleanup(func() { close(end) }) // runs first
	d, rec, dir := newLogged(t, srv.URL)
	wsA, _ := localAndRemote(t, d, f, wt)
	clock := newFakeClock()
	det := newDetector(d, dir)
	det.now, det.uplinkTimeout = clock.Now, 200*time.Millisecond
	ctx := context.Background()
	sideB := func() []core.Snippet {
		cs := d.conflictsFor(wsA)
		if len(cs) != 1 {
			t.Fatalf("conflictsFor(A) = %+v, want the local shared.go conflict", cs)
		}
		return cs[0].Text[1-mineOf(cs[0].Sides, wsA)]
	}

	det.run(ctx)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d calls to serve in the run whose first call timed out, want 1", n)
	}
	sideB()
	// B changes: the next runs check the local pair again, and leave serve be.
	testgit.WriteFile(t, filepath.Join(wt, "shared.go"), "from c\n")
	det.run(ctx)
	det.run(ctx)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d calls to serve, want 1: a run within the backoff called it", n)
	}
	if got := sideB(); !reflect.DeepEqual(got, []core.Snippet{{Lines: []string{"from c"}}}) {
		t.Fatalf("B's section = %+v, want B's new line: the local pair was not checked again", got)
	}
	clock.Add(59 * time.Second)
	det.run(ctx)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d calls to serve, want 1: a run 59 s after the timeout called it", n)
	}
	clock.Add(time.Second)
	det.run(ctx)
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d calls to serve, want 2: a minute after the timeout, one call, which times out", n)
	}
	// The stream connects again, to a serve that answers.
	hang.Store(false)
	d.uplink.OnConnect()
	det.run(ctx)
	if n := calls.Load(); n != 4 {
		t.Fatalf("%d calls to serve, want 4: the upload and the download once the stream connects", n)
	}
	if _, ok := rec.find("snapshot uploaded", map[string]string{"ws": string(wsA)}); !ok {
		t.Fatal("no upload once serve answers")
	}
	if n := rec.count("snapshot upload failed"); n != 1 {
		t.Errorf("snapshot upload failed logged %d times, want once per message", n)
	}
}

// relayRig is a daemon, not running, as localAndRemote leaves it, on a
// detector driven by hand on a fake clock, against a serve whose
// /v1/snapshot is snap and which takes every other request.
type relayRig struct {
	d     *Daemon
	det   *detector
	rec   *recorder
	clock *fakeClock
	f     testgit.Fixture
	ws    core.WorkstreamID // the clone's, paired with d2:x
}

func newRelayRig(t *testing.T, snap http.HandlerFunc) relayRig {
	t.Helper()
	f, wt := overlap(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // see TestRemoteHungServeKeepsLocalPairs
		if r.URL.Path == "/v1/snapshot" {
			snap(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() { closeServer(srv) })
	d, rec, dir := newLogged(t, srv.URL)
	ws, _ := localAndRemote(t, d, f, wt)
	clock := newFakeClock()
	det := newDetector(d, dir)
	det.now = clock.Now
	return relayRig{d: d, det: det, rec: rec, clock: clock, f: f, ws: ws}
}

// runs runs the detector n times.
func (r relayRig) runs(n int) {
	for range n {
		r.det.run(context.Background())
	}
}

// change gives the clone's side a new snapshot on the next run.
func (r relayRig) change(t *testing.T, text string) {
	t.Helper()
	atomicWrite(t, r.f.Base, filepath.Join(r.f.Clone, "shared.go"), text+"\n")
}

// TestRemoteOversizeNotRebuilt: a bundle over the limit is built once per
// commit and logged once, not built again on every run; a new commit is, and
// so is the same one once checkRetry passes.
func TestRemoteOversizeNotRebuilt(t *testing.T) {
	var posts atomic.Int32
	r := newRelayRig(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, req)
	})
	r.det.maxBundle = 1
	stub, calls := gitCalls(t, "bundle", "bundle create")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.runs(3)
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("Bundle ran %d times over 3 runs of one commit, want 1", n)
	}
	if n := posts.Load(); n != 0 {
		t.Fatalf("%d uploads of a bundle over the limit, want none", n)
	}
	r.change(t, "from a, again")
	r.runs(2)
	if n := callCount(t, calls); n != 2 {
		t.Fatalf("Bundle ran %d times, want 2: once per commit", n)
	}
	r.clock.Add(r.d.checkRetry)
	r.runs(1)
	if n := callCount(t, calls); n != 3 {
		t.Fatalf("Bundle ran %d times, want 3: the same commit again once checkRetry passed", n)
	}
	if n := countWith(r.rec, "snapshot too big to upload", map[string]string{"ws": string(r.ws), "level": "INFO"}); n != 2 {
		t.Errorf("snapshot too big to upload logged %d times at Info, want once per commit", n)
	}
	if w := warnsOf(r.rec); len(w) != 0 {
		t.Errorf("warned: %v", w)
	}
}

// TestRemoteOldServeNotRetried: an upload that serve refuses for a cause that
// lasts, a serve too old to relay snapshots or a full one, is tried
// once per commit, and logged once, until checkRetry passes.
func TestRemoteOldServeNotRetried(t *testing.T) {
	for _, c := range []struct {
		status     int
		msg, level string
	}{
		{http.StatusUnsupportedMediaType, "serve does not relay snapshots", "INFO"},
		{http.StatusNotFound, "serve does not relay snapshots", "INFO"},
		{http.StatusMethodNotAllowed, "serve does not relay snapshots", "INFO"},
		{http.StatusInsufficientStorage, "serve snapshot storage full", "WARN"},
	} {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			var posts atomic.Int32
			r := newRelayRig(t, func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodPost {
					posts.Add(1)
					w.WriteHeader(c.status)
					return
				}
				http.NotFound(w, req)
			})
			r.runs(3)
			if n := posts.Load(); n != 1 {
				t.Fatalf("%d uploads over 3 runs of one commit, want 1", n)
			}
			r.change(t, "from a, again")
			r.runs(2)
			if n := posts.Load(); n != 2 {
				t.Fatalf("%d uploads, want 2: one per commit", n)
			}
			r.clock.Add(r.d.checkRetry)
			r.runs(1)
			if n := posts.Load(); n != 3 {
				t.Fatalf("%d uploads, want 3: the same commit again once checkRetry passed", n)
			}
			want := map[string]string{"ws": string(r.ws), "level": c.level, "status": strconv.Itoa(c.status)}
			if n := countWith(r.rec, c.msg, want); n != 2 {
				t.Errorf("%s logged %d times at %s, want once per commit", c.msg, n, c.level)
			}
			if n := r.rec.count("snapshot upload failed"); n != 0 {
				t.Errorf("the refusal was logged as a failed upload %d times", n)
			}
		})
	}
}

// TestRemoteFailedImportNotRedownloaded: a bundle that does not import is
// downloaded once; the next runs ask for it by its commit and serve answers
// 304. Its kept bytes are imported again once checkRetry passes, and a new
// commit is downloaded.
func TestRemoteFailedImportNotRedownloaded(t *testing.T) {
	var mu sync.Mutex
	held := strings.Repeat("c", 40)
	var full, unchanged atomic.Int32
	r := newRelayRig(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		commit := held
		mu.Unlock()
		if req.Header.Get("If-None-Match") == `"`+commit+`"` {
			unchanged.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set(wire.HeaderCommit, commit)
		w.Header().Set(wire.HeaderBase, strings.Repeat("b", 40))
		w.Write([]byte("not a bundle"))
	})
	stub, imports := gitCalls(t, "bundle", "bundle verify")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.runs(3)
	if n := full.Load(); n != 1 {
		t.Fatalf("%d full downloads over 3 runs of a bundle that does not import, want 1", n)
	}
	if n := unchanged.Load(); n != 2 {
		t.Fatalf("%d 304s over the next 2 runs, want 2", n)
	}
	if n := callCount(t, imports); n != 1 {
		t.Fatalf("%d imports, want 1: one ran again before checkRetry", n)
	}
	r.clock.Add(r.d.checkRetry)
	r.runs(1)
	if n, m := full.Load(), callCount(t, imports); n != 1 || m != 2 {
		t.Fatalf("once checkRetry passed: %d full downloads and %d imports, want 1 and 2", n, m)
	}
	mu.Lock()
	held = strings.Repeat("d", 40)
	mu.Unlock()
	r.runs(1)
	if n := full.Load(); n != 2 {
		t.Fatalf("%d full downloads, want 2: a new commit is downloaded", n)
	}
	if n := r.rec.count("snapshot download failed"); n != 1 {
		t.Errorf("snapshot download failed logged %d times, want once per message", n)
	}
}

// TestRemoteMissingObjectsDownloadsAgain: a check of a remote pair that
// misses objects takes this daemon's side again and drops the other's, which
// the next run downloads whole; the conflict stays meanwhile.
func TestRemoteMissingObjectsDownloadsAgain(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	a, b := startPair(t, newServe(t).URL, f.Clone, clone2)
	waitConflicts(t, a, b)
	id := a.d.conflictsFor(a.ws)[0].ID
	downloads := countWith(a.rec, "snapshot downloaded", map[string]string{"ws": string(b.ws)})

	// The first merge-tree in A's store after this fails as if objects were gone.
	marker := filepath.Join(t.TempDir(), "failed-once")
	stub := stubGit(t, "merge-tree", "case \"$*\" in *'"+a.dir+"'*) if [ ! -e '"+marker+"' ]; then touch '"+marker+
		"'; echo 'fatal: missing blob' >&2; exit 128; fi ;; esac")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	atomicWrite(t, f.Base, filepath.Join(f.Clone, "multi.txt"), strings.Replace(
		strings.Join(multiLines(), "\n")+"\n", "line 2\n", "line 2, from a again\n", 1))
	eventually(t, 20*time.Second, "A downloads B's snapshot again after the failed check", func() bool {
		_, err := os.Stat(marker)
		return err == nil && countWith(a.rec, "snapshot downloaded", map[string]string{"ws": string(b.ws)}) > downloads
	})
	eventually(t, 20*time.Second, "A checks the pair on its new side", func() bool {
		cs := a.d.conflictsFor(a.ws)
		return len(cs) == 1 && reflect.DeepEqual(cs[0].Text[mineOf(cs[0].Sides, a.ws)], []core.Snippet{{Lines: []string{"line 2, from a again"}}})
	})
	if got, ok := a.rec.find("check failed", nil); ok {
		t.Errorf("the failed check was kept as a failure: %v", got)
	}
	if got, ok := a.rec.find("conflict resolved", map[string]string{"id": id}); ok {
		t.Errorf("the conflict went away while B's side was downloaded again: %v", got)
	}
}

// TestRemoteEmptyChangeClearsConflict: a side whose only change left is a
// file no snapshot takes (over 1 MB) still uploads its snapshot, which then
// changes nothing, so the other daemon's conflict goes away. The pair stays,
// through the big file both sides change.
func TestRemoteEmptyChangeClearsConflict(t *testing.T) {
	f, clone2 := twoClones(t)
	setLine(t, f.Clone, 2, "line 2, from a")
	setLine(t, clone2, 2, "line 2, from b")
	a, b := startPair(t, newServe(t).URL, f.Clone, clone2)
	waitConflicts(t, a, b)
	id := b.d.conflictsFor(b.ws)[0].ID
	for _, dir := range []string{f.Clone, clone2} {
		testgit.WriteFile(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", 1<<20+1))
	}
	for _, m := range []machine{a, b} {
		eventually(t, 15*time.Second, m.session+" sees the overlap on big.bin", func() bool {
			return hasPath(m.d.overlapsFor(m.ws), "big.bin")
		})
	}
	setLine(t, f.Clone, 2, "line 2") // as origin has it: A's snapshot changes nothing now
	eventually(t, 20*time.Second, "B's conflict goes away", func() bool {
		_, ok := b.rec.find("conflict resolved", map[string]string{"id": id})
		return ok
	})
	if !hasPath(b.d.overlapsFor(b.ws), "big.bin") {
		t.Fatal("the pair went away: the test proves nothing")
	}
	if out := hook(t, b.sock, "PreToolUse", b.session, b.root, "Edit", filepath.Join(b.root, "multi.txt")); out != "{}" {
		t.Fatalf("B's Edit of multi.txt once A undid its change = %s, want {}", out)
	}
}
