package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/snapshot"
)

// The remote side of stage 2. A pair with a workstream of another daemon is
// checked on that daemon's own snapshot, which serve relays as a git bundle:
// this daemon uploads its side when serve may not have its snapshot yet,
// downloads the other side when serve has a newer one, and fetches from
// origin a base the other side is on and the store does not have.

const (
	// baseFetchEvery is the least time between two base fetches of one repo.
	baseFetchEvery = time.Minute
	// serveBackoff is how long the remote side leaves serve be once a call
	// found it not answering, unless the stream connects again first.
	serveBackoff = time.Minute
)

// serveUp reports whether the remote side may call serve: not within
// serveBackoff, on the detector's clock, of a call that found serve not
// answering. A stream that connects again lifts the wait (run).
func (det *detector) serveUp() bool {
	return det.serveDown.IsZero() || det.now().Sub(det.serveDown) >= serveBackoff
}

// noteServe pauses the calls to serve (serveUp) when err, of one, is serve
// not answering: no connection or no answer in time, rather than an answer
// with a status.
func (det *detector) noteServe(err error) {
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		det.serveDown = det.now()
	}
}

// remote reports whether p has a side of another daemon.
func (det *detector) remote(p core.Pair) bool {
	return p.Sides[0].Daemon != det.d.id || p.Sides[1].Daemon != det.d.id
}

// runRemote syncs the remote pairs among pairs, all of repo, with serve and
// checks them. runRepo has taken this daemon's sides already. While serve
// does not answer, the pairs are checked on the snapshots the store has:
// each call that waits for it would hold up the next run's local pairs.
func (det *detector) runRemote(ctx context.Context, repo string, pairs []core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot) {
	rs := det.repos[repo]
	if rs == nil || rs.store == nil || rs.altErr != "" {
		return // runRepo could not get the store ready
	}
	// Each of this daemon's workstreams goes up with the paths of all its
	// remote pairs and no others, so one upload serves every peer and none
	// gets the rest of the change.
	shared := map[core.WorkstreamID][]string{}
	for _, p := range pairs {
		if det.remote(p) {
			for _, s := range p.Sides {
				if s.Daemon == det.d.id {
					shared[s.Workstream] = append(shared[s.Workstream], p.Files...)
				}
			}
		}
	}
	synced := map[core.WorkstreamID]bool{}
	for _, p := range pairs {
		if ctx.Err() != nil {
			return
		}
		if det.remote(p) {
			det.syncRemote(ctx, rs.store, p, scans, synced, shared)
			det.checkPair(ctx, rs.store, p, scans)
		}
	}
}

// syncRemote brings the sides of p, a remote pair, up to date with serve,
// each workstream once per run (synced): this daemon's goes up, the other
// daemon's comes down. Nothing goes either way while serve is not up.
func (det *detector) syncRemote(ctx context.Context, st *snapshot.Store, p core.Pair, scans map[core.WorkstreamID]gitscan.Snapshot,
	synced map[core.WorkstreamID]bool, shared map[core.WorkstreamID][]string) {
	mine, theirs := p.Sides[0], p.Sides[1]
	if mine.Daemon != det.d.id {
		mine, theirs = theirs, mine
	}
	if !synced[mine.Workstream] && det.serveUp() {
		synced[mine.Workstream] = true
		det.upload(ctx, st, mine.Workstream, shared[mine.Workstream])
	}
	if !synced[theirs.Workstream] && det.serveUp() {
		synced[theirs.Workstream] = true
		det.download(ctx, st, p.Repo, theirs.Workstream, scans[mine.Workstream])
	}
}

// refusal is a snapshot of this daemon's that serve will not take for a
// cause that lasts: a bundle too big, a full serve, or one too old to relay
// snapshots. Building and sending it again would only be refused
// again, so it waits for a new commit, or for checkRetry on the detector's
// clock.
type refusal struct {
	commit string
	at     time.Time
	msg    string // as logged, once per commit and message
}

// failedImport is a snapshot of another daemon's whose import failed: what
// serve announced, its bundle, when on the detector's clock, whether the
// store lacked its base, and how many base fetches of the repo had worked by
// then. Serve answers 304 for it from then on, so the bundle is kept to try
// again.
type failedImport struct {
	snap        snapshot.Snap
	bundle      []byte
	at          time.Time
	missingBase bool
	baseGot     int
}

