package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const statusDown = "dugout status: the daemon is not responding: run dugout doctor"

// cmdStatus is `dugout status`: what the daemon sees on this machine, the
// worktrees with files in flight and what touches each, by #N.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	sock, err := statedir.SocketPath()
	if err != nil {
		fmt.Fprintln(stderr, "dugout status:", err)
		return 1
	}
	resp, err := ask(sock, wire.Request{Type: wire.TypeStatus})
	if err != nil {
		fmt.Fprintln(stderr, statusDown)
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintln(stderr, "dugout status:", resp.Error)
		return 1
	}
	var st wire.Status
	if err := json.Unmarshal(resp.Output, &st); err != nil {
		fmt.Fprintln(stderr, "dugout status: unexpected response from the daemon:", err)
		return 1
	}
	renderStatus(stdout, st)
	return 0
}

func renderStatus(w io.Writer, st wire.Status) {
	fmt.Fprintf(w, "Dugout on this machine (daemon %s)\n", shortID(st.Daemon))
	switch {
	case st.Server == "":
		fmt.Fprintln(w, "Server: none")
	case st.Connected:
		fmt.Fprintf(w, "Server: %s (connected)\n", st.Server)
	default:
		fmt.Fprintf(w, "Server: %s (disconnected)\n", st.Server)
	}
	switch st.Paused {
	case "local":
		fmt.Fprintln(w, "Paused: on this machine")
	case "team":
		fmt.Fprintln(w, "Paused: for the whole team")
	}
	fmt.Fprintln(w, judgeLine(st.Judge))
	if len(st.Workstreams) == 0 {
		fmt.Fprintf(w, "\nNothing in flight on this machine.\n")
	}
	for _, ws := range st.Workstreams {
		files := fmt.Sprintf("%d files", len(ws.Files))
		if len(ws.Files) == 1 {
			files = "1 file"
		}
		fmt.Fprintf(w, "\n%s\n  branch \"%s\" in %s, %s in flight\n", ws.Root, ws.Branch, ws.Repo, files)
		if ws.Intent != "" {
			fmt.Fprintf(w, "  Intent: \"%s\"\n", ws.Intent)
		}
		for _, c := range ws.Conflicts {
			fmt.Fprintf(w, "  conflict: %s conflicts with branch \"%s\" (daemon %s)\n", c.Path, c.Branch, shortID(c.Daemon))
			committed := ""
			if c.Tally.Committed {
				committed = "; a side committed or pushed while it stood"
			}
			fmt.Fprintf(w, "    so far: %s%s\n", tallyText(c.Tally), committed)
		}
		for _, o := range ws.Overlaps {
			fmt.Fprintf(w, "  overlap: branch \"%s\" also has %s in flight\n", o.Branch, o.Path)
		}
		for _, dup := range ws.Duplicates {
			fmt.Fprintf(w, "  possible duplicate: branch \"%s\" (daemon %s) declared \"%s\"", dup.Branch, shortID(dup.Daemon), dup.Intent)
			if dup.Reason != "" {
				fmt.Fprintf(w, ": %s", dup.Reason)
			}
			fmt.Fprintln(w)
		}
	}
	if len(st.Totals) == 0 {
		return
	}
	fmt.Fprintf(w, "\nConflicts ended since the daemon started\n")
	for _, t := range st.Totals {
		share := ""
		if s, ok := t.ResolvedShare(); ok {
			share = fmt.Sprintf(" (%.0f%% of those resolved or committed)", 100*s)
		}
		fmt.Fprintf(w, "  %s: %s, %d resolved before a commit or push%s, %d committed while standing, %d gone\n",
			t.Repo, plural(t.Conflicts, "conflict", "conflicts"), t.Resolved, share, t.Committed, t.Gone)
		n := float64(t.Conflicts)
		fmt.Fprintf(w, "    per conflict: %.1f messages, %.1f escalations; %s\n", float64(t.Messages)/n, float64(t.Escalations)/n,
			heldText(t.Held, t.Changed, t.Unchanged))
		for _, e := range st.Ended {
			if e.Repo == t.Repo {
				fmt.Fprintf(w, "    latest: %s, branches \"%s\" and \"%s\": %s; %s\n", e.Path, e.Branches[0], e.Branches[1], outcomeText(e.Outcome), tallyText(e.Tally))
			}
		}
	}
}

// tallyText says what was measured of a conflict: the messages between its
// sides, its escalations and its held edits.
func tallyText(t core.Tally) string {
	return fmt.Sprintf("%s, %s, %s", plural(t.Messages, "message", "messages"), plural(t.Escalations(), "escalation", "escalations"),
		heldText(t.Held, t.Changed, t.Unchanged))
}

// heldText says how many edits Dugout held and what the next edit of each
// did.
func heldText(held, changed, unchanged int) string {
	if held == 0 {
		return "no edit held"
	}
	return fmt.Sprintf("%s held: %d changed after, %d retried unchanged, %d not retried",
		plural(held, "edit", "edits"), changed, unchanged, max(0, held-changed-unchanged))
}

// outcomeText says how a conflict ended (core.Outcome).
func outcomeText(o string) string {
	switch core.Outcome(o) {
	case core.OutcomeResolved:
		return "resolved before a commit or push"
	case core.OutcomeCommitted:
		return "a side committed or pushed while it stood"
	case core.OutcomeGone:
		return "a side's worktree or branch went away"
	}
	return o
}

// plural is n and the noun that agrees with it.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// judgeLine says whether duplicate detection is on, with which judge and at
// what cost so far, or why it is off.
func judgeLine(j wire.StatusJudge) string {
	if j.Name == "" {
		why := j.Off
		if why == "" {
			why = "the daemon does not report it: update dugout"
		}
		return "Duplicate detection: off (" + why + ")"
	}
	line := fmt.Sprintf("Duplicate detection: on, judge %s: %d comparisons, %d duplicates, %d input and %d output tokens, $%.4f",
		j.Name, j.Comparisons, j.Duplicates, j.InputTokens, j.OutputTokens, j.USD)
	if j.Unpriced > 0 {
		line += fmt.Sprintf(" (%d comparisons of a model with no known price left out)", j.Unpriced)
	}
	if j.Errors > 0 {
		line += fmt.Sprintf("; %d failed, the last: %s", j.Errors, j.LastError)
	}
	return line
}

// shortID is the first 8 characters of a daemon id.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
