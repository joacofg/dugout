package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// Escalation: what the agents cannot settle goes to both humans,
// by name, with what to decide. Three things send a conflict there: the time
// rule and a dispute (core.EscalationRule), checked on every poll, and an
// agent's office_escalate. Each daemon tells its own human, with a desktop
// notification, and its sessions on the conflict, at their next hook: the
// agent leaves the span alone until its human decides, and its human sees the
// escalation in the session too. A desktop that cannot show it is never
// silent: the agent is told to ask its human in its own session, and the
// hook writes it to the terminal as an OSC 9 notification on the way.

const (
	// notifyTimeout bounds one desktop notification.
	notifyTimeout = 2 * time.Second
	// relayTimeout bounds handing an escalation to serve.
	relayTimeout = 2 * time.Second
	// escalationGrace keeps an escalation whose conflicts this daemon does not
	// see standing: one relayed from the other side can come before this
	// detector finds the conflict, and one whose conflicts resolved is moot.
	escalationGrace = 2 * time.Minute
	// triggerAgent is an escalation an agent asked for, with office_escalate.
	triggerAgent core.Trigger = "agent"
	// escTitle heads every desktop notification.
	escTitle = "Dugout: decide"
)

// Notification is one desktop notification.
type Notification struct {
	Title, Subtitle, Body string
	Urgent                bool // with a sound
}

// Notifier shows n on this machine's desktop within ctx, or says why it could
// not. Config.Notify replaces DesktopNotify, for tests.
type Notifier func(ctx context.Context, n Notification) error

// notifyGOOS is a var so a test can take another system's path on a Mac.
var notifyGOOS = runtime.GOOS

// DesktopNotify shows n with osascript's display notification on a Mac: the
// texts go as arguments, never inside the script, so no quote in them can
// change it. Elsewhere there is no desktop notification yet.
func DesktopNotify(ctx context.Context, n Notification) error {
	if notifyGOOS != "darwin" {
		return fmt.Errorf("no desktop notifications on %s yet", notifyGOOS)
	}
	show := "display notification (item 1 of argv) with title (item 2 of argv) subtitle (item 3 of argv)"
	if n.Urgent {
		show += ` sound name "Glass"`
	}
	cmd := exec.CommandContext(ctx, "osascript", "-e", "on run argv", "-e", show, "-e", "end run", n.Body, n.Title, n.Subtitle)
	out, err := cmd.CombinedOutput()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errors.New("osascript is not installed")
	case ctx.Err() != nil:
		return errors.New("osascript did not answer in time")
	case err != nil:
		if msg := Sanitize(strings.TrimSpace(string(out))); msg != "" {
			return fmt.Errorf("osascript failed: %s", msg)
		}
		return fmt.Errorf("osascript failed: %v", err)
	}
	return nil
}

// escalation is one thing that went to the humans, as this daemon tells its
// own: the conflicts it is about, both sides, and what to decide. Every text
// is sanitized. Under Daemon.mu.
type escalation struct {
	trigger   core.Trigger
	conflicts []string     // IDs, sorted by path
	paths     []string     // the conflicts' paths, sanitized, sorted
	sides     [2]core.Side // for an agent's, the asking side first; else the conflict's order
	summary   string       // what both humans read: who, where and what to decide
	detail    string       // the summary and both sides' intents, for the sessions
	urgent    bool
	at        time.Time
	// ready is set once the desktop notification was tried; desktop is ""
	// when it showed, and why it could not otherwise.
	ready   bool
	desktop string
	// asker is the session that asked with office_escalate, "" for the rules
	// and for one relayed from another machine; answered, that its human
	// answered it since (a prompt in that session), so it may ask again.
	asker    string
	answered bool
	told     map[string]bool // sessions that heard of it
	osc      map[string]bool // sessions whose terminal got it
}

func (e *escalation) touches(ws core.WorkstreamID) bool { return touches(e.sides, ws) }