// upload sends serve the snapshot of ws, a workstream of this daemon's,
// narrowed to paths (snapshot.Narrow), unless serve already has it: what
// goes up, and what is scanned for secrets, is only what its peers pair
// with. One that holds a secret is not sent, and
// serve keeps the last one that was: it is logged once per commit, with the
// path and the rule, never the value, at Warn: the other daemon checks a
// stale snapshot while it lasts. One that serve refuses for a cause that
// lasts waits (refusal). One that changes nothing is sent like any
// other: the other daemon's conflicts with what it changed go away.
func (det *detector) upload(ctx context.Context, st *snapshot.Store, ws core.WorkstreamID, paths []string) {
	full, ok := det.snaps[ws]
	if !ok {
		return
	}
	snap, err := det.narrow(ctx, st, ws, full, paths)
	if err != nil {
		if ctx.Err() == nil {
			det.syncFailed(ws, "snapshot narrowing failed", err)
		}
		return
	}
	if det.uploaded[ws] == snap.Commit || det.secretLogged[ws] == snap.Commit {
		return
	}
	if r, ok := det.refused[ws]; ok && r.commit == snap.Commit && det.now().Sub(r.at) < det.d.checkRetry {
		return
	}
	start := time.Now()
	gctx, cancel := context.WithTimeout(ctx, checkTimeout)
	path, rule, err := st.Secret(gctx, snap)
	var bundle []byte
	if err == nil && path == "" {
		bundle, err = st.Bundle(gctx, snap)
	}
	cancel()
	switch {
	case ctx.Err() != nil:
		return
	case path != "":
		det.d.log.Warn("snapshot not uploaded", "ws", ws, "path", path, "rule", rule)
		det.secretLogged[ws] = snap.Commit
		return
	case err == nil && len(bundle) > det.maxBundle:
		det.refuse(ws, snap, slog.LevelInfo, "snapshot too big to upload", "bytes", len(bundle), "limit", det.maxBundle)
		return
	case err == nil:
		uctx, cancel := context.WithTimeout(ctx, det.uplinkTimeout)
		err = det.d.uplink.PutSnapshot(uctx, ws, snap, bundle)
		cancel()
		if ctx.Err() != nil {
			return
		}
		det.noteServe(err)
		if det.refusedUpload(ws, snap, err) {
			return
		}
	}
	if err != nil {
		det.syncFailed(ws, "snapshot upload failed", err)
		return
	}
	delete(det.syncErr, ws)
	delete(det.refused, ws)
	det.uploaded[ws] = snap.Commit
	det.d.log.Info("snapshot uploaded", "ws", ws, "commit", short(snap.Commit), "bytes", len(bundle),
		"ms", time.Since(start).Milliseconds())
}

// narrowed is the last Narrow of a workstream for upload: of which snapshot
// and paths (key), and the snapshot it gave.
type narrowed struct {
	key  string
	snap snapshot.Snap
}

// narrow is st.Narrow of full to paths, once per snapshot and set of paths.
func (det *detector) narrow(ctx context.Context, st *snapshot.Store, ws core.WorkstreamID, full snapshot.Snap, paths []string) (snapshot.Snap, error) {
	paths = slices.Compact(slices.Sorted(slices.Values(paths)))
	key := full.Commit + "\x00" + strings.Join(paths, "\x00")
	if n, ok := det.narrowed[ws]; ok && n.key == key {
		return n.snap, nil
	}
	nctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	snap, err := st.Narrow(nctx, full, paths)
	if err != nil {
		return snapshot.Snap{}, err
	}
	det.narrowed[ws] = narrowed{key: key, snap: snap}
	return snap, nil
}

// refusedUpload reports whether err, of the upload of snap of ws, is serve
// refusing it for a cause that lasts, and records the refusal if so: a
// bundle over its limit, its storage full, or a serve that predates the
// relay, whose guard wants JSON (415) or whose mux lacks the route.
func (det *detector) refusedUpload(ws core.WorkstreamID, snap snapshot.Snap, err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Status {
	case http.StatusRequestEntityTooLarge:
		det.refuse(ws, snap, slog.LevelInfo, "snapshot too big to upload", "status", se.Status)
	case http.StatusInsufficientStorage:
		det.refuse(ws, snap, slog.LevelWarn, "serve snapshot storage full", "status", se.Status)
	case http.StatusUnsupportedMediaType, http.StatusNotFound, http.StatusMethodNotAllowed:
		det.refuse(ws, snap, slog.LevelInfo, "serve does not relay snapshots", "status", se.Status)
	default:
		return false
	}
	return true
}

