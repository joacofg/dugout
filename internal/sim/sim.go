// Package sim is a seeded, single-threaded simulation of the sync between
// daemons and serve. Each daemon drives the real daemoncore outbox;
// serve is core.Apply plus the last warnings pushed on each open stream and
// each daemon's conflict set; the network between them delays, reorders,
// duplicates and drops messages. Daemons get partitioned, restart and leave,
// and serve restarts. One PRNG makes every choice and nothing iterates a map
// unsorted, so a seed replays its run, trace and all. At the end Run turns
// the faults off, lets the system settle and checks the invariants.
package sim

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/daemoncore"
)

const (
	tickEvery   = 10   // steps between a daemon's ticks
	maxDelay    = 12   // steps a reordered message may take to land
	pChange     = 0.1  // per daemon and step: its workstreams change
	pNewSet     = 0.04 // per daemon and step: its conflict set changes
	settleTicks = 3    // ticks in a row once the faults stop

	// A restarting daemon is back sooner than a message's longest flight, so
	// a late message of the old run may land after the new run's, even on a
	// serve that never saw the old run. The epoch, one more each run as the
	// daemon persists it, tells them apart; random epochs, which carry
	// no order, let such a message replace the new run's state.
	downMin = 1
	downMax = 5
)

var (
	repos       = []string{"r1", "r2"}
	files       = []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}
	conflictIDs = []string{"c1", "c2", "c3", "c4"}
)

// Config sizes a run and sets how hostile it is. Each P is a chance: PDrop,
// PDup and PReorder per message, PPartition, PDaemonRestart and PDaemonLeave
// per daemon and step, PServeRestart per step.
type Config struct {
	Daemons, Steps                                                   int
	PDrop, PDup, PReorder, PPartition, PDaemonRestart, PServeRestart float64
	// PDaemonLeave is a daemon going away for good without an empty full
	// state: serve keeps its warnings until it restarts.
	PDaemonLeave float64
}

// DefaultConfig is the run TestSimSeeds repeats for each seed.
func DefaultConfig() Config {
	return Config{Daemons: 3, Steps: 300, PDrop: 0.1, PDup: 0.05, PReorder: 0.3,
		PPartition: 0.01, PDaemonRestart: 0.005, PServeRestart: 0.004, PDaemonLeave: 0.0005}
}

// Result is one run: its trace, one line per step, and the invariants it
// broke, each line starting with "invariant N:".
type Result struct {
	Trace      []string
	Violations []string
}

// Run simulates cfg with seed.
func Run(seed uint64, cfg Config) Result {
	return newWorld(seed, cfg).run()
}

// world is one run: the daemons, serve and the network between them.
type world struct {
	cfg    Config
	r      *rand.Rand
	faults bool // the hostile part is on; quiesce turns it off
	step   int

	daemons []*daemon
	serve   serve
	net     []message // in flight, by (at, n)
	sent    int       // messages put on the network so far, copies too
	made    int       // messages posted so far; a copy shares its original's

	// apply is serve's core.Apply, and admit the core.Clock.Admit it orders
	// conflict sets with; a test swaps in a broken one to see a run catch it.
	apply func(core.State, core.Event) (core.State, []core.Effect)
	admit func(core.Clock, uint64, uint64) (core.Clock, bool)

	violations []string      // found before the end, by invariants 4 and 7
	flagged    map[feed]bool // invariant 7 already broke for that feed
	trace      []string      // one line per step
	notes      []string      // what the current step did so far
}

// feed is one daemon's messages of one kind.
type feed struct {
	d    core.DaemonID
	kind daemoncore.Kind
}

// daemon is one daemon across its runs.
type daemon struct {
	id   core.DaemonID
	slot int // it ticks at the steps where step % tickEvery == slot

	ws  []core.Workstream // in flight now; replaced whole, never mutated
	set []core.Conflict   // its conflict set, sorted by ID; the same

	epoch uint64 // of the current run: 1 for the first, one more per restart
	out   daemoncore.Outbox

	up, gone  bool // the process runs; it left for good (and is not up)
	leftAt    int  // the step it left at
	upAt      int  // while down: the step it starts again
	connected bool // its warnings stream is open
	connectAt int  // while up and not connected: the step it reconnects
	healAt    int  // while partitioned: the step it heals; 0 otherwise
}