// millis is t on the time rule's clock; the zero time is "not yet".
func millis(t time.Time) core.Millis {
	if t.IsZero() {
		return 0
	}
	return core.Millis(t.UnixMilli())
}

// personOf names s's human in an escalation: the person and the branch, or the
// branch alone when its git user.name is unset.
func personOf(s core.Side) string {
	if p := Sanitize(s.Person); p != "" {
		return fmt.Sprintf(`%s (branch "%s")`, p, Sanitize(s.Branch))
	}
	return fmt.Sprintf(`branch "%s"`, Sanitize(s.Branch))
}

// personName is personOf without the branch, for short mentions.
func personName(s core.Side) string {
	if p := Sanitize(s.Person); p != "" {
		return p
	}
	return fmt.Sprintf(`branch "%s"`, Sanitize(s.Branch))
}

// where names the files of an escalation: the first, and how many more.
func where(paths []string) string {
	switch len(paths) {
	case 0:
		return "a file"
	case 1:
		return fmt.Sprintf(`"%s"`, paths[0])
	case 2:
		return fmt.Sprintf(`"%s" and 1 more file`, paths[0])
	}
	return fmt.Sprintf(`"%s" and %d more files`, paths[0], len(paths)-1)
}

// intents is both sides' declared intents, for the sessions; "" when neither
// declared one.
func intents(sides [2]core.Side) string {
	var parts []string
	for _, s := range sides {
		if i := SanitizeIntent(s.Intent); i != "" {
			parts = append(parts, fmt.Sprintf(`%s's intent: "%s".`, personName(s), i))
		}
	}
	return strings.Join(parts, " ")
}

// span says d for a human: whole minutes as "10 min", anything else as Go
// writes it.
func span(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return d.Round(time.Second).String()
}

// ruleEscalation is the escalation the fixed rules make of cs, the standing
// conflicts of one pair of workstreams that went due at now, after after.
func ruleEscalation(trigger core.Trigger, cs []core.Conflict, after time.Duration, now time.Time) *escalation {
	slices.SortFunc(cs, func(a, b core.Conflict) int { return strings.Compare(a.Path, b.Path) })
	e := &escalation{trigger: trigger, sides: cs[0].Sides, at: now, told: map[string]bool{}, osc: map[string]bool{}}
	owners := map[core.WorkstreamID]bool{}
	for _, c := range cs {
		e.conflicts = append(e.conflicts, c.ID)
		e.paths = append(e.paths, Sanitize(c.Path))
		owners[c.Owner()] = true
	}
	a, b := personOf(e.sides[0]), personOf(e.sides[1])
	if trigger == core.TriggerDisputed {
		e.summary = fmt.Sprintf("%s and %s both claimed the conflicting lines of %s: decide who keeps them.", a, b, where(e.paths))
	} else {
		decide := "Dugout's rule splits the spans between them: decide who yields each."
		if len(owners) == 1 {
			keeper, yielder := e.sides[0], e.sides[1]
			if cs[0].Owner() == yielder.Workstream {
				keeper, yielder = yielder, keeper
			}
			decide = fmt.Sprintf("Dugout's rule keeps %s's lines: decide whether %s yields them.", personName(keeper), personName(yielder))
		}
		e.summary = fmt.Sprintf("Conflict in %s still standing %s after both agents were warned, between %s and %s. %s",
			where(e.paths), span(after), a, b, decide)
	}
	e.detail = strings.TrimSpace(e.summary + " " + intents(e.sides))
	return e
}

// agentEscalation is the escalation of an agent's office_escalate about c,
// from the side mine, with reason saying what to decide, sanitized.
func agentEscalation(c core.Conflict, mine core.WorkstreamID, reason string, urgent bool, now time.Time) *escalation {
	i := mineOf(c.Sides, mine)
	e := &escalation{trigger: triggerAgent, conflicts: []string{c.ID}, paths: []string{Sanitize(c.Path)}, sides: [2]core.Side{c.Sides[i], c.Sides[1-i]},
		urgent: urgent, at: now, told: map[string]bool{}, osc: map[string]bool{}}
	e.summary = fmt.Sprintf(`%s asks about the conflict in %s with %s: %s`, personOf(e.sides[0]), where(e.paths), personOf(e.sides[1]), reason)
	e.detail = strings.TrimSpace(e.summary + " " + intents(e.sides))
	return e
}

