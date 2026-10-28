package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/daemoncore"
	"github.com/joacofg/dugout/internal/snapshot"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	minBackoff  = 250 * time.Millisecond
	maxBackoff  = 5 * time.Second
	postTimeout = 5 * time.Second
	// snapTimeout bounds one bundle upload or download.
	snapTimeout = 30 * time.Second
	// streamIdle is how long a stream may stay silent before it is treated
	// as dead; serve pings every wire.StreamKeepAlive.
	streamIdle = 2 * wire.StreamKeepAlive
)

// ErrNoSnapshot is serve's 404: no bundle for that workstream, or none this
// daemon may see yet.
var ErrNoSnapshot = errors.New("no snapshot for that workstream")

// ErrNotRelayed is serve answering an escalation as a type it does not know:
// it predates escalations and handed it to nobody.
var ErrNotRelayed = errors.New("the server does not relay escalations: it is older than this dugout")

// StatusError is serve answering a bundle request with a status the uplink
// does not handle itself.
type StatusError struct {
	Op     string // "upload snapshot", "download snapshot", "get state" or "escalate"
	Status int
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: status %d", e.Op, e.Status) }

// Uplink keeps the daemon in sync with serve. It sends the full state when
// the warnings stream connects and every Resend, and on Notify only if
// the state changed. It hands each warning set the stream delivers to
// OnWarnings. What goes and when is its daemoncore.Outbox's call; the Uplink
// does the I/O.
type Uplink struct {
	Server string
	Daemon core.DaemonID
	// Epoch identifies this run of the daemon: every full state and conflict
	// set carries it with a growing seq, so serve ignores a stale one.
	// New takes it from statedir.NextEpoch, which grows with each start: the
	// stored epoch plus one, or the clock in milliseconds if that is more.
	// With 0 the posts carry no epoch and serve takes each one, in whatever
	// order it arrives.
	Epoch      uint64
	Token      string // the person's token for serve (the shared one without -state); empty sends none
	Resend     time.Duration
	Snapshot   func() core.FullState
	OnWarnings func([]core.Warning)
	// OnControl, when set, gets each control event of the stream: serve's
	// current word, on connect and whenever it changes. A serve that sends
	// none leaves it uncalled.
	OnControl func(wire.Control)
	// OnEscalation, when set, gets each escalation serve hands this daemon:
	// another daemon's agent took a conflict with this machine to both humans.
	OnEscalation func(wire.Escalation)
	// OnConnect, when set, runs each time the warnings stream connects; the
	// owner forgets what it uploaded, since serve may have restarted.
	OnConnect func()
	Log       *slog.Logger

	// jitter returns a value in [0, 1) that spreads the resend and reconnect
	// waits; nil means math/rand/v2. Set before Run: it is read
	// without a lock.
	jitter func() float64

	once      sync.Once
	notify    chan struct{}
	client    *http.Client
	snapCl    *http.Client // bundles, with the longer snapTimeout
	rejected  atomic.Bool  // serve answered 401 last time
	protoSeen atomic.Bool  // a protocol mismatch was logged already
	forbidden atomic.Int32 // what serve's last 403 meant: forbidNone, forbidHost or forbidOther
	streaming atomic.Bool  // the warnings stream is established
	tooOld    atomic.Bool  // serve answered 426: this daemon is older than it accepts; set until restart
	newer     atomic.Bool  // serve answered a protocol above ours

	mu    sync.Mutex
	out   daemoncore.Outbox
	queue []daemoncore.Send // stepped, not posted yet: what SetConflicts sends waits for Run
}

func (u *Uplink) authorize(req *http.Request) {
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
}

// What a 403 from serve meant.
const (
	forbidNone  int32 = iota
	forbidHost        // the Host is not on serve's -allow-host
	forbidOther       // the daemon_id is bound to another person
)

// otherPersonMsg is what serve's claim answers when the daemon_id belongs to
// another person.
const otherPersonMsg = "daemon_id of another person"

// noteResp is noteStatus for a response: it reads the first 512 bytes of the
// body of a 403, which tells the Host apart from a daemon_id of someone else.
// The caller still closes the body.
func (u *Uplink) noteResp(resp *http.Response) bool {
	u.noteProtocol(resp)
	var body string
	if resp.StatusCode == http.StatusForbidden {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		body = string(b)
	}
	return u.noteStatus(resp.StatusCode, body)
}

// noteStatus logs a refusal from serve once per change, not on every
// retry, and reports whether serve refused the request. A 401 is the token.
// A 403 is the Host (serve's -allow-host) and says nothing about the token,
// unless its body says the daemon_id is another person's. Only a 2xx clears
// any of these states. body matters only for a 403.
func (u *Uplink) noteStatus(status int, body string) bool {
	switch {
	case status == http.StatusUnauthorized:
		if !u.rejected.Swap(true) {
			u.Log.Warn("serve rejected the token", "server", u.Server, "env", wire.EnvToken, "token_set", u.Token != "")
		}
		return true
	case status == http.StatusForbidden:
		if strings.Contains(body, otherPersonMsg) {
			if u.forbidden.Swap(forbidOther) != forbidOther {
				u.Log.Warn("serve: this daemon_id belongs to another person", "server", u.Server,
					"hint", "delete the daemon_id file from this machine's state dir and restart the daemon, or ask the admin to revoke the token of the person who has it")
			}
		} else if u.forbidden.Swap(forbidHost) != forbidHost {
			u.Log.Warn("serve refused this host", "server", u.Server,
				"hint", "add the host of DUGOUT_SERVER to serve's -allow-host")
		}
		return true
	case status >= 200 && status < 300:
		if u.rejected.Swap(false) {
			u.Log.Info("serve accepted the token again", "server", u.Server)
		}
		u.forbidden.Store(forbidNone)
	}
	return false
}

// noteProtocol logs once per process that serve speaks another protocol
// version. A response without the header, or with 0, is an older serve
// and says nothing.
func (u *Uplink) noteProtocol(resp *http.Response) {
	theirs := protocolOf(resp)
	if theirs == 0 || theirs == wire.ProtocolVersion {
		return
	}
	if theirs > wire.ProtocolVersion {
		u.newer.Store(true)
	}
	if !u.protoSeen.Swap(true) {
		u.Log.Warn("protocol mismatch", "ours", wire.ProtocolVersion, "theirs", theirs, "server", u.Server)
	}
}

// TooOld reports that serve refused this daemon's protocol (426). Only a
// restart, on the updated binary, clears it.
func (u *Uplink) TooOld() bool { return u.tooOld.Load() }

// ServerNewer reports that serve spoke a protocol above this daemon's.
func (u *Uplink) ServerNewer() bool { return u.newer.Load() }

// protocolOf is the protocol version serve answered resp with, 0 for none.
func protocolOf(resp *http.Response) int {
	n, _ := strconv.Atoi(resp.Header.Get(wire.HeaderProtocol))
	return n
}

func (u *Uplink) init() {
	u.once.Do(func() {
		u.notify = make(chan struct{}, 1)
		u.client = &http.Client{Timeout: postTimeout}
		u.snapCl = &http.Client{Timeout: snapTimeout}
		u.out = daemoncore.NewOutbox(u.Daemon, u.Epoch)
		if u.Log == nil {
			u.Log = slog.Default()
		}
	})
}

// Notify wakes the uplink soon: the current state goes if it changed, with
// whatever else waits. It never blocks; several calls before the wake
// coalesce into one.
func (u *Uplink) Notify() {
	u.init()
	select {
	case u.notify <- struct{}{}:
	default:
	}
}

// Run posts and streams until ctx is done. Each wake hands the outbox the
// current state first, which goes only when it changed.
func (u *Uplink) Run(ctx context.Context) {
	u.init()
	if wire.InsecureServer(u.Server) {
		u.Log.Warn("server without TLS", "url", u.Server, "hint", "the token travels in the clear; use an https:// DUGOUT_SERVER")
	}
	connected := make(chan struct{}, 1)
	go u.streamLoop(ctx, connected)
	// Each resend wait is drawn anew, so daemons do not hit serve in lockstep.
	// A Resend of 0 or less sends no periodic resend (a Ticker would panic).
	var tickC <-chan time.Time
	var timer *time.Timer
	if u.Resend > 0 {
		timer = time.NewTimer(jittered(u.Resend, u.jitterFactor()))
		defer timer.Stop()
		tickC = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-connected:
			if u.OnConnect != nil {
				u.OnConnect()
			}
			u.flush(ctx, u.current(), daemoncore.Connected{})
		case <-u.notify:
			u.flush(ctx, u.current())
		case <-tickC:
			u.flush(ctx, u.current(), daemoncore.Tick{})
			timer.Reset(jittered(u.Resend, u.jitterFactor()))
		}
	}
}

