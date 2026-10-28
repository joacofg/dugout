package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

// sendLine and claimLine start the hints after the block.
const (
	sendLine  = "Dugout: write to a session named above with SendMessage"
	claimLine = "Dugout: if you agree on who keeps a conflicting span, record it with office_claim"
)

// withSession is c with its other side's session, as the daemon stamps it.
func withSession(c core.Conflict, session string) core.Conflict {
	c.Sides[1-mineOf(c.Sides, me)].Session = session
	return c
}

// TestAnswerNamesSessions: a conflict and an overlap name the session their
// other side is messaged at, and after the block Dugout says how to message
// it and how to record who keeps the span, before the ask.
func TestAnswerNamesSessions(t *testing.T) {
	w := warning("y.go", "feat")
	w.Sides[1].Session = "feat-12"
	c := withSession(conflict("x.go", "feat", "content", core.Range{Start: 3, Len: 1}), "feat-12")
	ctx := ctxOf(t, Respond("SessionStart", "", "", NewSession("s1", "/repo", 0), me, View{Overlaps: []core.Warning{w}, Conflicts: []core.Conflict{c}}))
	for _, want := range []string{`with in-flight changes on branch "feat" (daemon d2, session "feat-12")`, `- branch "feat" (daemon d2, session "feat-12") also has in flight: "y.go".`} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("SessionStart lacks %q: %q", want, ctx)
		}
	}
	_, after, ok := strings.Cut(ctx, "</dugout-context>\n")
	lines := strings.Split(after, "\n")
	if !ok || len(lines) != 4 || !strings.HasPrefix(lines[0], sendLine) || !strings.HasPrefix(lines[1], claimLine) ||
		!strings.Contains(lines[1], `session_id "s1"`) || strings.Join(lines[2:], "\n") != askFor("s1") {
		t.Fatalf("after the block = %q, want the send hint, the claim hint and the ask", after)
	}
	// A conflict whose other side has no session open names none and says
	// nothing of the channel: there is no one to agree with.
	ctx = ctxOf(t, Respond("PostToolUse", "", "", NewSession("s2", "/repo", 0), me, View{Conflicts: []core.Conflict{conflict("x.go", "feat", "content", core.Range{Start: 3, Len: 1})}}))
	if strings.Contains(ctx, "session") || strings.Contains(ctx, "SendMessage") || strings.Contains(ctx, "office_claim") {
		t.Fatalf("no session, yet the answer names one or the channel: %q", ctx)
	}
	// A deny names the session too, and says how to record the span.
	reason := denyOf(t, me, c)
	if !strings.Contains(reason, `session "feat-12"`) || !strings.Contains(reason, "\n"+claimLine) {
		t.Fatalf("deny = %q, want the session and the claim hint", reason)
	}
}

// TestOfficeLines: a workstream of the office, sharing no file, is named with
// its session and intent at the turn's start, once; a PostToolUse names only
// who joined since, and a Stop never names the office.
func TestOfficeLines(t *testing.T) {
	peer := core.Side{Daemon: "d1", Workstream: "d1:peer", Branch: "feat-c", Session: "c-3", Intent: "fixes the login"}
	s := NewSession("s1", "/repo", 0)
	ctx := ctxOf(t, Respond("SessionStart", "", "", s, me, View{Office: []core.Side{peer}}))
	want := `- in the office: branch "feat-c" (daemon d1, session "c-3"), with no files in common with yours. Intent: "fixes the login".`
	if !strings.Contains(ctx, "\n"+want+"\n") || !strings.Contains(ctx, "\n"+sendLine) || strings.Contains(ctx, claimLine) {
		t.Fatalf("SessionStart = %q, want the office line and the send hint, no claim hint", ctx)
	}
	if out := Respond("UserPromptSubmit", "", "", s, me, View{Office: []core.Side{peer}}); len(out) != 0 {
		t.Fatalf("office told again = %s", asJSON(t, out))
	}
	late := core.Side{Daemon: "d1", Workstream: "d1:late", Branch: "feat-d", Session: "d-4"}
	if out := Respond("Stop", "", "", s, me, View{Office: []core.Side{peer, late}}); len(out) != 0 {
		t.Fatalf("Stop named the office: %s", asJSON(t, out))
	}
	ctx = ctxOf(t, Respond("PostToolUse", "Edit", "", s, me, View{Office: []core.Side{peer, late}}))
	if !strings.Contains(ctx, `branch "feat-d" (daemon d1, session "d-4")`) || strings.Contains(ctx, "feat-c") {
		t.Fatalf("PostToolUse = %q, want only the newcomer", ctx)
	}
	// A new session on a workstream already told is told again.
	peer.Session = "c-9"
	if ctx := ctxOf(t, Respond("UserPromptSubmit", "", "", s, me, View{Office: []core.Side{peer, late}})); !strings.Contains(ctx, `session "c-9"`) {
		t.Fatalf("new session not told: %q", ctx)
	}
}