// notification is e as a desktop notification.
func (e *escalation) notification() Notification {
	title := escTitle
	if e.urgent {
		title = escTitle + " (urgent)"
	}
	return Notification{Title: title, Subtitle: truncate(fmt.Sprintf("%s and %s, %s", personName(e.sides[0]), personName(e.sides[1]), where(e.paths)), 120, "…"),
		Body: e.summary, Urgent: e.urgent}
}

// notify tries e's desktop notification and records how it went; e is
// ready after it, for the sessions to hear of.
func (d *Daemon) notify(e *escalation) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	err := d.cfg.Notify(ctx, e.notification())
	cancel()
	d.mu.Lock()
	e.ready = true
	if err != nil {
		e.desktop = Sanitize(err.Error())
	}
	d.mu.Unlock()
	if err != nil {
		d.log.Warn("escalation not shown on the desktop", "trigger", e.trigger, "conflicts", strings.Join(e.conflicts, ","), "err", err)
	} else {
		d.log.Info("escalation shown on the desktop", "trigger", e.trigger, "conflicts", strings.Join(e.conflicts, ","))
	}
}

// noteWarned records that a session of ws was just handed conflicts cs: the
// time rule counts from when both sides were first warned. Callers hold d.mu.
func (d *Daemon) noteWarnedLocked(ws core.WorkstreamID, cs []core.Conflict, now time.Time) {
	for _, c := range cs {
		by := d.warned[c.ID]
		if by == nil {
			by = map[core.WorkstreamID]time.Time{}
			d.warned[c.ID] = by
		}
		if _, ok := by[ws]; !ok {
			by[ws] = now
		}
	}
}

// forgetResolvedLocked drops what the time rule kept of each conflict seen
// standing that no longer stands, so one that comes back starts over, and the
// escalations whose conflicts all resolved, past escalationGrace. Callers hold
// d.mu.
func (d *Daemon) forgetResolvedLocked(now time.Time) {
	for id := range d.seen {
		if _, ok := d.conflicts[id]; !ok {
			delete(d.seen, id)
			delete(d.warned, id)
			delete(d.escalated, id)
		}
	}
	for id := range d.warned {
		if _, ok := d.conflicts[id]; !ok {
			delete(d.warned, id)
		}
	}
	d.escs = slices.DeleteFunc(d.escs, func(e *escalation) bool {
		if now.Sub(e.at) < escalationGrace {
			return false
		}
		for _, id := range e.conflicts {
			if _, ok := d.conflicts[id]; ok {
				return false
			}
		}
		return true
	})
	for id := range d.escalated {
		if _, seen := d.seen[id]; seen {
			continue
		}
		if !slices.ContainsFunc(d.escs, func(e *escalation) bool { return slices.Contains(e.conflicts, id) }) {
			delete(d.escalated, id)
		}
	}
}

