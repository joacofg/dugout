package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/snapshot"
	"github.com/joacofg/dugout/internal/textual"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	checkTimeout = 30 * time.Second
	checkRetry   = 5 * time.Minute // a failed check or Take, on the same snapshots or scan; a refused upload or failed import, of the same commit
	gcTimeout    = 5 * time.Minute
	gcEvery      = 24 * time.Hour
	gcLoose      = 5000
	gcPacks      = 50        // an import writes a pack
	gcRetry      = time.Hour // a failed gc
	hashBig      = 1 << 20   // a regular file over this is not in a snapshot (snapshot's own cap): its size stands for it
	hashBudget   = 64 << 20  // most bytes a fingerprint hashes; past it the Take is not skipped
)

// needGC reports whether a store last collected at last, holding loose loose
// objects and packs packs, is due a gc at now: on its first run, every
// gcEvery, past gcLoose loose objects and past gcPacks packs.
func needGC(last, now time.Time, loose, packs int) bool {
	return last.IsZero() || now.Sub(last) >= gcEvery || loose > gcLoose || packs > gcPacks
}

// overlapsLocked is the union of overlaps for every workstream of this daemon:
// the local core's warnings, plus serve's warnings that pair a local workstream
// with one of another daemon. Serve's warnings between two local workstreams are
// left out, since the local core is fresher. Sorted by ID; d.mu held.
func (d *Daemon) overlapsLocked() []core.Warning {
	out := d.local.WarningsFor(d.id)
	for _, w := range d.warnings {
		if (w.Sides[0].Daemon == d.id) != (w.Sides[1].Daemon == d.id) {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b core.Warning) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func touches(sides [2]core.Side, ws core.WorkstreamID) bool {
	return sides[0].Workstream == ws || sides[1].Workstream == ws
}

// overlapsFor returns the union's warnings that touch ws, sorted by ID.
func (d *Daemon) overlapsFor(ws core.WorkstreamID) []core.Warning {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []core.Warning
	for _, w := range d.overlapsLocked() {
		if touches(w.Sides, ws) {
			out = append(out, w)
		}
	}
	return out
}

// conflictsFor returns the current conflicts that touch ws, sorted by ID.
func (d *Daemon) conflictsFor(ws core.WorkstreamID) []core.Conflict {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []core.Conflict
	for _, c := range d.conflicts {
		if touches(c.Sides, ws) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b core.Conflict) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// kickDetector asks for a detector run soon. It never blocks; a kick during
// a run makes one more run after it.
func (d *Daemon) kickDetector() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// detectLoop runs the detector on every kick until ctx is done.
func (d *Daemon) detectLoop(ctx context.Context) {
	stateDir, err := filepath.Abs(d.cfg.StateDir)
	if err != nil {
		stateDir = d.cfg.StateDir
	}
	det := newDetector(d, stateDir)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kick:
		}
		det.run(ctx)
	}
}

// newDetector returns a detector of d's that has taken and checked nothing
// yet, on d's clock.
func newDetector(d *Daemon, stateDir string) *detector {
	return &detector{
		d: d, stateDir: stateDir, now: d.clock, hashBudget: hashBudget, uplinkTimeout: snapTimeout, maxBundle: wire.MaxBundle,
		repos: map[string]*repoStore{}, probeErr: map[string]string{},
		snaps: map[core.WorkstreamID]snapshot.Snap{}, taken: map[core.WorkstreamID]takeKey{}, takeFailed: map[core.WorkstreamID]takeFailure{},
		results: map[pairKey]pairResult{}, failed: map[pairKey]pairFailure{},
		uploaded: map[core.WorkstreamID]string{}, narrowed: map[core.WorkstreamID]narrowed{}, peers: map[core.WorkstreamID]snapshot.Snap{}, baseFetched: map[string]time.Time{},
		fetchErr: map[string]string{}, baseGot: map[string]int{}, noBase: map[core.WorkstreamID]string{}, secretLogged: map[core.WorkstreamID]string{},
		syncErr: map[core.WorkstreamID]string{}, refused: map[core.WorkstreamID]refusal{}, failedPeer: map[core.WorkstreamID]failedImport{},
	}
}

// detector finds textual conflicts (stage 2) between pairs of workstreams:
// two of this daemon's, or one of this daemon's and one of another's, whose
// snapshot comes through serve (remote.go). Only the detector goroutine
// touches it, and it is the only user of the stores, so a gc never runs
// alongside a Take or a Check.
type detector struct {
	d          *Daemon
	stateDir   string
	now        func() time.Time                    // the wall clock; tests set it
	hashBudget int64                               // most bytes a fingerprint hashes; tests set it
	repos      map[string]*repoStore               // by repo identity
	probeErr   map[string]string                   // last probe error logged, of a repo to probe again
	snaps      map[core.WorkstreamID]snapshot.Snap // last good snapshot
	taken      map[core.WorkstreamID]takeKey       // what the last good Take saw, when its fingerprint can be trusted
	takeFailed map[core.WorkstreamID]takeFailure   // last failed Take, since the last good one
	results    map[pairKey]pairResult              // last good check
	failed     map[pairKey]pairFailure             // last failed check, since the last good one
	current    map[string]core.Conflict            // as last published
	cost       runCost                             // of the current run

	uplinkTimeout time.Duration                       // bound on one uplink call; tests shorten it
	maxBundle     int                                 // largest bundle to upload, serve's; tests shrink it
	uploaded      map[core.WorkstreamID]string        // commit serve has, of each workstream of this daemon's
	narrowed      map[core.WorkstreamID]narrowed      // last narrowing for upload, of each workstream of this daemon's
	peers         map[core.WorkstreamID]snapshot.Snap // last snapshot imported, of each workstream of another daemon's
	failedPeer    map[core.WorkstreamID]failedImport  // last snapshot of another daemon's that did not import, since the last that did
	baseFetched   map[string]time.Time                // last base fetch, by repo
	baseGot       map[string]int                      // base fetches that worked, by repo
	fetchErr      map[string]string                   // last base fetch error logged, by repo
	noBase        map[core.WorkstreamID]string        // commit of another daemon's whose missing base was logged
	secretLogged  map[core.WorkstreamID]string        // commit of this daemon's whose secret was logged
	refused       map[core.WorkstreamID]refusal       // last snapshot of this daemon's that serve will not take
	syncErr       map[core.WorkstreamID]string        // last upload or download error logged
	serveDown     time.Time                           // when a call last found serve not answering
}

// runCost is what a detector run spent on Takes and checks, for the run's
// log line.
type runCost struct {
	takes, checks   int
	takeMS, checkMS int64
}

// takeKey is what a Take depends on: the scan and what each file it lists
// holds. A Take with the same key as the last good one would write the same
// commit: a file is read by content, so a tool that keeps mtimes (cp -p,
// rsync -a) cannot slip past it. A submodule whose HEAD moves, and a file
// over hashBig that changes without changing size, do (a snapshot leaves the
// latter out anyway). Accepted limits.
type takeKey struct {
	scan  string // the scan's fields, but the file lists
	files string // Files and AllFiles
	state string // what each of AllFiles holds
}

// fingerprint is scan's takeKey and whether it is stable, that is, whether a
// Take with the same key can be skipped: not when the files to hash add up
// to more than budget bytes or one cannot be read. A file outside AllFiles
// cannot change without showing up in the next scan, so this covers every
// change. A file that is gone counts as "-". Only regular files are read.
func fingerprint(scan gitscan.Snapshot, budget int64) (takeKey, bool) {
	var state strings.Builder
	stable := true
	for _, p := range scan.AllFiles {
		full := filepath.Join(scan.Root, p)
		fi, err := os.Lstat(full)
		switch {
		case err != nil:
			if !errors.Is(err, fs.ErrNotExist) {
				stable = false
			}
			state.WriteString("-")
		case fi.Mode()&os.ModeSymlink != 0:
			dest, err := os.Readlink(full)
			if err != nil {
				stable = false
			}
			fmt.Fprintf(&state, "l:%q", dest)
		case fi.IsDir(): // a submodule
			fmt.Fprintf(&state, "d:%d", fi.ModTime().UnixNano())
		case !fi.Mode().IsRegular():
			// A FIFO, socket or device: opening one can block for good, and
			// no snapshot holds its content.
			fmt.Fprintf(&state, "x:%v", fi.Mode())
		case fi.Size() > hashBig:
			fmt.Fprintf(&state, "b:%d", fi.Size())
		default:
			budget -= fi.Size()
			if budget < 0 {
				stable = false
				state.WriteString("f:?")
				break
			}
			sum, err := hashFile(full)
			if err != nil {
				stable = false
			}
			fmt.Fprintf(&state, "f:%x", sum)
		}
		state.WriteString(";")
	}
	return takeKey{
		scan: fmt.Sprintf("%q|%q|%q|%q|%q|%v|%q|%q", scan.Root, scan.Repo, scan.Branch, scan.Target, scan.MergeBase,
			scan.InProgress, scan.GitDir, scan.CommonDir),
		files: fmt.Sprintf("%q|%q", scan.Files, scan.AllFiles),
		state: state.String(),
	}, stable
}

// hashFile is the sha256 of the content of the file at path.
func hashFile(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	h.Sum(sum[:0])
	return sum, nil
}

// takeFailure is a workstream's failed Take: the scan it ran on, when, and
// its error.
type takeFailure struct {
	scan gitscan.Snapshot
	at   time.Time
	msg  string
}

type repoStore struct {
	store    *snapshot.Store // nil: stage 2 is disabled for the repo
	lastGC   time.Time       // last gc that worked
	gcFailed time.Time       // last gc that failed, since the last one that worked
	altErr   string          // last SetAlternates error logged
	gcErr    string          // last gc error logged
	pruneErr string          // last index prune error logged
}

type pairKey struct{ a, b core.WorkstreamID }

func keyOf(p core.Pair) pairKey { return pairKey{p.Sides[0].Workstream, p.Sides[1].Workstream} }

// pairResult is a pair's check, the snapshots it was computed from and how
// long it took.
type pairResult struct {
	a, b  snapshot.Snap
	files []textual.FileConflict
	ms    int64
}

// pairFailure is a pair's failed check: the snapshots it ran on, when, and
// its error.
type pairFailure struct {
	a, b snapshot.Snap
	at   time.Time
	msg  string
}

func (det *detector) run(ctx context.Context) {
	start := time.Now()
	det.cost = runCost{}
	d := det.d
	if d.forgetUploads.Swap(false) {
		// The stream connected: serve answers again, and may have
		// restarted without the bundles.
		clear(det.uploaded)
		det.serveDown = time.Time{}
	}
	d.mu.Lock()
	overlaps := d.overlapsLocked()
	scans := make(map[core.WorkstreamID]gitscan.Snapshot, len(d.worktrees))
	for _, wt := range d.worktrees {
		if wt.snap != nil {
			scans[wt.ws] = *wt.snap
		}
	}
	d.mu.Unlock()

	var pairs []core.Pair
	byRepo := map[string][]core.Pair{}
	remote := false
	for _, p := range core.CandidatePairs(overlaps, d.id) {
		if !det.scanned(p, scans) {
			continue
		}
		pairs = append(pairs, p)
		byRepo[p.Repo] = append(byRepo[p.Repo], p)
		remote = remote || det.remote(p)
	}
	repos := slices.Sorted(maps.Keys(byRepo))
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		det.runRepo(ctx, repo, byRepo[repo], scans)
	}
	if ctx.Err() != nil {
		return
	}
	n := det.publish(pairs, scans)
	if remote {
		// The local pairs are checked and published by now: serve, slow
		// or gone, holds up none of them.
		for _, repo := range repos {
			if ctx.Err() != nil {
				return
			}
			det.runRemote(ctx, repo, byRepo[repo], scans)
		}
		if ctx.Err() != nil {
			return
		}
		n = det.publish(pairs, scans)
	}
	det.forget(pairs, scans) // first: gc keeps the peers still held
	det.gc(ctx, scans)
	// What a run costs is measured in B, Takes that wrote the same commit
	// too; a run that took nothing and checked nothing is noise.
	c := det.cost
	level := slog.LevelDebug
	if c.takes > 0 || c.checks > 0 {
		level = slog.LevelInfo
	}
	d.log.Log(ctx, level, "detection done", "pairs", len(pairs), "takes", c.takes, "take_ms", c.takeMS,
		"checks", c.checks, "check_ms", c.checkMS, "conflicts", n, "ms", time.Since(start).Milliseconds())
}