// refuse records that snap of ws waits (refusal), and logs msg at level, with
// attrs, once per commit and message.
func (det *detector) refuse(ws core.WorkstreamID, snap snapshot.Snap, level slog.Level, msg string, attrs ...any) {
	if r := det.refused[ws]; r.commit != snap.Commit || r.msg != msg {
		det.d.log.Log(context.Background(), level, msg, append([]any{"ws", ws, "commit", short(snap.Commit)}, attrs...)...)
	}
	delete(det.syncErr, ws) // logged as the refusal instead
	det.refused[ws] = refusal{commit: snap.Commit, at: det.now(), msg: msg}
}

// download imports the snapshot serve has of ws, a workstream of another
// daemon's, when it is not the one the store has. While the other daemon has
// uploaded none, serve has none: the pair waits, which is not worth a log.
// A snapshot that did not import is asked for by its commit, so serve
// answers 304 while it holds no newer one, and its kept bundle is imported
// again only when that may work now (retryDue). scan is of this daemon's side
// of the pair, whose clone names the origin of a missing base.
func (det *detector) download(ctx context.Context, st *snapshot.Store, repo string, ws core.WorkstreamID, scan gitscan.Snapshot) {
	start := time.Now()
	have := det.peers[ws].Commit
	failed, retry := det.failedPeer[ws]
	if retry {
		have = failed.snap.Commit
	}
	uctx, cancel := context.WithTimeout(ctx, det.uplinkTimeout)
	snap, bundle, notModified, err := det.d.uplink.GetSnapshot(uctx, ws, have)
	cancel()
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, ErrNoSnapshot), err == nil && notModified && !retry:
		delete(det.syncErr, ws)
		return
	case err == nil && notModified:
		if !det.retryDue(ctx, st, repo, failed, scan) {
			return
		}
		snap, bundle = failed.snap, failed.bundle
	case err != nil:
		det.noteServe(err)
		det.syncFailed(ws, "snapshot download failed", err)
		return
	}
	err = det.importPeer(ctx, st, repo, ws, snap, bundle, scan)
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, snapshot.ErrMissingBase):
		if det.noBase[ws] != snap.Commit {
			det.d.log.Warn("no base", "ws", ws, "commit", short(snap.Commit))
			det.noBase[ws] = snap.Commit
		}
		det.failedPeer[ws] = failedImport{snap: snap, bundle: bundle, at: det.now(), missingBase: true, baseGot: det.baseGot[repo]}
		return
	case err != nil:
		det.syncFailed(ws, "snapshot download failed", err)
		det.failedPeer[ws] = failedImport{snap: snap, bundle: bundle, at: det.now(), baseGot: det.baseGot[repo]}
		return
	}
	delete(det.syncErr, ws)
	delete(det.noBase, ws)
	delete(det.failedPeer, ws)
	det.peers[ws] = snap
	det.d.log.Info("snapshot downloaded", "ws", ws, "commit", short(snap.Commit), "bytes", len(bundle),
		"ms", time.Since(start).Milliseconds())
}

// retryDue reports whether f, a snapshot of another daemon's that did not
// import and that serve still holds, may import now from its kept bundle: a
// base fetch of repo worked since it failed, or checkRetry passed on the
// detector's clock. For a missing base, a fetch runs here when one is due
// (fetchBase), since nothing else would ask for it.
func (det *detector) retryDue(ctx context.Context, st *snapshot.Store, repo string, f failedImport, scan gitscan.Snapshot) bool {
	if f.missingBase && det.fetchBase(ctx, st, repo, scan) {
		return true
	}
	return det.baseGot[repo] > f.baseGot || det.now().Sub(f.at) >= det.d.checkRetry
}