// checkEscalations runs the fixed rules on every standing conflict with a
// side of this machine's and escalates, once while it stands, each one due:
// one escalation per pair of workstreams. A side of this machine counts as
// warned when a session of it was first handed the conflict; another
// machine's, when this daemon first saw the conflict, since its own daemon
// runs the rule for its human. The delay is the repo's escalate_after, the
// shortest of this machine's sides that set one. While Dugout is paused,
// nothing escalates.
func (d *Daemon) checkEscalations() {
	if paused, _ := d.pausedNow(); paused {
		return
	}
	now := d.clock()
	type pair [2]core.WorkstreamID
	due := map[pair][]core.Conflict{}
	trigger := map[pair]core.Trigger{}
	delay := map[pair]time.Duration{}
	d.mu.Lock()
	d.forgetResolvedLocked(now)
	after := map[core.WorkstreamID]time.Duration{}
	for _, wt := range d.worktrees {
		if wt.snap != nil {
			after[wt.ws] = wt.snap.EscalateAfter
		}
	}
	for id, c := range d.conflicts {
		if _, ok := d.seen[id]; !ok {
			d.seen[id] = now
		}
		if d.escalated[id] {
			continue
		}
		var warned [2]core.Millis
		wait, local := time.Duration(0), false
		for i, s := range c.Sides {
			if s.Daemon != d.id {
				warned[i] = millis(d.seen[id])
				continue
			}
			local = true
			warned[i] = millis(d.warned[id][s.Workstream])
			if a := after[s.Workstream]; a > 0 && (wait == 0 || a < wait) {
				wait = a
			}
		}
		if !local {
			continue
		}
		c.Stances = d.stancesLocked(c)
		t := core.EscalationRule{After: core.Millis(wait.Milliseconds())}.Decide(c, warned, millis(now))
		if t == core.TriggerNone {
			continue
		}
		d.escalated[id] = true
		k := pair{c.Sides[0].Workstream, c.Sides[1].Workstream}
		due[k] = append(due[k], c)
		if trigger[k] != core.TriggerDisputed {
			trigger[k] = t
		}
		delay[k] = wait
	}
	d.mu.Unlock()
	for _, k := range slices.SortedFunc(maps.Keys(due), func(a, b pair) int {
		return strings.Compare(string(a[0])+"\x00"+string(a[1]), string(b[0])+"\x00"+string(b[1]))
	}) {
		e := ruleEscalation(trigger[k], due[k], delay[k], now)
		d.mu.Lock()
		d.escs = append(d.escs, e)
		d.metrics.Escalated(e.conflicts, core.EscalatedByRule)
		d.mu.Unlock()
		d.log.Info("escalated", "trigger", e.trigger, "conflicts", strings.Join(e.conflicts, ","), "sides", string(k[0])+","+string(k[1]))
		d.notify(e)
		if d.cfg.Server != "" {
			trigger := wire.TriggerTime
			if e.trigger == core.TriggerDisputed {
				trigger = wire.TriggerDisputed
			}
			d.uplink.Escalated(wire.EscalationNotice{Conflicts: e.conflicts, Trigger: trigger})
		}
	}
}