// scanned reports whether each side of p that is this daemon's has a scan.
// A side of another daemon's comes from serve.
func (det *detector) scanned(p core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot) bool {
	for _, s := range p.Sides {
		if _, ok := scans[s.Workstream]; s.Daemon == det.d.id && !ok {
			return false
		}
	}
	return true
}

// runRepo snapshots this daemon's workstreams of repo's pairs and checks the
// local pairs; the remote ones are runRemote's.
func (det *detector) runRepo(ctx context.Context, repo string, pairs []core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot) {
	rs := det.open(ctx, repo)
	if rs == nil || rs.store == nil {
		return
	}
	var objects []string
	for _, s := range scans {
		if s.Repo == repo {
			objects = append(objects, filepath.Join(s.CommonDir, "objects"))
		}
	}
	if err := rs.store.SetAlternates(objects); err != nil {
		if msg := err.Error(); msg != rs.altErr {
			det.d.log.Warn("set alternates failed", "repo", repo, "err", err)
			rs.altErr = msg
		}
		return
	}
	rs.altErr = ""
	var wss []core.WorkstreamID
	for _, p := range pairs {
		for _, s := range p.Sides {
			if s.Daemon == det.d.id {
				wss = append(wss, s.Workstream)
			}
		}
	}
	slices.Sort(wss)
	for _, ws := range slices.Compact(wss) {
		det.take(ctx, rs.store, ws, scans[ws])
	}
	for _, p := range pairs {
		if !det.remote(p) {
			det.checkPair(ctx, rs.store, p, scans)
		}
	}
}