// jitterFactor draws from the injected source, or math/rand/v2.
func (u *Uplink) jitterFactor() float64 {
	if u.jitter != nil {
		return u.jitter()
	}
	return rand.Float64()
}

// jittered scales d by 0.8 + 0.4*f, so f in [0, 1) gives [0.8d, 1.2d). A d of
// zero or less stays zero.
func jittered(d time.Duration, f float64) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.8 + 0.4*f))
}

// backoffWait is the jittered reconnect wait, never above maxBackoff.
func (u *Uplink) backoffWait(backoff time.Duration) time.Duration {
	return min(jittered(backoff, u.jitterFactor()), maxBackoff)
}

func (u *Uplink) current() daemoncore.Event { return daemoncore.SetState{State: u.Snapshot()} }

// SetConflicts hands the daemon's current conflict set to the outbox, which
// sends it when it differs from the last one given, or is the first.
func (u *Uplink) SetConflicts(cs []core.Conflict) {
	u.init()
	u.mu.Lock()
	var sends []daemoncore.Send
	u.out, sends = daemoncore.Step(u.out, daemoncore.SetConflicts{Conflicts: cs})
	u.queue = append(u.queue, sends...)
	u.mu.Unlock()
	if len(sends) > 0 {
		u.Notify()
	}
}