// serve is the office server as handleMsg runs it.
type serve struct {
	st       core.State
	streams  map[core.DaemonID][]core.Warning // per open stream, the last warnings pushed
	reported map[core.DaemonID][]string       // per daemon, its conflict IDs, sorted
	clocks   map[core.DaemonID]core.Clock     // per daemon, of its conflict sets
	landed   map[feed]message                 // per feed, the newest made that reached this serve
}

func newServe() serve {
	return serve{st: core.NewState(), streams: map[core.DaemonID][]core.Warning{},
		reported: map[core.DaemonID][]string{}, clocks: map[core.DaemonID]core.Clock{}, landed: map[feed]message{}}
}

// message is one Send on the network. It lands at step at or is lost there,
// and the sender's run hears the result then.
type message struct {
	at, n int // ties at a step go in the order they were sent
	made  int // the order it was posted in: the truth invariant 7 holds serve to
	from  *daemon
	send  daemoncore.Send // its Epoch is the sender's run, as the outbox stamps it
	lost  bool            // posted while partitioned: it fails
	copy  bool            // a duplicate: serve may take it again, the sender hears nothing
}

func (m message) String() string {
	s := fmt.Sprintf("%s.%d %s#%d", m.from.id, m.send.Epoch, m.send.Kind, m.send.Seq)
	if m.copy {
		s += " copy"
	}
	return s
}

func newWorld(seed uint64, cfg Config) *world {
	w := &world{cfg: cfg, r: rand.New(rand.NewPCG(seed, seed)), faults: true, serve: newServe(),
		apply: core.Apply, admit: core.Clock.Admit, flagged: map[feed]bool{}}
	for i := range cfg.Daemons {
		d := &daemon{id: core.DaemonID("d" + strconv.Itoa(i+1)), slot: i % tickEvery, upAt: 1}
		d.ws, d.set = w.randomWorkstreams(d.id), w.randomSet()
		w.daemons = append(w.daemons, d)
	}
	return w
}

func (w *world) run() Result {
	for w.step < w.cfg.Steps {
		w.step++
		w.stepOnce(func(d *daemon) bool { return w.step%tickEvery == d.slot })
	}
	w.quiesce()
	return Result{Trace: w.trace, Violations: w.check()}
}

// stepOnce is one step: the network delivers what is due, each daemon acts
// in order, ticking when tick says so, and serve may restart. What happened
// becomes the step's trace line.
func (w *world) stepOnce(tick func(*daemon) bool) {
	w.deliver()
	for _, d := range w.daemons {
		w.act(d, tick(d))
	}
	if w.faults && w.chance(w.cfg.PServeRestart) {
		w.restartServe()
	}
	line := "-"
	if len(w.notes) > 0 {
		line = strings.Join(w.notes, "; ")
	}
	w.trace = append(w.trace, fmt.Sprintf("%03d %s", w.step, line))
	w.notes = w.notes[:0]
}

// quiesce turns the faults off and lets the system settle for the checks.
// Every partition heals and every daemon not gone comes back and reconnects.
// Then serve restarts once more, while messages of the run may still be in
// flight and land after the daemons' resends, as a stale one may on a real
// network: every daemon reconnects, the network drains with no tick, and 1,
// 2 and 6 must hold again (invariant 4), so Connected alone brings serve
// back. Last, each daemon ticks settleTicks times in a row and the network
// drains for the final check.
func (w *world) quiesce() {
	w.faults = false
	for _, d := range w.daemons {
		if d.healAt != 0 {
			d.healAt = w.step + 1
		}
		d.connectAt = min(d.connectAt, w.step+1)
	}
	w.note("faults off")
	never := func(*daemon) bool { return false }
	w.until(w.settled, never)

	w.step++
	w.noteLeft()
	w.restartServe()
	at := w.step
	w.stepOnce(never)
	w.until(func() bool { return w.settled() && len(w.net) == 0 }, never)
	for _, s := range append(w.serveWrong(), w.setsWrong()...) {
		w.violations = append(w.violations, fmt.Sprintf("invariant 4: after serve restarted at %03d, with the faults off and no tick, %s", at, s))
	}

	for range settleTicks {
		w.step++
		w.stepOnce(func(*daemon) bool { return true })
	}
	w.until(func() bool { return len(w.net) == 0 }, never)
}