// open returns the store of repo, opening and probing it the first time. A
// store that does not open, or whose git exits with an error on the probe,
// disables stage 2 for the repo, logged once. A probe cut short (a timeout)
// says nothing about git: the store stays unopened and the next run probes
// again.
func (det *detector) open(ctx context.Context, repo string) *repoStore {
	if rs, ok := det.repos[repo]; ok {
		return rs
	}
	st, err := snapshot.Open(det.stateDir, repo)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, det.d.probeTimeout)
		err = st.Probe(pctx)
		timedOut := pctx.Err() != nil
		cancel()
		if ctx.Err() != nil {
			return nil // shutting down: says nothing about git
		}
		if err != nil && (timedOut || !gitExited(err)) {
			if timedOut {
				err = fmt.Errorf("probe timed out after %s", det.d.probeTimeout)
			}
			if msg := err.Error(); msg != det.probeErr[repo] {
				det.d.log.Warn("store probe failed", "repo", repo, "err", err)
				det.probeErr[repo] = msg
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return nil // shutting down: says nothing about git
	}
	delete(det.probeErr, repo)
	rs := &repoStore{store: st}
	if err != nil {
		det.d.log.Warn("stage 2 disabled", "repo", repo, "err", err)
		rs.store = nil
	}
	det.repos[repo] = rs
	return rs
}