// AddOutcomes hands the records of conflicts that ended to the outbox, which
// sends them until serve takes them.
func (u *Uplink) AddOutcomes(rs []core.Record) {
	u.init()
	u.mu.Lock()
	var sends []daemoncore.Send
	u.out, sends = daemoncore.Step(u.out, daemoncore.AddOutcomes{Records: rs})
	u.queue = append(u.queue, sends...)
	u.mu.Unlock()
	if len(sends) > 0 {
		u.Notify()
	}
}

// step feeds events to the outbox and takes every send waiting, the ones
// SetConflicts and AddOutcomes queued first.
func (u *Uplink) step(events ...daemoncore.Event) []daemoncore.Send {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, e := range events {
		var sends []daemoncore.Send
		u.out, sends = daemoncore.Step(u.out, e)
		u.queue = append(u.queue, sends...)
	}
	sends := u.queue
	u.queue = nil
	return sends
}

// flush steps events and posts what the outbox says to, feeding each result
// back, until nothing waits.
func (u *Uplink) flush(ctx context.Context, events ...daemoncore.Event) {
	for sends := u.step(events...); len(sends) > 0 && ctx.Err() == nil; {
		var results []daemoncore.Event
		for _, s := range newest(sends) {
			results = append(results, daemoncore.Sent{Kind: s.Kind, Seq: s.Seq, OK: u.post(ctx, s)})
		}
		sends = u.step(results...)
	}
}

// newest keeps the last send of each kind, the state first: each carries the
// whole state, set or records waiting, so an older one has nothing for serve
// that the newer lacks, and its result would not count.
func newest(sends []daemoncore.Send) []daemoncore.Send {
	var out []daemoncore.Send
	for _, k := range []daemoncore.Kind{daemoncore.KindState, daemoncore.KindConflicts, daemoncore.KindOutcomes} {
		if i := lastOf(sends, k); i >= 0 {
			out = append(out, sends[i])
		}
	}
	return out
}

func lastOf(sends []daemoncore.Send, k daemoncore.Kind) int {
	for i := len(sends) - 1; i >= 0; i-- {
		if sends[i].Kind == k {
			return i
		}
	}
	return -1
}

func (u *Uplink) post(ctx context.Context, s daemoncore.Send) bool {
	switch s.Kind {
	case daemoncore.KindConflicts:
		return u.send(ctx, wire.Message{Type: wire.TypeConflicts,
			FullState: core.FullState{Daemon: u.Daemon, Epoch: s.Epoch, Seq: s.Seq}, Conflicts: s.Conflicts, Stances: stancesOf(s.Conflicts)})
	case daemoncore.KindOutcomes:
		return u.send(ctx, wire.Message{Type: wire.TypeOutcomes,
			FullState: core.FullState{Daemon: u.Daemon, Epoch: s.Epoch, Seq: s.Seq}, Outcomes: s.Outcomes})
	}
	return u.send(ctx, wire.Message{Type: wire.TypeFullState, FullState: s.State})
}