// TestNewAddressHandsOver: an overlap or conflict handed before the other
// side had a session open is handed again once it has one.
func TestNewAddressHandsOver(t *testing.T) {
	s := NewSession("s1", "/repo", 0)
	w := warning("y.go", "feat")
	c := conflict("x.go", "feat", "content", core.Range{Start: 3, Len: 1})
	Respond("UserPromptSubmit", "", "", s, me, View{Overlaps: []core.Warning{w}, Conflicts: []core.Conflict{c}})
	w.Sides[1].Session = "feat-12"
	if ctx := ctxOf(t, Respond("UserPromptSubmit", "", "", s, me, View{Overlaps: []core.Warning{w}})); !strings.Contains(ctx, `session "feat-12"`) {
		t.Fatalf("overlap with a new session = %q", ctx)
	}
	if ctx := ctxOf(t, Respond("PostToolUse", "", "", s, me, View{Conflicts: []core.Conflict{withSession(c, "feat-12")}})); !strings.Contains(ctx, `session "feat-12"`) {
		t.Fatalf("conflict with a new session = %q", ctx)
	}
}

// TestStancesDecideRole: what the sides told the registry decides who keeps
// the span, each side's answer says how, a change of stance hands the
// conflict over again, and from both sides exactly one keeps.
func TestStancesDecideRole(t *testing.T) {
	c := conflictWith(first, "x.go") // first keeps by the rule, me yields
	i := mineOf(c.Sides, me)
	cases := []struct {
		mine, theirs  core.Stance
		meKeeps       bool
		mineWhy, they string
	}{
		{core.StanceNone, core.StanceNone, false, "", ""},
		{core.StanceClaim, core.StanceNone, true, "you claimed it", "the other branch claimed it"},
		{core.StanceNone, core.StanceYield, true, "the other branch yielded it to you", "you yielded it"},
		{core.StanceYield, core.StanceNone, false, "you yielded it", "the other branch yielded it to you"},
		{core.StanceClaim, core.StanceClaim, false, "both branches claimed it and the fixed tiebreak gives it to the other one", "both branches claimed it and the fixed tiebreak gives it to you"},
		{core.StanceYield, core.StanceYield, false, "both branches yielded it and the fixed tiebreak gives it to the other one", "both branches yielded it and the fixed tiebreak gives it to you"},
		{core.StanceClaim, core.StanceYield, true, "you claimed it", "you yielded it"},
	}
	for _, tc := range cases {
		c.Stances[i], c.Stances[1-i] = tc.mine, tc.theirs
		if whyOwner(me, c) != tc.mineWhy || whyOwner(first, c) != tc.they {
			t.Errorf("%q/%q: why = %q / %q, want %q / %q", tc.mine, tc.theirs, whyOwner(me, c), whyOwner(first, c), tc.mineWhy, tc.they)
		}
		mine, theirs := roleOf(me, c), roleOf(first, c)
		keep, yield := mine, theirs
		if !tc.meKeeps {
			keep, yield = theirs, mine
		}
		if !strings.HasPrefix(keep, keepRole) || !strings.HasPrefix(yield, yieldRole) {
			t.Errorf("%q/%q: me %q, first %q: want exactly one to keep (me: %v)", tc.mine, tc.theirs, mine, theirs, tc.meKeeps)
		}
	}
	s := NewSession("s1", "/repo", 0)
	c.Stances = [2]core.Stance{}
	if ctx := contextFor(t, me, c); !strings.Contains(ctx, " You yield.\n") {
		t.Fatalf("no stances: %q", ctx)
	}
	Respond("PostToolUse", "", "", s, me, View{Conflicts: []core.Conflict{c}})
	c.Stances[1-i] = core.StanceYield
	ctx := ctxOf(t, Respond("Stop", "", "", s, me, View{Conflicts: []core.Conflict{c}}))
	if !strings.Contains(ctx, " You keep it: the other branch yielded it to you.\n") || !strings.Contains(ctx, keepAdvice) {
		t.Fatalf("after the other side yields = %q", ctx)
	}
}