// take snapshots ws into st. A worktree half-way through a merge, rebase or
// cherry-pick keeps its previous snapshot, and so does one whose Take
// fails. A failed Take is not tried again on the same scan for
// d.checkRetry: one that times out would hold up every pair on every run.
func (det *detector) take(ctx context.Context, st *snapshot.Store, ws core.WorkstreamID, scan gitscan.Snapshot) {
	if f, ok := det.takeFailed[ws]; ok && reflect.DeepEqual(f.scan, scan) && time.Since(f.at) < det.d.checkRetry {
		return
	}
	key, stable := fingerprint(scan, det.hashBudget)
	if _, ok := det.snaps[ws]; ok && det.taken[ws] == key {
		return
	}
	start := time.Now()
	tctx, cancel := context.WithTimeout(ctx, scanTimeout)
	snap, err := st.Take(tctx, string(ws), scan)
	timedOut := tctx.Err() != nil
	cancel()
	ms := time.Since(start).Milliseconds()
	switch {
	case errors.Is(err, snapshot.ErrInProgress):
		return
	case err != nil:
		if ctx.Err() != nil {
			return
		}
		det.cost.takes++
		det.cost.takeMS += ms
		if timedOut {
			err = fmt.Errorf("timed out after %s", scanTimeout)
		}
		msg := err.Error()
		if msg != det.takeFailed[ws].msg {
			det.d.log.Warn("snapshot failed", "ws", ws, "err", err)
		}
		det.takeFailed[ws] = takeFailure{scan: scan, at: time.Now(), msg: msg}
		return
	}
	det.cost.takes++
	det.cost.takeMS += ms
	delete(det.takeFailed, ws)
	// A file written while the Take read it may be in the snapshot either
	// way: only a key that held from before the Take to after it vouches for
	// the snapshot. Else a file that goes back to what the first fingerprint
	// read would skip every Take, and keep the content in between for good.
	if after, ok := fingerprint(scan, det.hashBudget); stable && ok && after == key {
		det.taken[ws] = key
	} else {
		delete(det.taken, ws) // a fingerprint that cannot be trusted: the next run takes again
	}
	if prev, ok := det.snaps[ws]; !ok || prev.Commit != snap.Commit {
		det.d.log.Info("snapshot", "ws", ws, "commit", short(snap.Commit), "ms", ms)
	}
	det.snaps[ws] = snap
}

