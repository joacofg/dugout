// Package daemon is Dugout's per-machine process. It scans the worktrees
// that sessions registered, keeps serve in sync through an Uplink,
// and answers hooks over a unix socket from what it already has in memory,
// so a hook never waits on git or the network.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/judge"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	DefaultPoll   = 10 * time.Second
	DefaultResend = 60 * time.Second

	connDeadline = time.Second
	rootTimeout  = 100 * time.Millisecond
	// repoTimeout bounds asking git for a repo's identity while a new
	// worktree's first scan runs, within the hook's budget.
	repoTimeout = 50 * time.Millisecond
	scanTimeout = 30 * time.Second
	maxRequest  = 2 << 20
	// whoTimeout bounds serve's answer to a who, so the MCP tool that asks
	// still answers within its second.
	whoTimeout = 600 * time.Millisecond
)

// Config configures a Daemon. Zero Poll and Resend take the defaults, and an
// empty Socket the one in StateDir.
type Config struct {
	StateDir string
	Socket   string // where hooks reach the daemon (statedir.SocketPath)
	Server   string
	// Insecure lets Server be an http:// URL outside this machine, where the
	// token and the bundles travel in the clear; without it New refuses one.
	Insecure bool
	Token    string // shared token for serve
	Poll     time.Duration
	Resend   time.Duration
	Log      *slog.Logger
	// Notify shows an escalation on this machine's desktop; nil is
	// DesktopNotify.
	Notify Notifier
	// Judge compares the declared intents of the repo's workstreams for
	// duplicate work; nil turns duplicate detection off, and JudgeOff says
	// why ("" for judge.ErrNotConfigured).
	Judge    judge.Judge
	JudgeOff string
	// MetricsGrace is how long an ended conflict may come back as the same
	// one before its metrics are final (metrics.go); 0 is
	// DefaultMetricsGrace.
	MetricsGrace time.Duration
}

type worktree struct {
	root     string
	snap     *gitscan.Snapshot // last good scan
	ws       core.WorkstreamID
	intent   string // last declared by a session attached here, sanitized; kept after the session ends
	repo     string // the repo's identity asked of git before the first scan, for its mode
	lastErr  string
	scanning bool
	again    bool
}

// Daemon is one machine's daemon. Create it with New and start it with Run.
type Daemon struct {
	cfg    Config
	id     core.DaemonID
	log    *slog.Logger
	uplink *Uplink
	ctx    context.Context

	kick         chan struct{}                          // detector runs pending, at most one
	dupKick      chan struct{}                          // duplicate detector runs pending, at most one
	save         chan struct{}                          // worktrees.json writes pending, at most one
	saveMu       sync.Mutex                             // one worktrees.json write at a time, each of the latest set
	saveRoots    func(dir string, roots []string) error // writes worktrees.json; tests slow it
	checkRetry   time.Duration                          // wait before checking a failed pair's same snapshots, or taking a failed scan, again; tests shorten it
	probeTimeout time.Duration                          // bound on a store's git probe; tests shorten it
	clock        func() time.Time                       // the detector's wall clock; tests set it

	forgetUploads atomic.Bool   // the stream connected: serve may have lost every bundle
	paused        atomic.Bool   // serve's team pause: hooks answer nothing
	pauses        atomic.Uint64 // pauses begun, so a session is told once per pause

	localMu     sync.Mutex
	localPaused bool // the paused file was there at the last hook

	mu        sync.Mutex
	worktrees map[string]*worktree
	rejected  map[string]bool // roots that cannot be a workstream; only `dugout track` retries them
	sessions  map[string]*Session
	warnings  []core.Warning           // latest set from serve
	lastState []byte                   // last full state handed to the uplink
	local     core.State               // this daemon's own core, fed its full state
	conflicts map[string]core.Conflict // by ID, as the detector last found them
	dupes     map[string]Duplicate     // by ID, as the duplicate detector last found them
	modes     map[string]Mode          // by repo id, as modes.json has them; never written in place

	modeMu sync.Mutex // one mode change at a time, each saved before it takes effect

	// stances is the office registry: by conflict ID, what each of its
	// workstreams said of its span (office_claim, office_yield).
	stances map[string]map[core.WorkstreamID]core.Stance
	// spans is every conflict the detector published, by ID, while both its
	// workstreams are this machine's and live, so a stance can name one that
	// resolved since.
	spans map[string]core.Conflict

	// The escalations (escalate.go). warned is, by conflict ID, when a
	// session of each of its sides on this machine was first handed it; seen,
	// when the escalation check first found it standing; escalated, the
	// conflicts that went to the humans while they stand; escs, what went
	// there, for the sessions to hear of.
	warned    map[string]map[core.WorkstreamID]time.Time
	seen      map[string]time.Time
	escalated map[string]bool
	escs      []*escalation

	judgeMu sync.Mutex
	stats   JudgeStats // what the judge has cost since the start

	// The agent metrics (metrics.go): the conflicts standing and ended
	// within the grace, the latest ended past it, and all of those by repo.
	metrics *core.Ledger
	ended   []core.Record
	totals  map[string]core.Totals
}