// until steps the world, ticking when tick says so, until done holds.
func (w *world) until(done func() bool, tick func(*daemon) bool) {
	for !done() {
		w.step++
		w.stepOnce(tick)
	}
}

// settled reports whether every daemon not gone is up and connected. A
// restart keeps its downtime, as during the run.
func (w *world) settled() bool {
	for _, d := range w.daemons {
		if !d.gone && !d.connected {
			return false
		}
	}
	return true
}

// act is daemon d's part of a step: start or fail, heal, reconnect, change
// and tick, in that order.
func (w *world) act(d *daemon, tick bool) {
	switch {
	case d.gone:
		return
	case !d.up:
		if w.step < d.upAt {
			return
		}
		w.start(d)
	case w.faults && w.chance(w.cfg.PDaemonLeave):
		w.leave(d)
		return
	case w.faults && w.chance(w.cfg.PDaemonRestart):
		w.stop(d)
		return
	}
	if d.healAt == 0 && w.faults && w.chance(w.cfg.PPartition) {
		w.partition(d)
	} else if d.healAt != 0 && w.step >= d.healAt {
		d.healAt, d.connectAt = 0, w.step
		w.note("%s heals", d.id)
	}
	if !d.connected && d.healAt == 0 && w.step >= d.connectAt {
		w.connect(d)
	}
	if w.faults && w.chance(pChange) {
		d.ws = w.randomWorkstreams(d.id)
		w.note("%s changes to %s%s", d.id, workstreamsString(d.ws), w.give(d, daemoncore.SetState{State: d.state()}))
	}
	if w.faults && w.chance(pNewSet) {
		d.set = w.randomSet()
		w.note("%s finds %v%s", d.id, idsOf(d.set), w.give(d, daemoncore.SetConflicts{Conflicts: d.set}))
	}
	if tick {
		w.note("%s ticks%s", d.id, w.give(d, daemoncore.SetState{State: d.state()}, daemoncore.Tick{}))
	}
}

// state is what d hands its outbox, which stamps the daemon, epoch and seq.
func (d *daemon) state() core.FullState { return core.FullState{Workstreams: d.ws} }

// start begins a new run of d: the next epoch, as the daemon takes it from
// its state dir, and a new outbox handed the workstreams and the conflict
// set d has; its stream connects soon.
func (w *world) start(d *daemon) {
	d.epoch++
	d.out = daemoncore.NewOutbox(d.id, d.epoch)
	d.up, d.connectAt = true, w.step+w.backoff()
	w.note("%s.%d starts%s", d.id, d.epoch,
		w.give(d, daemoncore.SetState{State: d.state()}, daemoncore.SetConflicts{Conflicts: d.set}))
}

// stop ends d's run; the next one starts a few steps later.
func (w *world) stop(d *daemon) {
	d.up, d.upAt = false, w.step+downMin+w.r.IntN(downMax-downMin+1)
	w.disconnect(d)
	w.note("%s.%d stops, back at %03d", d.id, d.epoch, d.upAt)
}

// leave ends d for good, without an empty full state.
func (w *world) leave(d *daemon) {
	d.up, d.gone, d.leftAt = false, true, w.step
	w.disconnect(d)
	w.note("%s.%d leaves for good", d.id, d.epoch)
}

// partition cuts d off for a few steps: its posts fail and its stream drops.
func (w *world) partition(d *daemon) {
	d.healAt = w.step + 3 + w.r.IntN(13)
	w.disconnect(d)
	w.note("%s partitioned until %03d", d.id, d.healAt)
}