// send posts one message to serve and reports whether serve took it.
func (u *Uplink) send(ctx context.Context, msg wire.Message) bool {
	if u.tooOld.Load() {
		return false // serve would refuse it again
	}
	msg.ProtocolVersion = wire.ProtocolVersion
	body, err := json.Marshal(msg)
	if err != nil {
		u.Log.Error("encode "+msg.Type, "err", err)
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Server+"/v1/msg", bytes.NewReader(body))
	if err != nil {
		u.Log.Error("build "+msg.Type+" request", "err", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	u.authorize(req)
	resp, err := u.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			u.Log.Warn("post "+msg.Type, "err", err)
		}
		return false
	}
	refused := u.noteResp(resp)
	if resp.StatusCode == http.StatusUpgradeRequired {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if !u.tooOld.Swap(true) {
			u.Log.Error("serve refuses this dugout as too old", "server", u.Server, "body", strings.TrimSpace(string(body)))
		}
		return false
	}
	resp.Body.Close()
	if refused {
		return false
	}
	if resp.StatusCode != http.StatusNoContent {
		u.Log.Warn("post "+msg.Type, "status", resp.StatusCode)
		return false
	}
	return true
}

// Escalate hands e to serve for the other side's daemon, within ctx, and
// reports whether a stream of that daemon took it. A serve that predates
// escalations is ErrNotRelayed; one that refuses it, a StatusError.
func (u *Uplink) Escalate(ctx context.Context, e wire.Escalation) (bool, error) {
	u.init()
	if u.tooOld.Load() {
		return false, errors.New("the server refuses this dugout as too old")
	}
	body, err := json.Marshal(wire.Message{Type: wire.TypeEscalation, FullState: core.FullState{Daemon: u.Daemon}, Escalation: &e,
		ProtocolVersion: wire.ProtocolVersion})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Server+"/v1/msg", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	u.authorize(req)
	resp, err := u.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	u.noteResp(resp)
	switch resp.StatusCode {
	case http.StatusNoContent:
		return false, ErrNotRelayed
	case http.StatusOK:
	default:
		return false, &StatusError{Op: "escalate", Status: resp.StatusCode}
	}
	var r wire.Relayed
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&r); err != nil {
		return false, err
	}
	return r.Delivered, nil
}

func (u *Uplink) snapshotURL(ws core.WorkstreamID) string {
	return u.Server + "/v1/snapshot?ws=" + url.QueryEscape(string(ws)) + "&daemon_id=" + url.QueryEscape(string(u.Daemon))
}

// PutSnapshot uploads the bundle of workstream ws, which must be this
// daemon's own.
func (u *Uplink) PutSnapshot(ctx context.Context, ws core.WorkstreamID, snap snapshot.Snap, bundle []byte) error {
	u.init()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.snapshotURL(ws), bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", wire.BundleType)
	req.Header.Set(wire.HeaderCommit, snap.Commit)
	req.Header.Set(wire.HeaderBase, snap.Base)
	u.authorize(req)
	resp, err := u.snapCl.Do(req)
	if err != nil {
		return err
	}
	u.noteResp(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return &StatusError{Op: "upload snapshot", Status: resp.StatusCode}
	}
	return nil
}

// GetSnapshot downloads the bundle of workstream ws. With have set to the
// commit already held, serve answers notModified and no bundle. A missing
// bundle is ErrNoSnapshot.
func (u *Uplink) GetSnapshot(ctx context.Context, ws core.WorkstreamID, have string) (snap snapshot.Snap, bundle []byte, notModified bool, err error) {
	u.init()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.snapshotURL(ws), nil)
	if err != nil {
		return snap, nil, false, err
	}
	if have != "" {
		req.Header.Set("If-None-Match", `"`+have+`"`)
	}
	u.authorize(req)
	resp, err := u.snapCl.Do(req)
	if err != nil {
		return snap, nil, false, err
	}
	defer resp.Body.Close()
	u.noteResp(resp)
	switch resp.StatusCode {
	case http.StatusNotModified:
		return snap, nil, true, nil
	case http.StatusNotFound:
		return snap, nil, false, ErrNoSnapshot
	case http.StatusOK:
	default:
		return snap, nil, false, &StatusError{Op: "download snapshot", Status: resp.StatusCode}
	}
	bundle, err = io.ReadAll(io.LimitReader(resp.Body, wire.MaxBundle+1))
	if err != nil {
		return snap, nil, false, err
	}
	if len(bundle) > wire.MaxBundle {
		return snap, nil, false, errors.New("download snapshot: bundle over the limit")
	}
	snap = snapshot.Snap{Commit: resp.Header.Get(wire.HeaderCommit), Base: resp.Header.Get(wire.HeaderBase)}
	return snap, bundle, false, nil
}