// WorkstreamIDFor names the workstream of (root, branch) on daemon d.
func WorkstreamIDFor(d core.DaemonID, root, branch string) core.WorkstreamID {
	h := fnv.New64a()
	h.Write([]byte(root + "\x00" + branch))
	id := strconv.FormatUint(h.Sum64(), 16)
	return core.WorkstreamID(string(d) + ":" + strings.Repeat("0", 16-len(id)) + id)
}

// New prepares the state dir, loads or creates the daemon_id and the
// tracked worktrees, and takes the run's epoch from the state dir.
func New(cfg Config) (*Daemon, error) {
	if cfg.Socket == "" {
		cfg.Socket = statedir.Socket(cfg.StateDir)
	}
	if sock := cfg.Socket; len(sock) > statedir.MaxSocketPath {
		return nil, fmt.Errorf("socket path too long (%d bytes, maximum %d): %s; use a shorter DUGOUT_HOME",
			len(sock), statedir.MaxSocketPath, sock)
	}
	if wire.InsecureServer(cfg.Server) && !cfg.Insecure {
		return nil, fmt.Errorf("server %s: %w", cfg.Server, wire.ErrInsecureServer)
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Resend <= 0 {
		cfg.Resend = DefaultResend
	}
	if cfg.Notify == nil {
		cfg.Notify = DesktopNotify
	}
	if cfg.MetricsGrace <= 0 {
		cfg.MetricsGrace = DefaultMetricsGrace
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if err := statedir.Ensure(cfg.StateDir); err != nil {
		return nil, err
	}
	id, created, err := statedir.LoadOrCreateID(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if created {
		log.Info("new daemon_id", "daemon_id", id)
	}
	// An epoch that cannot be persisted fails the start, as a daemon_id does.
	epoch, reset, err := statedir.NextEpoch(cfg.StateDir, time.Now())
	if err != nil {
		return nil, fmt.Errorf("epoch of this run: %w", err)
	}
	if reset {
		log.Warn("epoch file unreadable, epoch from the clock", "state_dir", cfg.StateDir, "epoch", epoch)
	}
	roots, err := statedir.LoadWorktrees(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	modes := map[string]Mode{}
	stored, err := statedir.LoadModes(cfg.StateDir)
	if err != nil {
		// A mode is a preference: without it every repo takes the default.
		log.Warn("modes unreadable, every repo in the default mode", "err", err)
	}
	for repo, name := range stored {
		if m, err := ParseMode(name); err == nil {
			modes[repo] = m
		}
	}
	d := &Daemon{cfg: cfg, id: id, log: log, kick: make(chan struct{}, 1), dupKick: make(chan struct{}, 1), save: make(chan struct{}, 1), saveRoots: statedir.SaveWorktrees, checkRetry: checkRetry, probeTimeout: checkTimeout, clock: time.Now, worktrees: map[string]*worktree{}, rejected: map[string]bool{},
		sessions: map[string]*Session{}, local: core.NewState(), conflicts: map[string]core.Conflict{}, dupes: map[string]Duplicate{}, stances: map[string]map[core.WorkstreamID]core.Stance{},
		spans: map[string]core.Conflict{}, modes: modes, warned: map[string]map[core.WorkstreamID]time.Time{}, seen: map[string]time.Time{}, escalated: map[string]bool{},
		metrics: core.NewLedger(core.Millis(cfg.MetricsGrace.Milliseconds())), totals: map[string]core.Totals{}}
	for _, r := range roots {
		d.worktrees[r] = &worktree{root: r}
	}
	// Built even without a server, so a test can set one before Run.
	d.uplink = &Uplink{Server: cfg.Server, Daemon: id, Epoch: epoch, Token: cfg.Token, Resend: cfg.Resend, Snapshot: d.fullState, OnWarnings: d.setWarnings, OnControl: d.setControl,
		OnEscalation: func(e wire.Escalation) { go d.takeRelayed(e) }, Log: log}
	// A serve that restarted has none of the bundles: the next run uploads
	// them again.
	d.uplink.OnConnect = func() { d.forgetUploads.Store(true); d.kickDetector() }
	return d, nil
}

// setControl takes serve's control event. Scanning, detecting and the uplink
// go on while paused, so nothing is missing when it ends. Only the start of a
// pause counts as a new one; serve repeats the value on every connect.
func (d *Daemon) setControl(c wire.Control) {
	if c.InjectionOff {
		// Only the stream's goroutine writes, so this check is not racy; the
		// count goes first so a hook that sees the pause sees its number.
		if !d.paused.Load() {
			d.pauses.Add(1)
			d.paused.Store(true)
			d.log.Info("team pause on")
		}
		return
	}
	if d.paused.Swap(false) {
		d.log.Info("team pause off")
	}
}

// pausedNow reports whether hooks are paused, by serve or by this machine's
// paused file, and which text tells the session: the team's wins. The file is
// stat'ed at every hook. A local pause that begins while the team's is on is
// not a new one: the sessions were told already.
func (d *Daemon) pausedNow() (paused, team bool) {
	_, err := os.Stat(statedir.PausedPath(d.cfg.StateDir))
	local := err == nil
	d.localMu.Lock()
	if local != d.localPaused {
		d.localPaused = local
		if local && !d.paused.Load() {
			d.pauses.Add(1)
		}
	}
	d.localMu.Unlock()
	team = d.paused.Load()
	return team || local, team
}

// ID is the persisted daemon_id.
func (d *Daemon) ID() core.DaemonID { return d.id }

// Run serves the socket, scans, detects and syncs until ctx is done. With no
// server it stays on this machine: the uplink never starts.
func (d *Daemon) Run(ctx context.Context) error {
	sock := d.cfg.Socket
	if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("dugout daemon is already running (%s)", sock)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	defer os.Remove(sock)
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	d.ctx = ctx
	d.log.Info("daemon listening", "socket", sock, "daemon_id", d.id, "server", d.cfg.Server)
	if d.cfg.Judge != nil {
		d.log.Info("duplicate detection on", "judge", d.cfg.Judge.Name())
	}

	// Run returns only after the detector, which runs git in the state dir
	// and the worktrees, has stopped.
	detectCtx, stopDetector := context.WithCancel(ctx)
	detectDone := make(chan struct{})
	go func() { defer close(detectDone); d.detectLoop(detectCtx) }()
	defer func() { stopDetector(); <-detectDone }()
	dupDone := make(chan struct{})
	go func() { defer close(dupDone); d.dupLoop(detectCtx) }()
	defer func() { stopDetector(); <-dupDone }()
	// Nor before the last worktrees.json write a hook asked for has landed.
	saveDone := make(chan struct{})
	go func() { defer close(saveDone); d.saveLoop(ctx) }()
	defer func() { <-saveDone }()

	// Hooks get answers right away, but serve hears from this daemon only
	// after a first scan: an empty full state would clear the other side's
	// warnings until the scan ends.
	withServer := d.cfg.Server != ""
	go func() {
		d.initialScan()
		if withServer {
			go d.uplink.Run(ctx)
		}
		d.pollLoop(ctx)
	}()
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serveConn(conn)
	}
}

// initialScan scans every tracked worktree once, in parallel, and waits.
func (d *Daemon) initialScan() {
	d.mu.Lock()
	wts := make([]*worktree, 0, len(d.worktrees))
	for _, wt := range d.worktrees {
		wts = append(wts, wt)
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, wt := range wts {
		wg.Add(1)
		go func() { defer wg.Done(); d.scanOne(wt) }()
	}
	wg.Wait()
	d.notifyIfChanged()
	d.kickDetector()
	d.kickDuplicates()
}

func (d *Daemon) pollLoop(ctx context.Context) {
	tick := time.NewTicker(d.cfg.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.pruneSessions()
			d.pruneStances()
			d.checkEscalations()
			d.settleMetrics()
			d.scanAll()
		}
	}
}

func (d *Daemon) scanAll() {
	d.mu.Lock()
	roots := make([]string, 0, len(d.worktrees))
	for r := range d.worktrees {
		roots = append(roots, r)
	}
	d.mu.Unlock()
	for _, r := range roots {
		d.triggerScan(r)
	}
}

// pruneSessions drops sessions whose Claude Code process is gone.
func (d *Daemon) pruneSessions() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, s := range d.sessions {
		if s.PID > 0 && errors.Is(syscall.Kill(s.PID, 0), syscall.ESRCH) {
			delete(d.sessions, id)
		}
	}
}

// triggerScan scans root in the background, at most one scan at a time per
// worktree; a trigger during a scan runs one more scan after it. Each scan's
// result is handed on when that scan ends, not when the scans stop: while
// scans outlast the poll interval, or a busy session's hooks keep coming, a
// trigger is always waiting and they never stop.
func (d *Daemon) triggerScan(root string) {
	d.mu.Lock()
	wt := d.worktrees[root]
	if wt == nil {
		d.mu.Unlock()
		return
	}
	if wt.scanning {
		wt.again = true
		d.mu.Unlock()
		return
	}
	wt.scanning = true
	d.mu.Unlock()
	go func() {
		for {
			d.scanOne(wt)
			d.notifyIfChanged()
			d.kickDetector() // the files may have changed even if the list did not
			d.mu.Lock()
			if !wt.again {
				wt.scanning = false
				d.mu.Unlock()
				return
			}
			wt.again = false
			d.mu.Unlock()
		}
	}()
}

func (d *Daemon) scanOne(wt *worktree) {
	if _, err := os.Stat(wt.root); errors.Is(err, fs.ErrNotExist) {
		d.log.Info("worktree gone, untracking", "root", wt.root)
		d.untrack(wt.root)
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, scanTimeout)
	defer cancel()
	snap, err := gitscan.Scan(ctx, wt.root)
	if d.ctx.Err() != nil {
		return // shutting down: a canceled scan says nothing about the worktree
	}
	if errors.Is(err, gitscan.ErrNotRepo) || errors.Is(err, gitscan.ErrNoRepoID) {
		d.log.Warn("not trackable, untracking", "root", wt.root, "err", err)
		d.mu.Lock()
		d.rejected[wt.root] = true
		d.mu.Unlock()
		d.untrack(wt.root)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if msg := err.Error(); msg != wt.lastErr && d.ctx.Err() == nil {
			d.log.Warn("scan failed", "root", wt.root, "err", err)
			wt.lastErr = msg
		}
		return
	}
	wt.lastErr = ""
	if snap.ConfigErr != "" && (wt.snap == nil || wt.snap.ConfigErr != snap.ConfigErr) {
		d.log.Warn("repo config", "root", wt.root, "err", snap.ConfigErr)
	}
	wt.snap = &snap
	wt.ws = WorkstreamIDFor(d.id, wt.root, snap.Branch)
}

// track adds root and persists the list. It reports whether root is new.
func (d *Daemon) track(root string) bool {
	if !d.addWorktree(root) {
		return false
	}
	d.saveWorktrees()
	return true
}

// addWorktree tracks root in memory and reports whether it is new.
func (d *Daemon) addWorktree(root string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.worktrees[root]; ok {
		return false
	}
	d.worktrees[root] = &worktree{root: root}
	return true
}

// saveWorktreesSoon asks saveLoop for a worktrees.json write and returns at
// once: a hook's whole answer has 150 ms, and the write's two fsyncs
// can take that on a busy disk.
func (d *Daemon) saveWorktreesSoon() {
	select {
	case d.save <- struct{}{}:
	default:
	}
}

// saveLoop writes worktrees.json when asked, until ctx is done, and then once
// more if a write is still pending.
func (d *Daemon) saveLoop(ctx context.Context) {
	for {
		select {
		case <-d.save:
			d.saveWorktrees()
		case <-ctx.Done():
			select {
			case <-d.save:
				d.saveWorktrees()
			default:
			}
			return
		}
	}
}

func (d *Daemon) untrack(root string) {
	d.mu.Lock()
	delete(d.worktrees, root)
	for id, s := range d.sessions {
		if s.Root == root {
			delete(d.sessions, id)
		}
	}
	d.mu.Unlock()
	d.saveWorktrees()
	d.notifyIfChanged()
}

func (d *Daemon) saveWorktrees() {
	d.saveMu.Lock()
	defer d.saveMu.Unlock()
	d.mu.Lock()
	roots := make([]string, 0, len(d.worktrees))
	for r := range d.worktrees {
		roots = append(roots, r)
	}
	d.mu.Unlock()
	if err := d.saveRoots(d.cfg.StateDir, roots); err != nil {
		d.log.Error("save worktrees", "err", err)
	}
}

// fullState is every scanned worktree with files in flight.
func (d *Daemon) fullState() core.FullState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fullStateLocked()
}

func (d *Daemon) fullStateLocked() core.FullState {
	fs := core.FullState{Daemon: d.id, Workstreams: []core.Workstream{}}
	for _, wt := range d.worktrees {
		if wt.snap == nil || len(wt.snap.Files) == 0 {
			continue
		}
		fs.Workstreams = append(fs.Workstreams, core.Workstream{
			ID: wt.ws, Repo: wt.snap.Repo, Branch: wt.snap.Branch, Files: slices.Clone(wt.snap.Files), Intent: wt.intent, Person: wt.snap.Person,
		})
	}
	slices.SortFunc(fs.Workstreams, func(a, b core.Workstream) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return fs
}

// notifyIfChanged feeds a changed full state to the local core and asks the
// uplink to send it. Both under one lock, so a slower caller with an older
// state cannot overwrite a newer one.
func (d *Daemon) notifyIfChanged() {
	d.mu.Lock()
	fs := d.fullStateLocked()
	b, err := json.Marshal(fs)
	changed := err == nil && !bytes.Equal(b, d.lastState)
	if changed {
		d.lastState = b
		d.local, _ = core.Apply(d.local, fs)
	}
	d.mu.Unlock()
	if changed {
		d.uplink.Notify()
		d.kickDuplicates()
	}
}

func (d *Daemon) setWarnings(ws []core.Warning) {
	d.mu.Lock()
	known := make(map[string]bool, len(d.warnings))
	for _, w := range d.warnings {
		known[w.ID] = true
	}
	d.warnings = ws
	d.mu.Unlock()
	for _, w := range ws {
		if !known[w.ID] {
			d.log.Info("warning received", "id", w.ID, "path", w.Path)
		}
	}
	d.kickDetector()
}

// logDelivered records each overlap, conflict and duplicate a hook answer
// handed to the agent, to time how long a warning takes to reach it.
func (d *Daemon) logDelivered(out map[string]any, event, session string, handed View) {
	hso, _ := out["hookSpecificOutput"].(map[string]any)
	denied := hso["permissionDecision"] == "deny"
	for _, w := range handed.Overlaps {
		d.log.Info("warning delivered", "id", w.ID, "kind", "overlap", "session", session, "event", event, "deny", denied)
	}
	for _, c := range handed.Conflicts {
		d.log.Info("warning delivered", "id", c.ID, "kind", "conflict", "session", session, "event", event, "deny", denied)
	}
	for _, dup := range handed.Duplicates {
		d.log.Info("warning delivered", "id", dup.ID, "kind", "duplicate", "session", session, "event", event, "deny", denied)
	}
}

func (d *Daemon) serveConn(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(connDeadline))
	line, err := bufio.NewReader(&limitedReader{conn, maxRequest}).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var resp wire.Response
	var req wire.Request
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = "bad request: " + err.Error()
	} else {
		resp = d.handle(req)
	}
	b, _ := json.Marshal(resp)
	conn.SetWriteDeadline(time.Now().Add(connDeadline)) // after handle: a track scans synchronously
	conn.Write(append(b, '\n'))
}