// snapOf is the snapshot of side s a check runs on: this daemon's own, or
// the one imported from serve.
func (det *detector) snapOf(s core.Side) (snapshot.Snap, bool) {
	if s.Daemon == det.d.id {
		snap, ok := det.snaps[s.Workstream]
		return snap, ok
	}
	snap, ok := det.peers[s.Workstream]
	return snap, ok
}

// checkPair checks p unless its two snapshots are the ones its last result
// came from. A failed check keeps the last result, and is not tried
// again on the same snapshots for d.checkRetry: a check that times out
// would hold up every later pair on every run. One that misses objects,
// which the user's repo can drop from under a snapshot that borrows them,
// first takes this daemon's sides again and retries once. A side of another
// daemon's is not taken here: it is forgotten, so the next run downloads it
// whole, and the pair waits for it.
func (det *detector) checkPair(ctx context.Context, st *snapshot.Store, p core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot) {
	k := keyOf(p)
	a, okA := det.snapOf(p.Sides[0])
	b, okB := det.snapOf(p.Sides[1])
	if !okA || !okB {
		return
	}
	if r, ok := det.results[k]; ok && r.a == a && r.b == b {
		return
	}
	if f, ok := det.failed[k]; ok && f.a == a && f.b == b && time.Since(f.at) < det.d.checkRetry {
		return
	}
	res, ms, err := det.check(ctx, st.Dir, a, b)
	if err != nil && ctx.Err() == nil && missingObjects(err) {
		// The skip would make these Takes no-ops: the objects the last one
		// wrote are gone, not the worktree.
		for _, s := range p.Sides {
			if s.Daemon == det.d.id {
				delete(det.taken, s.Workstream)
				det.take(ctx, st, s.Workstream, scans[s.Workstream])
			} else {
				delete(det.peers, s.Workstream)
			}
		}
		a, okA = det.snapOf(p.Sides[0])
		b, okB = det.snapOf(p.Sides[1])
		if !okA || !okB {
			return // the last result stands until the remote side is back
		}
		res, ms, err = det.check(ctx, st.Dir, a, b)
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		msg := err.Error()
		if msg != det.failed[k].msg {
			det.d.log.Warn("check failed", "ws_a", k.a, "ws_b", k.b, "err", err)
		}
		det.failed[k] = pairFailure{a: a, b: b, at: time.Now(), msg: msg}
		return
	}
	delete(det.failed, k)
	det.results[k] = pairResult{a: a, b: b, files: res.Files, ms: ms}
}