// fakeRegistry is a Claude Code config dir whose registry holds an entry per
// session, as Claude Code writes it.
type fakeRegistry string

func newRegistry(t *testing.T) fakeRegistry {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeRegistry(dir)
}

func (r fakeRegistry) add(t *testing.T, pid int, session, name string, started int64) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": session, "name": name, "nameSource": "derived", "startedAt": started})
	if err := os.WriteFile(filepath.Join(string(r), "sessions", fmt.Sprintf("%d.json", pid)), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r fakeRegistry) remove(t *testing.T, pid int) {
	t.Helper()
	if err := os.Remove(filepath.Join(string(r), "sessions", fmt.Sprintf("%d.json", pid))); err != nil {
		t.Fatal(err)
	}
}

// office is a daemon that never runs, with worktrees as a scan would leave
// them, for the office's own logic.
type office struct {
	d   *Daemon
	reg fakeRegistry
}

func newOffice(t *testing.T) office {
	t.Helper()
	d, err := New(Config{StateDir: shortDir(t), Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return office{d: d, reg: newRegistry(t)}
}

// worktree tracks a real directory, so paths under it resolve, on branch of
// repo, with files in flight.
func (o office) worktree(t *testing.T, repo, branch string, files ...string) (string, core.WorkstreamID) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := WorkstreamIDFor(o.d.id, root, branch)
	o.d.worktrees[root] = &worktree{root: root, ws: ws, snap: &gitscan.Snapshot{Root: root, Repo: repo, Branch: branch, Files: files}}
	o.d.local, _ = core.Apply(o.d.local, o.d.fullStateLocked())
	return root, ws
}

// session attaches a live session to root, named name in the registry, with
// its PID known to the daemon or not.
func (o office) session(t *testing.T, id, root string, pid int, name string, knownPID bool) *Session {
	t.Helper()
	known := 0
	if knownPID {
		known = pid
	}
	s := NewSession(id, root, known)
	s.ClaudeDir = string(o.reg)
	o.d.sessions[id] = s
	o.reg.add(t, pid, id, name, int64(pid))
	return s
}

// conflict between a and b, both of this daemon's, on path, as the detector
// publishes it.
func (o office) conflict(a, b core.WorkstreamID, path string) core.Conflict {
	if b < a {
		a, b = b, a
	}
	branch := map[core.WorkstreamID]string{}
	for _, wt := range o.d.worktrees {
		branch[wt.ws] = wt.snap.Branch
	}
	c := core.Conflict{ID: core.ConflictID("r", path, a, b), Repo: "r", Path: path, Kind: "content",
		Sides: [2]core.Side{{Daemon: o.d.id, Workstream: a, Branch: branch[a]}, {Daemon: o.d.id, Workstream: b, Branch: branch[b]}}}
	o.d.conflicts[c.ID] = c
	o.d.spans[c.ID] = c
	return c
}

func (o office) ask(t *testing.T, req wire.Request, out any) wire.Response {
	t.Helper()
	resp := o.d.handle(req)
	if resp.Error == "" && out != nil {
		if err := json.Unmarshal(resp.Output, out); err != nil {
			t.Fatalf("%s answer %s: %v", req.Type, resp.Output, err)
		}
	}
	return resp
}

// TestOfficeView: office_who lists every other workstream of the repo with
// files in flight or a session open, with the session to message, its files
// and its conflicts with the asking worktree; never the asking one, another
// repo's, or one with neither files nor a live session.
func TestOfficeView(t *testing.T) {
	o := newOffice(t)
	rootA, wsA := o.worktree(t, "r", "feat-a", "a.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "b.go", "shared.go")
	rootC, _ := o.worktree(t, "r", "feat-c") // a session, nothing in flight yet
	rootD, _ := o.worktree(t, "r", "feat-d") // nothing at all
	rootE, _ := o.worktree(t, "other", "feat-e", "e.go")
	o.session(t, "sA", rootA, 101, "a-1", true)
	o.session(t, "sB", rootB, 102, "b-2", false)
	o.session(t, "sC", rootC, 103, "c-3", true)
	o.session(t, "sE", rootE, 105, "e-5", true)
	dead := o.session(t, "sD", rootD, 104, "d-4", true)
	o.reg.remove(t, 104) // its Claude Code exited without a SessionEnd
	o.conflict(wsA, wsB, "shared.go")

	var got wire.Office
	if resp := o.ask(t, wire.Request{Type: wire.TypeOffice, Path: filepath.Join(rootA, "sub", "x.go")}, &got); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if !got.Tracked || got.Branch != "feat-a" || len(got.Workstreams) != 2 {
		t.Fatalf("office = %+v, want feat-b and feat-c", got)
	}
	byBranch := map[string]wire.OfficeWorkstream{}
	for _, w := range got.Workstreams {
		byBranch[w.Branch] = w
	}
	b, c := byBranch["feat-b"], byBranch["feat-c"]
	if b.Session != "b-2" || strings.Join(b.Files, ",") != "b.go,shared.go" || len(b.Conflicts) != 1 || b.Conflicts[0].Path != "shared.go" ||
		b.Conflicts[0].Keeps != (min(wsA, wsB) == wsA) || len(b.Conflicts[0].ID) != 8 {
		t.Fatalf("feat-b = %+v", b)
	}
	if c.Session != "c-3" || len(c.Files) != 0 {
		t.Fatalf("feat-c = %+v", c)
	}
	if o.d.sessions["sB"].PID != 102 {
		t.Fatalf("sB's PID from the registry = %d, want 102", o.d.sessions["sB"].PID)
	}
	if dead.PID != 104 {
		t.Fatal("a dead session lost its PID") // pruneSessions drops it on its own
	}
	var none wire.Office
	o.ask(t, wire.Request{Type: wire.TypeOffice, Path: "/nowhere"}, &none)
	if none.Tracked {
		t.Fatalf("untracked path = %+v", none)
	}
}

// TestViewForOffice: a hook's view names the session of every other side on
// this machine and its office, those of the repo that share no file.
func TestViewForOffice(t *testing.T) {
	o := newOffice(t)
	rootA, wsA := o.worktree(t, "r", "feat-a", "shared.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "shared.go")
	rootC, wsC := o.worktree(t, "r", "feat-c")
	o.session(t, "sA", rootA, 101, "a-1", true)
	o.session(t, "sB", rootB, 102, "b-2", true)
	o.session(t, "sB2", rootB, 202, "b-older", true)
	o.reg.add(t, 202, "sB2", "b-older", 1) // started before sB: sB names the worktree
	o.session(t, "sC", rootC, 103, "c-3", true)
	o.conflict(wsA, wsB, "shared.go")

	v := o.d.viewFor(wsA)
	if len(v.Overlaps) != 1 || len(v.Conflicts) != 1 || len(v.Office) != 1 {
		t.Fatalf("view = %+v", v)
	}
	if other := v.Conflicts[0].Sides[1-mineOf(v.Conflicts[0].Sides, wsA)]; other.Workstream != wsB || other.Session != "b-2" {
		t.Fatalf("conflict's other side = %+v", other)
	}
	if other := v.Overlaps[0].Sides[1-mineOf(v.Overlaps[0].Sides, wsA)]; other.Session != "b-2" {
		t.Fatalf("overlap's other side = %+v", other)
	}
	if v.Office[0].Workstream != wsC || v.Office[0].Session != "c-3" {
		t.Fatalf("office = %+v", v.Office)
	}
	o.reg.remove(t, 103)
	if v := o.d.viewFor(wsA); len(v.Office) != 0 {
		t.Fatalf("a workstream with nothing in flight and no session is in the office: %+v", v.Office)
	}
}

// TestClaimYield: office_claim and office_yield record each side's stance on
// a conflict between two workstreams of this machine; the answer says who
// keeps the span now, how, and the other side's session; both sides' views
// carry it, and both claiming falls to the keeper rule.
func TestClaimYield(t *testing.T) {
	o := newOffice(t)
	rootA, wsA := o.worktree(t, "r", "feat-a", "shared.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "shared.go")
	o.session(t, "sA", rootA, 101, "a-1", true)
	o.session(t, "sB", rootB, 102, "b-2", true)
	c := o.conflict(wsA, wsB, "shared.go")
	keeper, yielder, yielderSession, keeperSession := c.Keeper(), wsB, "sB", "sA"
	if keeper == wsB {
		yielder, yielderSession, keeperSession = wsA, "sA", "sB"
	}
	stance := func(typ, session string) wire.Stance {
		t.Helper()
		var st wire.Stance
		if resp := o.ask(t, wire.Request{Type: typ, SessionID: session, Conflict: c.ID[:8]}, &st); resp.Error != "" {
			t.Fatalf("%s by %s: %s", typ, session, resp.Error)
		}
		return st
	}
	ownerFrom := func(ws core.WorkstreamID) core.WorkstreamID {
		t.Helper()
		for _, vc := range o.d.viewFor(ws).Conflicts {
			if vc.ID == c.ID {
				return vc.Owner()
			}
		}
		t.Fatalf("%s's view lacks the conflict", ws)
		return ""
	}
	owner := func() core.WorkstreamID {
		t.Helper()
		k := ownerFrom(keeper)
		if y := ownerFrom(yielder); y != k {
			t.Fatalf("the two sides see different owners: %s, %s", k, y)
		}
		return k
	}

	st := stance(wire.TypeClaim, yielderSession)
	if !st.Keeps || st.Why != "you claimed it" || st.ID != c.ID[:8] || st.Path != "shared.go" || st.Session == "" || owner() != yielder {
		t.Fatalf("the yielder claims: %+v, owner %s", st, owner())
	}
	st = stance(wire.TypeClaim, keeperSession)
	if !st.Keeps || !strings.HasPrefix(st.Why, "both branches claimed it") || owner() != keeper {
		t.Fatalf("both claim: %+v, owner %s", st, owner())
	}
	st = stance(wire.TypeYield, keeperSession)
	if st.Keeps || st.Why != "you yielded it" || owner() != yielder {
		t.Fatalf("the keeper yields: %+v, owner %s", st, owner())
	}

	for _, tc := range []struct {
		name string
		req  wire.Request
		want string
	}{
		{"unknown session", wire.Request{Type: wire.TypeClaim, SessionID: "nope", Conflict: c.ID}, "does not know that session"},
		{"no conflict id", wire.Request{Type: wire.TypeClaim, SessionID: "sA"}, "conflict_id is missing"},
		{"unknown conflict", wire.Request{Type: wire.TypeYield, SessionID: "sA", Conflict: "zzzz"}, "does not see that conflict"},
	} {
		if resp := o.d.handle(tc.req); !strings.Contains(resp.Error, tc.want) {
			t.Errorf("%s: %+v, want an error with %q", tc.name, resp, tc.want)
		}
	}

	// A conflict with another machine's workstream takes no stance: its
	// daemon would never see it.
	far := core.Conflict{ID: core.ConflictID("r", "far.go", wsA, "zz:far"), Repo: "r", Path: "far.go",
		Sides: [2]core.Side{{Daemon: o.d.id, Workstream: wsA}, {Daemon: "zz", Workstream: "zz:far"}}}
	o.d.conflicts[far.ID] = far
	if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: "sA", Conflict: far.ID}); !strings.Contains(resp.Error, "another machine") {
		t.Fatalf("remote claim = %+v", resp)
	}

	// While paused nothing is recorded.
	if err := os.WriteFile(statedir.PausedPath(o.d.cfg.StateDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: keeperSession, Conflict: c.ID}); !strings.Contains(resp.Error, "paused") || owner() != yielder {
		t.Fatalf("paused claim = %+v, owner %s", resp, owner())
	}
	os.Remove(statedir.PausedPath(o.d.cfg.StateDir))

	// A span that resolved still takes a stance, which holds if it comes
	// back: the side that yields usually aligns first and records it after.
	delete(o.d.conflicts, c.ID)
	st = stance(wire.TypeClaim, keeperSession)
	if !st.Resolved || !st.Keeps || !strings.HasPrefix(st.Why, "both branches claimed it") { // the yielder's claim stands
		t.Fatalf("claim on a resolved span = %+v", st)
	}
	o.d.conflicts[c.ID] = c
	if owner() != keeper {
		t.Fatalf("the span came back with owner %s, want %s", owner(), keeper)
	}

	// A workstream that goes takes its spans and stances with it.
	delete(o.d.worktrees, rootB)
	o.d.pruneStances()
	if len(o.d.stances) != 0 || len(o.d.spans) != 0 {
		t.Fatalf("after feat-b went: stances %v, spans %v", o.d.stances, o.d.spans)
	}
}

// TestStanceDiesWithEitherSide: a stance by A does not outlive B's worktree,
// so a B that comes back on the same root and branch starts from the keeper
// rule.
func TestStanceDiesWithEitherSide(t *testing.T) {
	o := newOffice(t)
	rootA, wsA := o.worktree(t, "r", "feat-a", "shared.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "shared.go")
	o.session(t, "sA", rootA, 101, "a-1", true)
	o.session(t, "sB", rootB, 102, "b-2", true)
	c := o.conflict(wsA, wsB, "shared.go")
	keeper, other, session := c.Keeper(), wsB, "sB"
	if keeper == wsB {
		other, session = wsA, "sA"
	}
	// the side that would yield claims alone, so it owns the span
	if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: session, Conflict: c.ID}); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if c.Owner() == other || len(o.d.stances[c.ID]) != 1 {
		t.Fatalf("setup: stances %v", o.d.stances)
	}
	claimedBy := other

	gone := rootB
	if claimedBy == wsB {
		gone = rootA
	}
	root := gone
	wt := o.d.worktrees[root]
	delete(o.d.worktrees, root)
	o.d.pruneStances()
	if len(o.d.stances) != 0 {
		t.Fatalf("a stance outlived its pair: %v", o.d.stances)
	}

	o.d.worktrees[root] = wt
	o.d.conflicts[c.ID] = c
	for _, vc := range o.d.viewFor(keeper).Conflicts {
		if vc.ID == c.ID && vc.Owner() != keeper {
			t.Fatalf("a returning workstream starts with owner %s, want the keeper %s", vc.Owner(), keeper)
		}
	}
}

// TestClaimRehandsOtherSide: a stance hands the conflict over again to the
// other side's session, which reads the new owner at its next hook, and to
// the one that said it too, with its new role.
func TestClaimRehandsOtherSide(t *testing.T) {
	o := newOffice(t)
	rootA, wsA := o.worktree(t, "r", "feat-a", "shared.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "shared.go")
	sA := o.session(t, "sA", rootA, 101, "a-1", true)
	sB := o.session(t, "sB", rootB, 102, "b-2", true)
	c := o.conflict(wsA, wsB, "shared.go")
	for _, x := range []struct {
		s  *Session
		ws core.WorkstreamID
	}{{sA, wsA}, {sB, wsB}} {
		if ctx := ctxOf(t, Respond("PostToolUse", "", "", x.s, x.ws, o.d.viewFor(x.ws))); !strings.Contains(ctx, "conflict") {
			t.Fatalf("%s's first notice = %q", x.s.ID, ctx)
		}
	}
	claimer, other, otherWS := sA, sB, wsB
	if c.Keeper() == wsA {
		claimer, other, otherWS = sB, sA, wsA
	}
	if resp := o.d.handle(wire.Request{Type: wire.TypeClaim, SessionID: claimer.ID, Conflict: c.ID}); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	ctx := ctxOf(t, Respond("PostToolUse", "", "", other, otherWS, o.d.viewFor(otherWS)))
	if !strings.Contains(ctx, " You yield: the other branch claimed it.") {
		t.Fatalf("other side after the claim = %q", ctx)
	}
	claimerWS := wsA
	if claimer == sB {
		claimerWS = wsB
	}
	if ctx := ctxOf(t, Respond("PostToolUse", "", "", claimer, claimerWS, o.d.viewFor(claimerWS))); !strings.Contains(ctx, "you claimed it") {
		t.Fatalf("the claimer after its claim = %q", ctx)
	}
}

// TestHookLearnsSessionName: through the socket, a session's hooks name its
// Claude Code config dir, and the other worktree's notices name the session
// as the registry does.
func TestHookLearnsSessionName(t *testing.T) {
	f, wt := overlap(t)
	reg := newRegistry(t)
	// Live PIDs: the daemon drops a session whose Claude Code is gone.
	pidA, pidB := os.Getpid(), os.Getppid()
	reg.add(t, pidA, "sA", "clone-1a", 1)
	reg.add(t, pidB, "sB", "wtb-2b", 2)
	r := startDaemon(t, "")
	send := func(event, session, cwd string, pid int) string {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": event})
		resp := call(t, r.sock, wire.Request{Type: wire.TypeHook, Event: event, Input: in, ClaudePID: pid, ClaudeDir: string(reg)})
		if resp.Error != "" {
			t.Fatal(resp.Error)
		}
		return string(resp.Output)
	}
	send("SessionStart", "sA", f.Clone, pidA)
	send("SessionStart", "sB", wt, pidB)
	eventually(t, 15*time.Second, "sA's notice names sB's session", func() bool {
		out := send("PostToolUse", "sA", f.Clone, 0)
		return strings.Contains(out, `branch \"feat-b\" (daemon `) && strings.Contains(out, `session \"wtb-2b\"`)
	})
	var got wire.Office
	resp := call(t, r.sock, wire.Request{Type: wire.TypeOffice, Path: wt})
	if err := json.Unmarshal(resp.Output, &got); err != nil || len(got.Workstreams) != 1 || got.Workstreams[0].Session != "clone-1a" {
		t.Fatalf("office from feat-b = %s (%v)", resp.Output, err)
	}
}

// TestYielderHearsAgainAtStop: a side that yielded a conflict that still
// stands is handed it again, at its Stop, once it edited its lines or the
// other side's changed since it was last handed; a Stop with nothing changed
// says nothing, and the keeper is not handed it at all.
func TestYielderHearsAgainAtStop(t *testing.T) {
	snip := func(l string) []core.Snippet { return []core.Snippet{{Lines: []string{l}}} }
	c := conflictWith(first, "x.go", core.Snippet{Lines: []string{"theirs := 1"}})
	i := mineOf(c.Sides, me)
	c.Stances[i] = core.StanceYield // me yields
	if c.Owner() == me {
		t.Fatalf("setup: me owns %+v", c.Stances)
	}
	s := NewSession("s", "/repo", 0)
	view := func(c core.Conflict) View { return View{Conflicts: []core.Conflict{c}} }
	if out := Respond("UserPromptSubmit", "", "", s, me, view(c)); len(out) == 0 {
		t.Fatal("the conflict was not handed at the turn's start")
	}
	if out := Respond("Stop", "", "", s, me, view(c)); len(out) != 0 {
		t.Fatalf("a Stop with nothing changed said %v", out)
	}
	edited := c
	edited.Lines[i] = []core.Range{{Start: 4, Len: 3}}
	out := Respond("Stop", "", "", s, me, view(edited))
	if ctx, _ := json.Marshal(out); !strings.Contains(string(ctx), "theirs := 1") {
		t.Fatalf("the yielder's edit at Stop handed %s, want the conflict", ctx)
	}
	if out := Respond("Stop", "", "", s, me, view(edited)); len(out) != 0 {
		t.Fatalf("the next Stop, unchanged, said %v", out)
	}
	moved := edited
	moved.Text[1-i] = snip("theirs := 2")
	if out := Respond("Stop", "", "", s, me, view(moved)); len(out) == 0 {
		t.Fatal("the other side's change at Stop was not handed")
	}
	rewritten := moved
	rewritten.Text[i] = snip("mine := 2")
	if out := Respond("Stop", "", "", s, me, view(rewritten)); len(out) == 0 {
		t.Fatal("the yielder's rewrite on the same line range was not handed at Stop")
	}

	// The keeper's edits do not bring it back at Stop.
	k := conflictWith(first, "x.go", core.Snippet{Lines: []string{"theirs := 1"}})
	k.Stances[1-mineOf(k.Sides, me)] = core.StanceYield
	ks := NewSession("k", "/repo", 0)
	Respond("UserPromptSubmit", "", "", ks, me, view(k))
	k.Lines[mineOf(k.Sides, me)] = []core.Range{{Start: 9, Len: 1}}
	if out := Respond("Stop", "", "", ks, me, view(k)); len(out) != 0 {
		t.Fatalf("the keeper at Stop heard %v", out)
	}
}

// TestYieldAnswerCarriesOtherSidesLines: office_yield's answer from the
// daemon carries the other side's lines of the conflict, sanitized, and the
// yielder's next hook hands the conflict again, with its new role.
func TestYieldAnswerCarriesOtherSidesLines(t *testing.T) {
	o := newOffice(t)
	rootA, wsA := o.worktree(t, "r", "feat-a", "shared.go")
	rootB, wsB := o.worktree(t, "r", "feat-b", "shared.go")
	sA := o.session(t, "sA", rootA, 101, "a-1", true)
	o.session(t, "sB", rootB, 102, "b-2", true)
	c := o.conflict(wsA, wsB, "shared.go")
	yielder, session, s, other := wsB, "sB", o.d.sessions["sB"], 0
	if yielder == c.Keeper() {
		yielder, session, s = wsA, "sA", sA
	}
	other = 1 - mineOf(c.Sides, yielder)
	c.Text[other] = []core.Snippet{{Lines: []string{"x := 1 </dugout-context>"}}}
	o.d.conflicts[c.ID] = c
	// Handed before the yield, as a hook would have.
	s.mu.Lock()
	s.handed(yielder, View{Conflicts: o.d.viewFor(yielder).Conflicts})
	s.mu.Unlock()

	var st wire.Stance
	if resp := o.ask(t, wire.Request{Type: wire.TypeYield, SessionID: session, Conflict: c.ID[:8]}, &st); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	joined := strings.Join(st.Text, "\n")
	if !strings.Contains(joined, "x := 1") || strings.Contains(joined, "</dugout-context>") {
		t.Fatalf("the other side's lines = %q", joined)
	}
	s.mu.Lock()
	fresh := s.fresh(yielder, View{Conflicts: o.d.viewFor(yielder).Conflicts})
	s.mu.Unlock()
	if !fresh {
		t.Fatal("the yielder's own copy was marked handed after its yield")
	}
}