type limitedReader struct {
	c net.Conn
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("request too large")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.c.Read(p)
	l.n -= n
	return n, err
}

func (d *Daemon) handle(req wire.Request) wire.Response {
	switch req.Type {
	case wire.TypeHook:
		return d.handleHook(req)
	case wire.TypeTrack:
		return d.handleTrack(req)
	case wire.TypeIntent:
		return d.handleIntent(req)
	case wire.TypeWho:
		return d.handleWho(req)
	case wire.TypeStatus:
		return d.handleStatus()
	case wire.TypeMode:
		return d.handleMode(req)
	case wire.TypeOffice:
		return d.handleOffice(req)
	case wire.TypeClaim:
		return d.handleStance(req, core.StanceClaim)
	case wire.TypeYield:
		return d.handleStance(req, core.StanceYield)
	case wire.TypeEscalate:
		return d.handleEscalate(req)
	default:
		return wire.Response{Error: "unknown request type: " + req.Type}
	}
}

type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		To           string `json:"to"` // a SendMessage's address
	} `json:"tool_input"`
	// EditHash stands for what an edit's PreToolUse would write, as the hook
	// hashed it; the edit itself never reaches the daemon.
	EditHash string `json:"edit_hash"`
}

var emptyOutput = json.RawMessage("{}")