// handleEscalate is office_escalate: req.SessionID's agent takes to both
// humans what it cannot settle with the other side of one of its conflicts,
// the one whose ID starts with req.Conflict, or its only one. One question per
// session at a time: a session whose last one its human has not answered yet
// (a prompt in that session) cannot ask another. This machine's human gets a
// desktop notification; the other side's, too when it is this machine's, and
// through serve when it is another's. While Dugout is paused, nothing goes.
func (d *Daemon) handleEscalate(req wire.Request) wire.Response {
	if paused, team := d.pausedNow(); team {
		return wire.Response{Error: "Dugout is paused for the whole team: nothing was escalated. Ask your human in this session."}
	} else if paused {
		return wire.Response{Error: "Dugout is paused on this machine: nothing was escalated. Ask your human in this session."}
	}
	reason := SanitizeIntent(req.Text)
	if strings.TrimSpace(reason) == "" {
		return wire.Response{Error: "the reason is empty: say what the humans have to decide and what each side wants."}
	}
	urgent := ParseUrgency(req.Urgency)
	prefix := strings.ToLower(strings.TrimSpace(req.Conflict))
	now := d.clock()
	d.mu.Lock()
	s := d.sessions[req.SessionID]
	var ws core.WorkstreamID
	if s != nil {
		if wt := d.worktrees[s.Root]; wt != nil {
			ws = wt.ws
		}
	}
	if ws == "" {
		d.mu.Unlock()
		return wire.Response{Error: "Dugout does not know that session: use the session_id Dugout gave you at the start."}
	}
	for _, e := range d.escs {
		if e.asker == s.ID && !e.answered {
			d.mu.Unlock()
			return wire.Response{Error: fmt.Sprintf("you already escalated the conflict in %s and your human has not answered yet: one question at a time. Wait for their answer in this session, then escalate the next one.", where(e.paths))}
		}
	}
	var hits []core.Conflict
	for _, c := range d.conflicts {
		if touches(c.Sides, ws) && strings.HasPrefix(c.ID, prefix) {
			hits = append(hits, c)
		}
	}
	if len(hits) != 1 {
		d.mu.Unlock()
		switch {
		case len(hits) > 1 && prefix == "":
			return wire.Response{Error: fmt.Sprintf("you have %d conflicts: pass the conflict_id of the one to escalate, as the Dugout warning or office_who shows it.", len(hits))}
		case len(hits) > 1:
			return wire.Response{Error: "that conflict_id is the start of more than one of your conflicts: pass it in full, as office_who shows it."}
		case prefix == "":
			return wire.Response{Error: "Dugout sees no conflict on your branch, so there is no other side to escalate with: ask your human in this session."}
		}
		return wire.Response{Error: "Dugout does not see that conflict on your branch. office_who shows yours."}
	}
	c := hits[0]
	e := agentEscalation(c, ws, reason, urgent, now)
	e.asker, e.told[s.ID] = s.ID, true
	d.escalated[c.ID] = true // the time rule does not ask again
	d.escs = append(d.escs, e)
	d.metrics.Escalated([]string{c.ID}, core.EscalatedByAgent)
	d.mu.Unlock()
	other := e.sides[1]
	out := wire.Escalated{ID: short(c.ID), Path: Sanitize(c.Path), Mine: personOf(e.sides[0]), Theirs: personOf(other), Text: e.summary, Other: "local"}

	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		if other.Daemon == d.id {
			return
		}
		out.Other = "unreached"
		if d.cfg.Server == "" {
			out.OtherWhy = "this daemon runs without a server, so nothing reaches another machine"
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
		defer cancel()
		delivered, err := d.uplink.Escalate(ctx, wire.Escalation{Conflict: c.ID, Repo: c.Repo, Path: c.Path, From: e.sides[0], To: other, Reason: reason, Urgent: urgent})
		var status *StatusError
		switch {
		case errors.Is(err, ErrNotRelayed):
			out.OtherWhy = "the server is older than this dugout and does not relay escalations"
		case errors.As(err, &status) && status.Status == 422:
			out.OtherWhy = "the server does not see your work overlapping theirs yet"
		case err != nil:
			out.OtherWhy = "the server did not take it"
		case !delivered:
			out.OtherWhy = "their daemon is not connected to the server right now"
		default:
			out.Other = "relayed"
		}
		if err != nil {
			d.log.Warn("escalation not relayed", "conflict", c.ID, "err", err)
		}
	}()
	if d.cfg.Server != "" {
		d.uplink.Escalated(wire.EscalationNotice{Conflicts: []string{c.ID}, Trigger: wire.TriggerAgent, From: ws, Reason: reason, Urgent: urgent})
	}
	d.notify(e)
	<-relayed
	d.mu.Lock()
	out.Desktop = e.desktop
	d.mu.Unlock()
	if other.Daemon == d.id {
		out.Session = Sanitize(d.sessionNames(map[core.WorkstreamID]bool{other.Workstream: true})[other.Workstream])
	}
	d.log.Info("escalated", "trigger", triggerAgent, "conflicts", c.ID, "session", s.ID, "other", out.Other, "urgent", urgent)
	b, err := json.Marshal(out)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	return wire.Response{Output: b}
}

// ParseUrgency reads office_escalate's urgency: "urgent", "high" or
// "critical" are urgent, anything else is normal.
func ParseUrgency(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "urgent", "high", "critical":
		return true
	}
	return false
}