// check merges the in-flight changes of two snapshots over the files both
// change, within checkTimeout, and adds its time to the run's cost.
func (det *detector) check(ctx context.Context, gitDir string, a, b snapshot.Snap) (textual.Result, int64, error) {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	res, err := checkSnaps(cctx, gitDir, a, b)
	if err != nil && ctx.Err() == nil && cctx.Err() != nil {
		// A canceled git says "signal: interrupt" or "signal: killed", not why.
		err = fmt.Errorf("timed out after %s", checkTimeout)
	}
	ms := time.Since(start).Milliseconds()
	det.cost.checks++
	det.cost.checkMS += ms
	return res, ms, err
}

func checkSnaps(ctx context.Context, gitDir string, a, b snapshot.Snap) (textual.Result, error) {
	changedA, err := textual.Changed(ctx, gitDir, a.Base, a.Commit)
	if err != nil {
		return textual.Result{}, err
	}
	changedB, err := textual.Changed(ctx, gitDir, b.Base, b.Commit)
	if err != nil {
		return textual.Result{}, err
	}
	var shared []string
	for _, p := range changedA {
		if _, ok := slices.BinarySearch(changedB, p); ok {
			shared = append(shared, p)
		}
	}
	if len(shared) == 0 {
		return textual.Result{Status: textual.Clean}, nil
	}
	return textual.Check(ctx, gitDir, textual.Side{Base: a.Base, Head: a.Commit}, textual.Side{Base: b.Base, Head: b.Commit}, shared)
}

// gitExited reports whether err is git exiting on its own with a status,
// not git killed, interrupted or not started.
func gitExited(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.Exited()
}

func missingObjects(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "missing") || strings.Contains(msg, "bad object")
}

// publish rebuilds the conflicts from the results of the current pairs,
// hands them to the daemon, its metrics with the run's scans, and to serve
// when there is one, and logs what appeared and what went away. It returns
// how many there are.
func (det *detector) publish(pairs []core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot) int {
	next := map[string]core.Conflict{}
	checkMS := map[string]int64{} // of the check that found each conflict
	for _, p := range pairs {
		r, ok := det.results[keyOf(p)]
		if !ok {
			continue
		}
		for _, fc := range r.files {
			id := core.ConflictID(p.Repo, fc.Path, p.Sides[0].Workstream, p.Sides[1].Workstream)
			next[id] = core.Conflict{ID: id, Repo: p.Repo, Path: fc.Path, Kind: fc.Kind, Sides: p.Sides, Lines: fc.Lines,
				Text: fc.Text, TextMore: fc.TextMore}
			checkMS[id] = r.ms
		}
	}
	det.d.mu.Lock()
	det.d.conflicts = next
	for id, c := range next {
		det.d.spans[id] = c
	}
	det.d.syncMetricsLocked(next, scans)
	det.d.mu.Unlock()
	det.d.reportConflicts()
	for _, id := range slices.Sorted(maps.Keys(next)) {
		if _, ok := det.current[id]; !ok {
			c := next[id]
			det.d.log.Info("conflict detected", "id", id, "path", c.Path, "kind", c.Kind, "lines", formatLines(c.Lines[0]),
				"branch_a", c.Sides[0].Branch, "branch_b", c.Sides[1].Branch, "check_ms", checkMS[id])
		}
	}
	for _, id := range slices.Sorted(maps.Keys(det.current)) {
		if _, ok := next[id]; !ok {
			det.d.log.Info("conflict resolved", "id", id, "path", det.current[id].Path)
		}
	}
	det.current = next
	return len(next)
}