func (d *Daemon) handleHook(req wire.Request) wire.Response {
	var in hookInput
	if err := json.Unmarshal(req.Input, &in); err != nil || in.SessionID == "" {
		return wire.Response{Output: emptyOutput}
	}
	d.mu.Lock()
	s := d.sessions[in.SessionID]
	d.mu.Unlock()
	if req.Event == "SessionEnd" {
		d.mu.Lock()
		delete(d.sessions, in.SessionID)
		d.mu.Unlock()
		return wire.Response{Output: emptyOutput}
	}
	if s == nil {
		// New session, or one that outlived a daemon restart: attach it
		// to the worktree of its cwd.
		ctx, cancel := context.WithTimeout(d.ctx, rootTimeout)
		root, err := gitscan.Root(ctx, in.Cwd)
		cancel()
		if err != nil {
			return wire.Response{Output: emptyOutput}
		}
		d.mu.Lock()
		rejected := d.rejected[root]
		d.mu.Unlock()
		if rejected {
			return wire.Response{Output: emptyOutput}
		}
		s = NewSession(in.SessionID, root, req.ClaudePID)
		s.ClaudeDir = req.ClaudeDir
		// Registered at another hook, after a daemon restart or a SessionStart
		// the daemon missed: the session was never asked for its intent, or
		// the daemon forgot it.
		s.unasked = req.Event != "SessionStart"
		d.mu.Lock()
		if existing := d.sessions[in.SessionID]; existing != nil {
			s = existing
		} else {
			d.sessions[in.SessionID] = s
		}
		d.mu.Unlock()
		// A worktree with an intent but nothing in flight is compared once a
		// session is attached.
		d.kickDuplicates()
		// A restart before the write lands loses nothing: the session's
		// next hook attaches root again.
		if d.addWorktree(root) {
			d.saveWorktreesSoon()
		}
	}
	d.triggerScan(s.Root)

	path := in.ToolInput.FilePath
	if path == "" {
		path = in.ToolInput.NotebookPath
	}
	rel := relPath(s.Root, path)
	d.mu.Lock()
	if req.Event == "SessionStart" && req.ClaudePID > 0 {
		s.PID = req.ClaudePID
	}
	if req.ClaudeDir != "" {
		s.ClaudeDir = req.ClaudeDir
	}
	var ws core.WorkstreamID
	if wt := d.worktrees[s.Root]; wt != nil {
		ws = wt.ws
	}
	d.mu.Unlock()
	mode := d.modeFor(s.Root)
	s.mu.Lock()
	s.Mode = mode
	if paused, team := d.pausedNow(); paused {
		// Nothing is handed over, so nothing is marked delivered or told:
		// when the pause ends, what appeared meanwhile is told at the next hook.
		out := emptyOutput
		if n := d.pauses.Load(); s.pausedNoted != n {
			s.pausedNoted = n
			out = json.RawMessage(`{"systemMessage":"Dugout is paused on this machine."}`)
			if team {
				out = json.RawMessage(`{"systemMessage":"Dugout is paused for the whole team."}`)
			}
		}
		s.mu.Unlock()
		return wire.Response{Output: out}
	}
	var v View
	if ws != "" {
		v = d.viewFor(ws)
	}
	edit := req.Event == "PreToolUse" && editTools[in.ToolName] && rel != ""
	if edit {
		d.heldBefore(s, rel, in.EditHash)
	}
	answer, handed := d.answerHook(req.Event, in.ToolName, rel, s, ws, v)
	if hso, _ := answer["hookSpecificOutput"].(map[string]any); edit && hso["permissionDecision"] == "deny" {
		d.heldNow(s, rel, in.EditHash, handed.Conflicts)
	}
	if len(handed.Conflicts) > 0 {
		d.mu.Lock()
		d.noteWarnedLocked(ws, handed.Conflicts, d.clock())
		d.mu.Unlock()
	}
	news, human, terminal := d.escalationNews(req.Event, s, ws)
	addNews(answer, req.Event, news, human)
	d.sessionNotices(s, answer)
	s.mu.Unlock()
	if req.Event == "PostToolUse" && in.ToolName == "SendMessage" {
		d.messaged(ws, in.ToolInput.To)
	}
	d.logDelivered(answer, req.Event, s.ID, handed)
	out, err := json.Marshal(answer)
	if err != nil {
		return wire.Response{Output: emptyOutput}
	}
	return wire.Response{Output: out, Terminal: terminal}
}

