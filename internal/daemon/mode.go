package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

// Mode is how much a person lets its agents settle a conflict with the other
// agents on their own, chosen once per repo: like Claude Code's auto mode, the
// default acts on its own and stops only for what is risky. The zero Mode is
// one never chosen, and works as ModeAuto.
type Mode string

const (
	// ModeAuto resolves every conflict with the other side and tells the
	// human afterwards; it stops only on the escalation triggers.
	ModeAuto Mode = "auto"
	// ModeScoped yields within the task and stops only when yielding would
	// drop part of it: the advice before modes existed.
	ModeScoped Mode = "scoped"
	// ModeSupervised asks the human before changing the plan for a peer.
	ModeSupervised Mode = "supervised"
)

// Modes are the modes in the order they are offered.
var Modes = []Mode{ModeAuto, ModeScoped, ModeSupervised}

// ParseMode reads a mode's name, in any case.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range Modes {
		if m == known {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown mode %q: auto, scoped or supervised", s)
}

// Or is m, or ModeAuto when m was never chosen.
func (m Mode) Or() Mode {
	if m == "" {
		return ModeAuto
	}
	return m
}

// Summary says in one line what m lets the agent do, for the person choosing.
func (m Mode) Summary() string {
	switch m.Or() {
	case ModeScoped:
		return "your agent yields or aligns within its task and escalates to you and the other side only if yielding would leave it without part of the task."
	case ModeSupervised:
		return "your agent asks you before changing its plan for another branch."
	default:
		return "your agent settles conflicts with the other agents and tells you; it escalates to you and the other side only what is risky."
	}
}

// modeLine tells the agent, at the start of a session, its repo's mode and
// what it means, and how a conflict reaches both humans (escalateLine); it is
// Dugout's own instruction, after the block.
func modeLine(m Mode) string {
	chosen := " (your human changes it with /dugout)"
	if m == "" {
		chosen = " (default; your human changes it with /dugout)"
	}
	head := fmt.Sprintf("Dugout: mode %s in this repo%s.", m.Or(), chosen)
	switch m.Or() {
	case ModeScoped:
		return head + " In a conflict where your branch yields, align or drop that span within the scope of your task; escalate it only if yielding would force you to leave part of your task undone." + escalateLine
	case ModeSupervised:
		return head + " Before changing your plan for another branch's, ask your human and wait for the answer." + escalateLine
	default:
		return head + " Settle conflicts with other branches on your own and tell your human how afterwards; escalate only what is risky, as each warning says." + escalateLine
	}
}

// escalateLine ends modeLine: how a conflict reaches both humans. The tool's
// description says the rest, and the warnings when to; it stays short, since
// every SessionStart pays for it out of the answer's budget.
const escalateLine = " Escalate to both humans with office_escalate, one question at a time; Dugout escalates a conflict that stands too long on its own."

const (
	// autoTriggers are the only things an agent in ModeAuto escalates to both
	// humans: what a peer cannot settle, and what is risky to settle alone.
	autoTriggers = " if the two tasks ask for contradictory things in that span, if yielding would force you to leave part of your task undone, or if aligning means deleting something or changing a shared contract (an API, a schema, a migration)"
	autoEscalate = " Escalate it to both humans with office_escalate only" + autoTriggers + "; otherwise tell your human at the end, in one line, how you settled it."
	// askFirst is ModeSupervised's: the yielder proposes and waits.
	askFirst = ", and wait for the answer."
)

// denyYield is the part of a deny's tail for where ws yields, in m.
func denyYield(m Mode, shown bool) string {
	switch m.Or() {
	case ModeScoped:
		return align(" Where your branch yields, retry the edit aligned with the other one", shown) + ", or without that span" + yieldScope + escalate
	case ModeSupervised:
		return align(" Where your branch yields, do not retry it without your human: propose retrying it aligned with the other one", shown) + ", or without that span" + askFirst
	default:
		return align(" Where your branch yields, retry the edit aligned with the other one", shown) + ", or without that span, without asking your human." + autoEscalate
	}
}

// contextYield is the part of a context's tail for where ws yields, in m.
func contextYield(m Mode, shown bool) string {
	switch m.Or() {
	case ModeScoped:
		return align("Where your branch yields, before committing align your change with the other one's", shown) + ", or drop that span from your change" + yieldScope + escalate
	case ModeSupervised:
		return align("Where your branch yields, do not change your plan without your human: before committing propose aligning your change with the other one's", shown) + ", or dropping that span from your change" + askFirst
	default:
		return align("Where your branch yields, settle it yourself before committing: align your change with the other one's", shown) + ", or drop that span from your change, without asking your human." + autoEscalate
	}
}

// modeFor is the mode of the repo of the worktree at root: the repo its last
// scan named, else, while its first scan runs, the one git names, asked once.
func (d *Daemon) modeFor(root string) Mode {
	d.mu.Lock()
	wt := d.worktrees[root]
	if wt == nil || wt.snap != nil || wt.repo != "" {
		defer d.mu.Unlock()
		if wt == nil {
			return ""
		}
		if wt.snap != nil {
			return d.modes[wt.snap.Repo]
		}
		return d.modes[wt.repo]
	}
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, repoTimeout)
	repo, err := gitscan.RepoID(ctx, root)
	cancel()
	if err != nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	wt.repo = repo
	return d.modes[repo]
}

// handleMode tells the mode of the repo that holds req.Path and, with
// req.Text, first sets it: saved to modes.json before it takes effect, so a
// mode the daemon answered outlives a restart. The repo need not be tracked
// yet; its sessions take the new mode at their next hook.
func (d *Daemon) handleMode(req wire.Request) wire.Response {
	if !filepath.IsAbs(req.Path) {
		return wire.Response{Error: "the path must be absolute"}
	}
	ctx, cancel := context.WithTimeout(d.ctx, scanTimeout)
	defer cancel()
	root, err := gitscan.Root(ctx, req.Path)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	repo, err := gitscan.RepoID(ctx, root)
	if err != nil {
		return wire.Response{Error: err.Error()}
	}
	d.modeMu.Lock()
	defer d.modeMu.Unlock()
	d.mu.Lock()
	m := d.modes[repo]
	d.mu.Unlock()
	if req.Text != "" {
		if m, err = ParseMode(req.Text); err != nil {
			return wire.Response{Error: err.Error()}
		}
		d.mu.Lock()
		modes := maps.Clone(d.modes)
		d.mu.Unlock()
		modes[repo] = m
		stored := make(map[string]string, len(modes))
		for r, mm := range modes {
			stored[r] = string(mm)
		}
		if err := statedir.SaveModes(d.cfg.StateDir, stored); err != nil {
			return wire.Response{Error: "could not save the mode: " + err.Error()}
		}
		d.mu.Lock()
		d.modes = modes
		d.mu.Unlock()
		d.log.Info("mode set", "repo", repo, "mode", m)
	}
	out, _ := json.Marshal(wire.Mode{Repo: repo, Mode: string(m)})
	return wire.Response{Output: out}
}