// State fetches serve's whole state (GET /v1/state), within ctx: every
// workstream serve knows, with their intents.
func (u *Uplink) State(ctx context.Context) (wire.StateDump, error) {
	u.init()
	var dump wire.StateDump
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.Server+"/v1/state", nil)
	if err != nil {
		return dump, err
	}
	u.authorize(req)
	resp, err := u.client.Do(req)
	if err != nil {
		return dump, err
	}
	defer resp.Body.Close()
	u.noteResp(resp)
	if resp.StatusCode != http.StatusOK {
		return dump, &StatusError{Op: "get state", Status: resp.StatusCode}
	}
	if err := json.NewDecoder(resp.Body).Decode(&dump); err != nil {
		return wire.StateDump{}, err
	}
	return dump, nil
}

// Connected reports whether the warnings stream is established right now.
func (u *Uplink) Connected() bool { return u.streaming.Load() }

func (u *Uplink) streamLoop(ctx context.Context, connected chan<- struct{}) {
	backoff := minBackoff
	for ctx.Err() == nil {
		established, err := u.stream(ctx, connected)
		if ctx.Err() != nil {
			return
		}
		if established {
			backoff = minBackoff
		}
		u.Log.Debug("warnings stream ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.backoffWait(backoff)):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// stream holds one SSE connection until it ends. established reports
// whether the server accepted it.
func (u *Uplink) stream(ctx context.Context, connected chan<- struct{}) (established bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		u.Server+"/v1/stream?daemon_id="+url.QueryEscape(string(u.Daemon)), nil)
	if err != nil {
		return false, err
	}
	u.authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if u.noteResp(resp) || resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stream status %d", resp.StatusCode)
	}
	u.streaming.Store(true)
	defer u.streaming.Store(false)
	select {
	case connected <- struct{}{}:
	default:
	}

	idle := time.AfterFunc(streamIdle, cancel)
	defer idle.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	event := ""
	for sc.Scan() {
		idle.Reset(streamIdle)
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "warnings":
			var ws []core.Warning
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ws); err != nil {
				u.Log.Warn("bad warnings event", "err", err)
				continue
			}
			u.OnWarnings(ws)
		case strings.HasPrefix(line, "data: ") && event == "escalation":
			var e wire.Escalation
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				u.Log.Warn("bad escalation event", "err", err)
				continue
			}
			if u.OnEscalation != nil {
				u.OnEscalation(e)
			}
		case strings.HasPrefix(line, "data: ") && event == "control":
			var c wire.Control
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
				u.Log.Warn("bad control event", "err", err)
				continue
			}
			if u.OnControl != nil {
				u.OnControl(c)
			}
		case line == "":
			event = ""
		}
	}
	return true, sc.Err()
}

// stancesOf is the stances of the conflicts in cs that have one, by ID, for
// the conflicts message; nil when none has.
func stancesOf(cs []core.Conflict) map[string][2]core.Stance {
	var out map[string][2]core.Stance
	for _, c := range cs {
		if c.Stances != ([2]core.Stance{}) {
			if out == nil {
				out = map[string][2]core.Stance{}
			}
			out[c.ID] = c.Stances
		}
	}
	return out
}

// noticeTimeout bounds one office-view notice to serve.
const noticeTimeout = 2 * time.Second

// Message tells serve, for the office view, that a session of m.From
// messaged one of m.To. Best effort: one try, off the caller's path.
func (u *Uplink) Message(m wire.AgentMessage) {
	u.notice(wire.Message{Type: wire.TypeAgentMessage, AgentMessage: &m})
}

// Escalated tells serve, for the office view, of an escalation this daemon
// raised. Best effort, as Message.
func (u *Uplink) Escalated(n wire.EscalationNotice) {
	u.notice(wire.Message{Type: wire.TypeEscalated, Escalated: &n})
}

// notice posts msg once in the background. A refusal or a serve that is
// down loses it: the view shows less, and nothing else depends on it.
func (u *Uplink) notice(msg wire.Message) {
	u.init()
	if u.Server == "" || u.tooOld.Load() {
		return
	}
	msg.Daemon, msg.ProtocolVersion = u.Daemon, wire.ProtocolVersion
	go func() {
		body, err := json.Marshal(msg)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Server+"/v1/msg", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		u.authorize(req)
		resp, err := u.client.Do(req)
		if err != nil {
			u.Log.Debug("post "+msg.Type, "err", err)
			return
		}
		u.noteResp(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			u.Log.Debug("post "+msg.Type, "status", resp.StatusCode)
		}
	}()
}