// gc prunes and collects each open store that is due, on the detector's
// clock, keeping the snapshots of the repo's tracked workstreams and the
// peers the detector holds, and logs how long a gc that ran took. A store
// whose gc failed waits gcRetry before the next try. Indexes left unpruned
// do not fail the gc: they are logged once per message and tried again on
// the next gc.
func (det *detector) gc(ctx context.Context, scans map[core.WorkstreamID]gitscan.Snapshot) {
	now := det.now()
	peers := det.heldPeers()
	for _, repo := range slices.Sorted(maps.Keys(det.repos)) {
		rs := det.repos[repo]
		if rs.store == nil || ctx.Err() != nil || (!rs.gcFailed.IsZero() && now.Sub(rs.gcFailed) < gcRetry) {
			continue
		}
		var keep []string
		for ws, s := range scans {
			if s.Repo == repo {
				keep = append(keep, string(ws))
			}
		}
		start := time.Now()
		gctx, cancel := context.WithTimeout(ctx, gcTimeout)
		ran, pruneErr, err := gcStore(gctx, rs.store, rs.lastGC, now, keep, peers)
		timedOut := gctx.Err() != nil
		cancel()
		switch {
		case ctx.Err() != nil:
		case err != nil:
			if timedOut {
				err = fmt.Errorf("timed out after %s", gcTimeout)
			}
			if msg := err.Error(); msg != rs.gcErr {
				det.d.log.Warn("store gc failed", "repo", repo, "err", err)
				rs.gcErr = msg
			}
			rs.gcFailed = now
		case ran:
			det.d.log.Info("store gc", "repo", repo, "ms", time.Since(start).Milliseconds())
			rs.lastGC, rs.gcFailed, rs.gcErr = now, time.Time{}, ""
			msg := ""
			if pruneErr != nil {
				if msg = pruneErr.Error(); msg != rs.pruneErr {
					det.d.log.Warn("index prune failed", "repo", repo, "err", pruneErr)
				}
			}
			rs.pruneErr = msg
		}
	}
}

// gcStore runs st's gc when it is due, after pruning the snapshot refs of
// workstreams not in keep and the peer refs of those not in peers, and after
// it prunes the copied indexes. It reports whether the gc ran, the index
// prune's error apart from the gc's.
func gcStore(ctx context.Context, st *snapshot.Store, last, now time.Time, keep, peers []string) (ran bool, pruneErr, err error) {
	loose, packs, err := st.Counts(ctx)
	if err != nil {
		return false, nil, err
	}
	if !needGC(last, now, loose, packs) {
		return false, nil, nil
	}
	if err := st.Prune(ctx, keep); err != nil {
		return false, nil, err
	}
	if err := st.PrunePeers(ctx, peers); err != nil {
		return false, nil, err
	}
	if err := st.GC(ctx); err != nil {
		return true, nil, err
	}
	return true, st.PruneIndexes(keep), nil
}

// forget drops the snapshots of workstreams no longer tracked, the results
// of pairs no longer current, and what the remote side keeps of workstreams
// in none of them.
func (det *detector) forget(pairs []core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot) {
	maps.DeleteFunc(det.snaps, func(ws core.WorkstreamID, _ snapshot.Snap) bool { _, ok := scans[ws]; return !ok })
	maps.DeleteFunc(det.taken, func(ws core.WorkstreamID, _ takeKey) bool { _, ok := scans[ws]; return !ok })
	maps.DeleteFunc(det.takeFailed, func(ws core.WorkstreamID, _ takeFailure) bool { _, ok := scans[ws]; return !ok })
	maps.DeleteFunc(det.narrowed, func(ws core.WorkstreamID, _ narrowed) bool { _, ok := scans[ws]; return !ok })
	live := make(map[pairKey]bool, len(pairs))
	for _, p := range pairs {
		live[keyOf(p)] = true
	}
	maps.DeleteFunc(det.results, func(k pairKey, _ pairResult) bool { return !live[k] })
	maps.DeleteFunc(det.failed, func(k pairKey, _ pairFailure) bool { return !live[k] })
	det.forgetRemote(pairs)
}

// formatLines writes ranges as "3" for one line and "3-5" for several,
// joined with ",". A point between lines (Len 0) is written as its Start.
func formatLines(rs []core.Range) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.Len <= 1 {
			parts = append(parts, strconv.Itoa(r.Start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", r.Start, r.Start+r.Len-1))
		}
	}
	return strings.Join(parts, ",")
}