// takeRelayed is an escalation another daemon's agent asked for about a
// conflict with a workstream of this machine's, as serve handed it over: this
// machine's human gets it on the desktop, and its sessions on that workstream
// at their next hook. It is dropped while Dugout is paused, and when the
// workstream is not this machine's.
func (d *Daemon) takeRelayed(r wire.Escalation) {
	if paused, _ := d.pausedNow(); paused {
		d.log.Info("escalation dropped while paused", "conflict", r.Conflict, "from", r.From.Daemon)
		return
	}
	d.mu.Lock()
	live := false
	for _, wt := range d.worktrees {
		live = live || (wt.ws == r.To.Workstream && r.To.Daemon == d.id)
	}
	if !live {
		d.mu.Unlock()
		d.log.Warn("escalation for a workstream this machine does not have", "to", r.To.Workstream, "from", r.From.Daemon)
		return
	}
	c := core.Conflict{ID: r.Conflict, Repo: r.Repo, Path: r.Path, Sides: [2]core.Side{r.From, r.To}}
	e := agentEscalation(c, r.From.Workstream, SanitizeIntent(r.Reason), r.Urgent, d.clock())
	d.escalated[r.Conflict] = true
	d.escs = append(d.escs, e)
	d.metrics.Escalated([]string{r.Conflict}, core.EscalatedRelayed)
	d.mu.Unlock()
	d.log.Info("escalation received", "conflict", r.Conflict, "from", r.From.Daemon, "urgent", r.Urgent)
	d.notify(e)
}

// escalationNews is what session s of ws hears at a hook of event about the
// escalations that touch ws: the next one it was not told of, as lines after
// the block for its agent and one for its human's systemMessage, and the text
// for its terminal when this machine's desktop could not show it. A prompt
// from its human answers the escalation the session asked. PreToolUse and
// SessionEnd hear nothing. Callers hold s.mu.
func (d *Daemon) escalationNews(event string, s *Session, ws core.WorkstreamID) (ctx []string, human, terminal string) {
	if event == "PreToolUse" || event == "SessionEnd" || ws == "" {
		return nil, "", ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.escs {
		if e.asker == s.ID && event == "UserPromptSubmit" {
			e.answered = true
		}
	}
	for _, e := range d.escs {
		if !e.ready || !e.touches(ws) {
			continue
		}
		if e.desktop != "" && !e.osc[s.ID] {
			e.osc[s.ID] = true
			terminal = escTitle + ": " + e.summary
		}
		if e.told[s.ID] {
			if terminal != "" {
				return nil, "", terminal
			}
			continue
		}
		e.told[s.ID] = true
		ctx = []string{Block("- escalated to both humans: " + e.detail),
			"Dugout: that conflict went to both humans (the escalation above). Leave its span as it is until your human decides; go on with the rest of your task."}
		if e.desktop != "" {
			ctx = append(ctx, fmt.Sprintf("Dugout: this machine could not show your human a desktop notification (%s): tell them in this session now, in one line, what they have to decide, as the escalation above says.", e.desktop))
		}
		return ctx, "Dugout escalation: " + e.summary, terminal
	}
	return nil, "", terminal
}

// addNews puts an escalation's lines in a hook's answer: ctx after whatever
// context it has, human on its systemMessage.
func addNews(answer map[string]any, event string, ctx []string, human string) {
	if len(ctx) > 0 {
		hso, _ := answer["hookSpecificOutput"].(map[string]any)
		if hso == nil {
			hso = map[string]any{"hookEventName": event}
			answer["hookSpecificOutput"] = hso
		}
		cur, _ := hso["additionalContext"].(string)
		hso["additionalContext"] = strings.Join(append(slices.DeleteFunc([]string{cur}, func(s string) bool { return s == "" }), ctx...), "\n")
	}
	if human != "" {
		if cur, _ := answer["systemMessage"].(string); cur != "" {
			human = cur + "\n" + human
		}
		answer["systemMessage"] = human
	}
}