// connect opens d's warnings stream: serve pushes d's warnings, and the
// outbox hears Connected after the current state, as an uplink wake does.
func (w *world) connect(d *daemon) {
	d.connected = true
	w.serve.streams[d.id] = w.serve.st.WarningsFor(d.id)
	w.note("%s connects%s", d.id, w.give(d, daemoncore.SetState{State: d.state()}, daemoncore.Connected{}))
}

func (w *world) disconnect(d *daemon) {
	d.connected = false
	delete(w.serve.streams, d.id)
}

// restartServe empties serve and cuts every stream; each daemon up
// reconnects a few steps later.
func (w *world) restartServe() {
	w.serve = newServe()
	for _, d := range w.daemons {
		if d.connected {
			d.connected, d.connectAt = false, w.step+w.backoff()
		}
	}
	w.note("serve restarts")
}

// backoff is how long a dropped stream takes to reconnect: a few steps, or
// none once the faults are off.
func (w *world) backoff() int {
	if !w.faults {
		return 0
	}
	return 1 + w.r.IntN(3)
}

// give hands events to d's outbox and posts each send. It returns the sends
// for the trace.
func (w *world) give(d *daemon, events ...daemoncore.Event) string {
	var posted []string
	for _, e := range events {
		var sends []daemoncore.Send
		d.out, sends = daemoncore.Step(d.out, e)
		for _, s := range sends {
			posted = append(posted, w.post(d, s))
		}
	}
	if len(posted) == 0 {
		return ""
	}
	return " -> " + strings.Join(posted, " ")
}

// post puts s on the network. It lands on the next step or, by PReorder, up
// to maxDelay steps later, after messages sent since; PDup adds a copy with
// a delay of its own. A partitioned daemon's post fails.
func (w *world) post(d *daemon, s daemoncore.Send) string {
	w.made++
	m := message{at: w.step + 1, made: w.made, from: d, send: s, lost: d.healAt != 0}
	if !m.lost && w.faults && w.chance(w.cfg.PReorder) {
		m.at = w.step + 2 + w.r.IntN(maxDelay-1)
	}
	w.enqueue(m)
	label := fmt.Sprintf("%s#%d@%03d", s.Kind, s.Seq, m.at)
	if m.lost {
		return label + " (partitioned)"
	}
	if w.faults && w.chance(w.cfg.PDup) {
		c := m
		c.copy, c.at = true, w.step+1+w.r.IntN(maxDelay)
		w.enqueue(c)
		label += fmt.Sprintf(" (copy@%03d)", c.at)
	}
	return label
}

func (w *world) enqueue(m message) {
	w.sent++
	m.n = w.sent
	i, _ := slices.BinarySearchFunc(w.net, m, func(a, b message) int {
		return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(a.n, b.n))
	})
	w.net = slices.Insert(w.net, i, m)
}

// deliver lands every message due by now, in order. One lost, posted or
// landing while its sender is partitioned or dropped by PDrop, never reaches
// serve. The sender's run, if still up, hears either result as Sent: every
// Send gets one (see daemoncore.Step), except a copy's.
func (w *world) deliver() {
	for len(w.net) > 0 && w.net[0].at <= w.step {
		m := w.net[0]
		w.net = w.net[1:]
		d := m.from
		ok := !m.lost && d.healAt == 0 && !(w.faults && w.chance(w.cfg.PDrop))
		if ok {
			w.note("%s lands: %s", m, w.receive(m))
			w.checkLanded(m)
		} else {
			w.note("%s lost", m)
		}
		if !m.copy && d.up && d.epoch == m.send.Epoch {
			w.give(d, daemoncore.Sent{Kind: m.send.Kind, Seq: m.send.Seq, OK: ok})
		}
	}
}