// sessionNotices adds to answer's systemMessage, once per session, that this
// dugout is older than serve accepts or than serve itself. Callers
// hold s.mu and have ruled out a pause.
func (d *Daemon) sessionNotices(s *Session, answer map[string]any) {
	var lines []string
	if cur, _ := answer["systemMessage"].(string); cur != "" {
		lines = append(lines, cur)
	}
	if d.uplink.TooOld() && !s.tooOldNoted {
		s.tooOldNoted = true
		lines = append(lines, "Dugout: your dugout is older than the server; update it (dugout doctor tells you how).")
	}
	if d.uplink.ServerNewer() && !d.uplink.TooOld() && !s.newerNoted {
		s.newerNoted = true
		lines = append(lines, "Dugout: the server is newer than your dugout; update it when you can.")
	}
	if len(lines) > 0 {
		answer["systemMessage"] = strings.Join(lines, "\n")
	}
}

// relPath returns p relative to root with symlinks in its directory
// resolved, or "" when p is empty or outside root.
func relPath(root, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(dir, filepath.Base(p))
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (d *Daemon) handleTrack(req wire.Request) wire.Response {
	ctx, cancel := context.WithTimeout(d.ctx, scanTimeout)
	defer cancel()
	snap, err := gitscan.Scan(ctx, req.Path)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	d.mu.Lock()
	delete(d.rejected, snap.Root)
	d.mu.Unlock()
	d.track(snap.Root)
	d.mu.Lock()
	if wt := d.worktrees[snap.Root]; wt != nil {
		wt.snap = &snap
		wt.ws = WorkstreamIDFor(d.id, snap.Root, snap.Branch)
	}
	d.mu.Unlock()
	d.notifyIfChanged()
	out, _ := json.Marshal(map[string]string{"root": snap.Root})
	return wire.Response{Output: out}
}

// handleIntent stores a session's declared intent on its worktree, so it
// goes to serve with the full state and reaches the other workstreams. Only a
// session the daemon registered may declare.
func (d *Daemon) handleIntent(req wire.Request) wire.Response {
	text := SanitizeIntent(req.Text)
	d.mu.Lock()
	var wt *worktree
	if s := d.sessions[req.SessionID]; s != nil {
		wt = d.worktrees[s.Root]
	}
	if wt == nil { // a session is dropped with its worktree; nil only while a hook is still tracking it
		d.mu.Unlock()
		return wire.Response{Error: "Dugout does not know that session: use the session_id Dugout gave you at the start."}
	}
	if strings.TrimSpace(text) == "" {
		d.mu.Unlock()
		return wire.Response{Error: "the intent is empty"}
	}
	// A subagent runs with its parent's session_id, so its declaration
	// overwrites the session's intent; the worktree keeps whichever came last.
	wt.intent = text
	ws := wt.ws
	d.mu.Unlock()
	d.notifyIfChanged()
	d.kickDuplicates() // the full state leaves out a worktree with nothing in flight
	d.log.Info("intent declared", "session", req.SessionID, "workstream", ws)
	return wire.Response{Output: json.RawMessage(`{"ok":true}`)}
}

// handleWho tells who else has req.Path in flight: the other workstreams of
// the repo of the tracked worktree that holds it, the deepest one when
// worktrees nest. This daemon's own come from its local core; with the uplink
// on, the other daemons' come from serve, asked within whoTimeout. The asking
// worktree's workstream is never one of them. While paused, by the team or
// this machine, it tells only that and asks serve nothing.
func (d *Daemon) handleWho(req wire.Request) wire.Response {
	if paused, team := d.pausedNow(); team {
		return wire.Response{Output: json.RawMessage(`{"tracked":true,"paused":"team"}`)}
	} else if paused {
		return wire.Response{Output: json.RawMessage(`{"tracked":true,"paused":"local"}`)}
	}
	root, rel := d.deepestRoot(req.Path)
	if root == "" {
		return wire.Response{Output: json.RawMessage(`{"tracked":false}`)}
	}
	d.mu.Lock()
	var repo string
	var own core.WorkstreamID
	if wt := d.worktrees[root]; wt != nil && wt.snap != nil {
		repo, own = wt.snap.Repo, wt.ws
	}
	local := d.local.Workstreams()
	d.mu.Unlock()

	out := wire.Who{Tracked: true, Server: "off", Touches: []wire.Touch{}}
	var remote []core.Workstream
	if d.cfg.Server != "" {
		ctx, cancel := context.WithTimeout(d.ctx, whoTimeout)
		dump, err := d.uplink.State(ctx)
		cancel()
		if err != nil {
			out.Server = "down"
			d.log.Warn("who without serve", "err", err)
		} else {
			out.Server, remote = "ok", dump.Workstreams
		}
	}
	type touch struct {
		id core.WorkstreamID
		wire.Touch
	}
	var found []touch
	seen := map[core.WorkstreamID]bool{own: true}
	add := func(daemon core.DaemonID, ws core.Workstream) {
		if seen[ws.ID] {
			return
		}
		seen[ws.ID] = true
		if repo != "" && ws.Repo == repo && slices.Contains(ws.Files, rel) {
			found = append(found, touch{ws.ID, wire.Touch{Branch: ws.Branch, Daemon: daemon, Intent: ws.Intent}})
		}
	}
	for _, ws := range local {
		add(d.id, ws)
	}
	for _, ws := range remote {
		// A workstream id is "<daemon_id>:..."; this daemon's own are in its
		// local core, fresher than serve's copy.
		if owner, _, _ := strings.Cut(string(ws.ID), ":"); core.DaemonID(owner) != d.id {
			add(core.DaemonID(owner), ws)
		}
	}
	slices.SortFunc(found, func(a, b touch) int { return strings.Compare(string(a.id), string(b.id)) })
	for _, t := range found {
		out.Touches = append(out.Touches, t.Touch)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	return wire.Response{Output: b}
}
