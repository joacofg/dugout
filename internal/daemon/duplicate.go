package daemon

import (
	"context"
	"hash/fnv"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/judge"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	// dupEvery is how often the duplicate detector runs without a kick: it
	// asks serve for the other daemons' intents and retries failed pairs.
	dupEvery = 30 * time.Second
	// judgeTimeout bounds one comparison; judgeRetry is the wait before a
	// pair whose comparison failed is asked again.
	judgeTimeout = 30 * time.Second
	judgeRetry   = 5 * time.Minute
	// judgeBudget caps the comparisons of any hour, so intents that keep
	// changing cannot run up the bill: on the default model it is about a
	// cent an hour.
	judgeBudget = 120
	// remoteStale is how long the other daemons' intents are trusted after
	// serve stops answering.
	remoteStale = 5 * time.Minute
	// remoteTimeout bounds asking serve for them.
	remoteTimeout = 5 * time.Second
)

// Duplicate says the judge found that two workstreams of a repo declared the
// same task, whether or not they share a file. Sides are sorted by
// Workstream, each with the intent judged; a side's Session is this
// machine's (Daemon.viewFor). Reason is the judge's, sanitized.
type Duplicate struct {
	ID     string
	Repo   string
	Sides  [2]core.Side
	Reason string
}

// DuplicateID is stable for a (repo, pair of workstreams), whatever their
// order and intents.
func DuplicateID(repo string, a, b core.WorkstreamID) string {
	if b < a {
		a, b = b, a
	}
	h := fnv.New64a()
	for _, part := range []string{"duplicate", repo, string(a), string(b)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	id := strconv.FormatUint(h.Sum64(), 16)
	return strings.Repeat("0", 16-len(id)) + id
}

// JudgeStats is what the judge has cost since the daemon started, for
// `dugout status`.
type JudgeStats struct {
	Comparisons, Duplicates, Errors int
	InputTokens, OutputTokens       int64
	USD                             float64
	Unpriced                        int    // comparisons of a model without a known price
	LastError                       string // sanitized
}

// duplicatesFor is the current duplicates that touch ws, sorted by ID.
func (d *Daemon) duplicatesFor(ws core.WorkstreamID) []Duplicate {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Duplicate
	for _, dup := range d.dupes {
		if touches(dup.Sides, ws) {
			out = append(out, dup)
		}
	}
	slices.SortFunc(out, func(a, b Duplicate) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// kickDuplicates asks for a duplicate detector run soon. It never blocks.
func (d *Daemon) kickDuplicates() {
	select {
	case d.dupKick <- struct{}{}:
	default:
	}
}

// judgeStats is a copy of the judge's running totals.
func (d *Daemon) judgeStats() JudgeStats {
	d.judgeMu.Lock()
	defer d.judgeMu.Unlock()
	return d.stats
}

// dupLoop runs the duplicate detector on every kick and every dupEvery until
// ctx is done. Without a judge there is nothing to run.
func (d *Daemon) dupLoop(ctx context.Context) {
	if d.cfg.Judge == nil {
		return
	}
	det := &dupDetector{d: d, verdicts: map[dupKey]judge.Verdict{}, failed: map[dupKey]dupFailure{}, current: map[string]Duplicate{}}
	tick := time.NewTicker(dupEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.dupKick:
		case <-tick.C:
			det.fetchRemote = true
		}
		det.run(ctx)
	}
}

// dupKey is a pair of intents in judge.Order: the cache holds one verdict per
// pair of texts, so a pair is judged again only when an intent changes.
type dupKey struct{ a, b string }

func keyOfIntents(a, b string) dupKey {
	a, b = judge.Order(a, b)
	return dupKey{a, b}
}

type dupFailure struct {
	at  time.Time
	msg string
}

// dupDetector compares the declared intents of the in-flight workstreams of
// each repo, pairs with one side of this daemon's at least, and publishes
// the duplicates. Only the dupLoop goroutine touches it.
type dupDetector struct {
	d           *Daemon
	verdicts    map[dupKey]judge.Verdict // by pair of intents, of the current pairs
	failed      map[dupKey]dupFailure    // last failed comparison, since the last good one
	calls       []time.Time              // comparisons of the last hour
	budgetNoted bool                     // the budget's exhaustion was logged
	current     map[string]Duplicate     // as last published

	fetchRemote bool              // the next run asks serve again
	remoteTried bool              // a run asked serve already
	remote      []core.Workstream // the other daemons', with an intent, as serve last had them
	remoteAt    time.Time         // when serve last answered
	remoteErr   string            // the last error asking serve, logged once
}

// participant is a workstream whose intent is compared, with its repo.
type participant struct {
	repo string
	side core.Side
}

// participantsLocked is this machine's workstreams with a declared intent and files
// in flight or a session attached, sorted by workstream. d.mu held.
func (d *Daemon) participantsLocked() []participant {
	attached := map[string]bool{}
	for _, s := range d.sessions {
		attached[s.Root] = true
	}
	var out []participant
	for _, wt := range d.worktrees {
		if wt.snap == nil || wt.intent == "" || (len(wt.snap.Files) == 0 && !attached[wt.root]) {
			continue
		}
		out = append(out, participant{repo: wt.snap.Repo, side: core.Side{Daemon: d.id, Workstream: wt.ws, Branch: wt.snap.Branch, Intent: wt.intent}})
	}
	slices.SortFunc(out, func(a, b participant) int {
		return strings.Compare(string(a.side.Workstream), string(b.side.Workstream))
	})
	return out
}

// quietlyPaused reports a pause as pausedNow does, without counting a new
// one: only a hook tells a session of it.
func (d *Daemon) quietlyPaused() bool {
	if d.paused.Load() {
		return true
	}
	_, err := os.Stat(statedir.PausedPath(d.cfg.StateDir))
	return err == nil
}

// run compares every current pair it has no verdict for, within the hour's
// budget, and publishes the duplicates. A pair it could not compare keeps the
// duplicate it last published, if any. While paused it compares nothing and
// keeps what it published.
func (det *dupDetector) run(ctx context.Context) {
	d := det.d
	if d.quietlyPaused() {
		return
	}
	if det.fetchRemote || !det.remoteTried {
		det.fetchRemote, det.remoteTried = false, true
		det.refreshRemote(ctx)
	}
	d.mu.Lock()
	all := d.participantsLocked()
	d.mu.Unlock()
	if d.cfg.Server != "" && time.Since(det.remoteAt) < remoteStale {
		for _, ws := range det.remote {
			owner, _, _ := strings.Cut(string(ws.ID), ":")
			all = append(all, participant{repo: ws.Repo, side: core.Side{Daemon: core.DaemonID(owner), Workstream: ws.ID, Branch: ws.Branch, Intent: ws.Intent}})
		}
	}
	type pair struct {
		repo  string
		sides [2]core.Side
		key   dupKey
	}
	var pairs []pair
	for i, a := range all {
		for _, b := range all[i+1:] {
			if a.repo != b.repo || a.side.Workstream == b.side.Workstream || (a.side.Daemon != d.id && b.side.Daemon != d.id) {
				continue
			}
			sides := [2]core.Side{a.side, b.side}
			if sides[1].Workstream < sides[0].Workstream {
				sides[0], sides[1] = sides[1], sides[0]
			}
			pairs = append(pairs, pair{repo: a.repo, sides: sides, key: keyOfIntents(a.side.Intent, b.side.Intent)})
		}
	}
	slices.SortFunc(pairs, func(x, y pair) int {
		return strings.Compare(DuplicateID(x.repo, x.sides[0].Workstream, x.sides[1].Workstream), DuplicateID(y.repo, y.sides[0].Workstream, y.sides[1].Workstream))
	})
	live := map[dupKey]bool{}
	next := map[string]Duplicate{}
	for _, p := range pairs {
		if ctx.Err() != nil {
			return
		}
		live[p.key] = true
		id := DuplicateID(p.repo, p.sides[0].Workstream, p.sides[1].Workstream)
		v, ok := det.verdicts[p.key]
		if !ok {
			if v, ok = det.judge(ctx, p.sides); !ok {
				// No verdict on the new intents yet: the last one stands.
				if last, was := det.current[id]; was {
					next[id] = last
				}
				continue
			}
		}
		if v.Duplicate {
			next[id] = Duplicate{ID: id, Repo: p.repo, Sides: p.sides, Reason: Sanitize(v.Reason)}
		}
	}
	maps.DeleteFunc(det.verdicts, func(k dupKey, _ judge.Verdict) bool { return !live[k] })
	maps.DeleteFunc(det.failed, func(k dupKey, _ dupFailure) bool { return !live[k] })
	det.publish(next)
}

// refreshRemote asks serve for the other daemons' workstreams that declared
// an intent. On a failure it keeps the last answer, for remoteStale.
func (det *dupDetector) refreshRemote(ctx context.Context) {
	d := det.d
	if d.cfg.Server == "" {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	dump, err := d.uplink.State(sctx)
	cancel()
	if err != nil {
		if msg := err.Error(); msg != det.remoteErr && ctx.Err() == nil {
			d.log.Warn("duplicates without serve", "err", err)
			det.remoteErr = msg
		}
		return
	}
	det.remoteErr = ""
	det.remote = det.remote[:0]
	for _, ws := range dump.Workstreams {
		if owner, _, _ := strings.Cut(string(ws.ID), ":"); core.DaemonID(owner) != d.id && ws.Intent != "" {
			ws.Intent = SanitizeIntent(ws.Intent)
			det.remote = append(det.remote, ws)
		}
	}
	det.remoteAt = time.Now()
}

// judge asks the judge about a pair's intents and caches the verdict. It
// reports false when there is none: the pair failed within judgeRetry, the
// hour's budget is spent, or the comparison failed.
func (det *dupDetector) judge(ctx context.Context, sides [2]core.Side) (judge.Verdict, bool) {
	d := det.d
	key := keyOfIntents(sides[0].Intent, sides[1].Intent)
	if f, ok := det.failed[key]; ok && time.Since(f.at) < judgeRetry {
		return judge.Verdict{}, false
	}
	now := time.Now()
	det.calls = slices.DeleteFunc(det.calls, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	if len(det.calls) >= judgeBudget {
		if !det.budgetNoted {
			d.log.Warn("judge budget spent: duplicates wait for the next hour", "per_hour", judgeBudget)
			det.budgetNoted = true
		}
		return judge.Verdict{}, false
	}
	det.budgetNoted = false
	det.calls = append(det.calls, now)
	jctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	v, err := d.cfg.Judge.Judge(jctx, key.a, key.b)
	cancel()
	ms := time.Since(now).Milliseconds()
	if ctx.Err() != nil {
		return judge.Verdict{}, false // shutting down: says nothing about the pair
	}
	d.judgeMu.Lock()
	d.stats.Comparisons++
	d.stats.InputTokens += v.Usage.InputTokens
	d.stats.OutputTokens += v.Usage.OutputTokens
	d.stats.USD += v.Usage.USD
	if !v.Usage.Priced && (v.Usage.InputTokens > 0 || v.Usage.OutputTokens > 0) {
		d.stats.Unpriced++
	}
	if err != nil {
		d.stats.Errors++
		d.stats.LastError = Sanitize(err.Error())
	} else if v.Duplicate {
		d.stats.Duplicates++
	}
	d.judgeMu.Unlock()
	// What a comparison costs, never what it compared.
	attrs := []any{"ws_a", sides[0].Workstream, "ws_b", sides[1].Workstream, "judge", d.cfg.Judge.Name(), "input_tokens", v.Usage.InputTokens,
		"output_tokens", v.Usage.OutputTokens, "usd", v.Usage.USD, "ms", ms}
	if err != nil {
		msg := err.Error()
		if msg != det.failed[key].msg {
			d.log.Warn("intents not judged", append(attrs, "err", err)...)
		}
		det.failed[key] = dupFailure{at: now, msg: msg}
		return judge.Verdict{}, false
	}
	delete(det.failed, key)
	d.log.Info("intents judged", append(attrs, "duplicate", v.Duplicate)...)
	det.verdicts[key] = v
	return v, true
}

// publish hands next to the daemon and logs what appeared and what went away.
func (det *dupDetector) publish(next map[string]Duplicate) {
	d := det.d
	d.mu.Lock()
	d.dupes = next
	d.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(next)) {
		if _, ok := det.current[id]; !ok {
			dup := next[id]
			d.log.Info("duplicate detected", "id", id, "ws_a", dup.Sides[0].Workstream, "ws_b", dup.Sides[1].Workstream)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(det.current)) {
		if _, ok := next[id]; !ok {
			d.log.Info("duplicate resolved", "id", id)
		}
	}
	det.current = next
}

// statusJudge is the judge's line in `dugout status`.
func (d *Daemon) statusJudge() wire.StatusJudge {
	if d.cfg.Judge == nil {
		why := d.cfg.JudgeOff
		if why == "" {
			why = judge.ErrNotConfigured.Error()
		}
		return wire.StatusJudge{Off: Sanitize(why)}
	}
	st := d.judgeStats()
	return wire.StatusJudge{Name: Sanitize(d.cfg.Judge.Name()), Comparisons: st.Comparisons, Duplicates: st.Duplicates, Errors: st.Errors,
		InputTokens: st.InputTokens, OutputTokens: st.OutputTokens, USD: st.USD, Unpriced: st.Unpriced, LastError: st.LastError}
}