// receive is serve taking m as handleMsg does. A full state goes through
// apply, and each warning set it changes is pushed on that daemon's stream
// if open. A conflict set replaces the daemon's report unless its clock
// calls it stale.
func (w *world) receive(m message) string {
	s, d := &w.serve, m.from.id
	if m.send.Kind == daemoncore.KindConflicts {
		clock, ok := w.admit(s.clocks[d], m.send.Epoch, m.send.Seq)
		if !ok {
			return "stale"
		}
		s.clocks[d] = clock
		ids := idsOf(m.send.Conflicts)
		if len(ids) == 0 {
			delete(s.reported, d)
		} else {
			s.reported[d] = ids
		}
		return fmt.Sprintf("reports %v", ids)
	}
	var effects []core.Effect
	s.st, effects = w.apply(s.st, m.send.State)
	var pushed []string
	for _, e := range effects {
		wc, ok := e.(core.WarningsChanged)
		if _, open := s.streams[wc.Daemon]; ok && open {
			s.streams[wc.Daemon] = wc.Warnings
			pushed = append(pushed, fmt.Sprintf("%s:%d", wc.Daemon, len(wc.Warnings)))
		}
	}
	return fmt.Sprintf("%d effects, pushed %v", len(effects), pushed)
}

// noteLeft notes, for each daemon that left, what serve still holds of it,
// which stays until serve restarts. That is known, not a violation.
func (w *world) noteLeft() {
	for _, d := range w.daemons {
		if !d.gone {
			continue
		}
		n := 0
		for _, wr := range w.serve.st.Warnings() {
			if wr.Sides[0].Daemon == d.id || wr.Sides[1].Daemon == d.id {
				n++
			}
		}
		w.note("%s left at %03d: serve still holds %d of its workstreams and %d warnings with it",
			d.id, d.leftAt, len(w.held(d.id)), n)
	}
}

// checkLanded is invariant 7, after each message serve takes in: what serve
// holds for the sender, of that kind, and the epoch it holds it at, are
// those of the newest message posted of all that reached it since it
// started. The order messages were posted in is the truth here, whatever
// the clocks decide: a late message of an earlier run often carries the
// same workstreams, and only the epoch shows serve took it. The sim's
// workstreams already come in the order core keeps them.
func (w *world) checkLanded(m message) {
	f := feed{m.from.id, m.send.Kind}
	if last, ok := w.serve.landed[f]; !ok || m.made > last.made {
		w.serve.landed[f] = m
	}
	newest := w.serve.landed[f]
	got := fmt.Sprintf("%s at epoch %d", workstreamsString(w.held(f.d)), w.serve.st.Clock(f.d).Epoch())
	want := fmt.Sprintf("%s at epoch %d", workstreamsString(newest.send.State.Workstreams), newest.send.Epoch)
	if f.kind == daemoncore.KindConflicts {
		got = fmt.Sprintf("%v at epoch %d", w.serve.reported[f.d], w.serve.clocks[f.d].Epoch())
		want = fmt.Sprintf("%v at epoch %d", idsOf(newest.send.Conflicts), newest.send.Epoch)
	}
	if got != want && !w.flagged[f] {
		w.flagged[f] = true
		w.violations = append(w.violations, fmt.Sprintf("invariant 7: at %03d serve holds %s as %s's %s, the newest that reached it, %s, carries %s",
			w.step, got, f.d, f.kind, newest, want))
	}
}

// check is the invariants once the system settled, after the ones found on the
// way; 5, same seed same trace, is TestSimDeterministic's. A daemon that left
// counts with what serve still holds of it.
func (w *world) check() []string {
	v := slices.Clone(w.violations)
	bad := func(n int, format string, args ...any) {
		v = append(v, fmt.Sprintf("invariant %d: ", n)+fmt.Sprintf(format, args...))
	}
	v = append(v, w.serveWrong()...)
	s := w.serve

	// 3: no warning names a workstream its daemon no longer has.
	for _, wr := range s.st.Warnings() {
		for _, side := range wr.Sides {
			d := w.daemonByID(side.Daemon)
			if d == nil || d.gone {
				continue
			}
			if !slices.ContainsFunc(d.ws, func(x core.Workstream) bool { return x.ID == side.Workstream }) {
				bad(3, "warning %s names %s, which %s no longer has", wr.ID, side.Workstream, d.id)
			}
		}
	}

	return append(v, w.setsWrong()...)
}