// importPeer imports bundle, snapshot snap of ws, into st. When the store
// lacks its base, the base is fetched from origin and the import tried once
// more.
func (det *detector) importPeer(ctx context.Context, st *snapshot.Store, repo string, ws core.WorkstreamID, snap snapshot.Snap, bundle []byte, scan gitscan.Snapshot) error {
	imp := func() error {
		ictx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		return st.Import(ictx, string(ws), bundle, snap)
	}
	err := imp()
	if errors.Is(err, snapshot.ErrMissingBase) && det.fetchBase(ctx, st, repo, scan) {
		err = imp()
	}
	return err
}

// fetchBase fetches into st the target branch of scan's worktree, from the
// remote its clone has for it, unless repo had a fetch less than
// baseFetchEvery ago on the detector's clock. It reports whether a fetch ran
// and worked. A failure is logged once per message.
func (det *detector) fetchBase(ctx context.Context, st *snapshot.Store, repo string, scan gitscan.Snapshot) bool {
	now := det.now()
	if last, ok := det.baseFetched[repo]; ok && now.Sub(last) < baseFetchEvery {
		return false
	}
	det.baseFetched[repo] = now
	start := time.Now()
	err := fetchTarget(ctx, st, scan)
	switch {
	case ctx.Err() != nil:
		return false
	case err != nil:
		if msg := err.Error(); msg != det.fetchErr[repo] {
			det.d.log.Warn("base fetch failed", "repo", repo, "err", err)
			det.fetchErr[repo] = msg
		}
		return false
	}
	delete(det.fetchErr, repo)
	det.baseGot[repo]++
	det.d.log.Info("base fetched", "repo", repo, "ms", time.Since(start).Milliseconds())
	return true
}

// fetchTarget fetches scan.Target's branch from the remote it names
// (origin/main: main from origin), at the URL scan's clone has for it, with
// the core.sshCommand the clone's config sets, its own or an includeIf's,
// which the fetch on the store would not see. Both are read in the clone,
// read-only.
func fetchTarget(ctx context.Context, st *snapshot.Store, scan gitscan.Snapshot) error {
	remote, branch, ok := strings.Cut(scan.Target, "/")
	if !ok || remote == "" || branch == "" {
		return fmt.Errorf("target %q is not a remote's branch", scan.Target)
	}
	gctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	url, err := gitscan.Run(gctx, scan.Root, "remote", "get-url", remote)
	if err != nil {
		return err
	}
	sshCmd, _ := gitscan.Run(gctx, scan.Root, "config", "--get", "core.sshCommand") // unset exits 1: none
	return st.FetchBase(ctx, strings.TrimSpace(url), branch, strings.TrimSpace(sshCmd))
}

// syncFailed logs an upload or download of ws that failed with err, at Warn,
// once per message until one works.
func (det *detector) syncFailed(ws core.WorkstreamID, msg string, err error) {
	if s := err.Error(); s != det.syncErr[ws] {
		det.d.log.Warn(msg, "ws", ws, "err", err)
		det.syncErr[ws] = s
	}
}

// heldPeers lists the workstreams whose snapshots the detector holds as
// peers: the peer refs a gc keeps. A workstream of another repo is no ref in
// a store, so one list serves every store.
func (det *detector) heldPeers() []string {
	out := make([]string, 0, len(det.peers))
	for ws := range det.peers {
		out = append(out, string(ws))
	}
	return out
}

// forgetRemote drops what the remote side keeps of workstreams in none of
// pairs.
func (det *detector) forgetRemote(pairs []core.Pair) {
	paired := map[core.WorkstreamID]bool{}
	for _, p := range pairs {
		paired[p.Sides[0].Workstream], paired[p.Sides[1].Workstream] = true, true
	}
	dropUnpaired(det.uploaded, paired)
	dropUnpaired(det.peers, paired)
	dropUnpaired(det.failedPeer, paired)
	dropUnpaired(det.noBase, paired)
	dropUnpaired(det.secretLogged, paired)
	dropUnpaired(det.refused, paired)
	dropUnpaired(det.syncErr, paired)
}

func dropUnpaired[V any](m map[core.WorkstreamID]V, paired map[core.WorkstreamID]bool) {
	maps.DeleteFunc(m, func(ws core.WorkstreamID, _ V) bool { return !paired[ws] })
}