// setsWrong is invariant 6, each line starting with its number: what serve
// holds as each daemon's conflict set is the last one it gave.
func (w *world) setsWrong() []string {
	var v []string
	for _, d := range w.daemons {
		if got := w.serve.reported[d.id]; !d.gone && !slices.Equal(got, idsOf(d.set)) {
			v = append(v, fmt.Sprintf("invariant 6: serve holds %v as %s's conflict set, it last gave %v", got, d.id, idsOf(d.set)))
		}
	}
	return v
}

// serveWrong is invariants 1 and 2, each line starting with its number:
// serve's warnings are those of the daemons' last states, and what each
// daemon last got on its stream is what serve holds for it.
func (w *world) serveWrong() []string {
	var v []string
	s := w.serve
	want := core.NewState()
	for _, d := range w.daemons {
		ws := d.ws
		if d.gone {
			ws = w.held(d.id)
		}
		want, _ = core.Apply(want, core.FullState{Daemon: d.id, Workstreams: ws})
	}
	if got := s.st.Warnings(); !slices.Equal(got, want.Warnings()) {
		v = append(v, fmt.Sprintf("invariant 1: serve has warnings %v, the daemons' states give %v", warningIDs(got), warningIDs(want.Warnings())))
	}
	for _, d := range w.daemons {
		if d.gone {
			continue
		}
		pushed, open := s.streams[d.id]
		if !open {
			v = append(v, fmt.Sprintf("invariant 2: %s has no open stream", d.id))
		} else if held := s.st.WarningsFor(d.id); !slices.Equal(pushed, held) {
			v = append(v, fmt.Sprintf("invariant 2: %s last got %v on its stream, serve holds %v for it", d.id, warningIDs(pushed), warningIDs(held)))
		}
	}
	return v
}

// held is the workstreams serve holds for daemon d, sorted by ID.
func (w *world) held(d core.DaemonID) []core.Workstream {
	var out []core.Workstream
	for _, ws := range w.serve.st.Workstreams() {
		if owner, _ := w.serve.st.Owner(ws.ID); owner == d {
			out = append(out, ws)
		}
	}
	return out
}

func (w *world) daemonByID(id core.DaemonID) *daemon {
	for _, d := range w.daemons {
		if d.id == id {
			return d
		}
	}
	return nil
}

func (w *world) chance(p float64) bool { return p > 0 && w.r.Float64() < p }

func (w *world) note(format string, args ...any) {
	w.notes = append(w.notes, fmt.Sprintf(format, args...))
}

// randomWorkstreams draws up to 3 workstreams for d from small pools of
// repos and files, so overlaps are frequent. Every call builds new slices.
func (w *world) randomWorkstreams(d core.DaemonID) []core.Workstream {
	var out []core.Workstream
	for i := range w.r.IntN(4) {
		ws := core.Workstream{
			ID:     core.WorkstreamID(string(d) + ":w" + strconv.Itoa(i)),
			Repo:   repos[w.r.IntN(len(repos))],
			Branch: "b" + strconv.Itoa(w.r.IntN(3)),
		}
		for _, f := range files {
			if w.r.IntN(3) == 0 {
				ws.Files = append(ws.Files, f)
			}
		}
		out = append(out, ws)
	}
	return out
}

// randomSet draws a conflict set from a small pool of IDs, sorted by ID.
func (w *world) randomSet() []core.Conflict {
	var out []core.Conflict
	for _, id := range conflictIDs {
		if w.r.IntN(3) == 0 {
			out = append(out, core.Conflict{ID: id})
		}
	}
	return out
}

func idsOf(cs []core.Conflict) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func warningIDs(ws []core.Warning) []string {
	out := make([]string, len(ws))
	for i, wr := range ws {
		out[i] = wr.Repo + "/" + wr.Path + ":" + string(wr.Sides[0].Workstream) + "+" + string(wr.Sides[1].Workstream)
	}
	return out
}

func workstreamsString(ws []core.Workstream) string {
	parts := make([]string, len(ws))
	for i, x := range ws {
		parts[i] = fmt.Sprintf("%s(%s %s %s)", x.ID, x.Repo, x.Branch, strings.Join(x.Files, ","))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
