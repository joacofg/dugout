package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/gitscan"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

func statusOf(t *testing.T, d *Daemon) wire.Status {
	t.Helper()
	resp := d.handle(wire.Request{Type: wire.TypeStatus})
	var st wire.Status
	if resp.Error != "" || json.Unmarshal(resp.Output, &st) != nil {
		t.Fatalf("status = %+v", resp)
	}
	return st
}

// TestStatusAnswer: the worktrees with files in flight, by root, with what
// touches each, its #N when delivered, the other side's current intent and
// the pause.
func TestStatusAnswer(t *testing.T) {
	d := idle(t, shortDir(t))
	a := &worktree{root: "/r/a", ws: WorkstreamIDFor(d.id, "/r/a", "feat"), snap: &gitscan.Snapshot{Repo: "app", Branch: "feat", Files: []string{"x.go", "y.go"}}, intent: "fix the login"}
	b := &worktree{root: "/r/b", ws: WorkstreamIDFor(d.id, "/r/b", "other"), snap: &gitscan.Snapshot{Repo: "app", Branch: "other", Files: []string{"x.go"}}, intent: `charge "new"`}
	empty := &worktree{root: "/r/c", ws: WorkstreamIDFor(d.id, "/r/c", "vacia"), snap: &gitscan.Snapshot{Repo: "app", Branch: "vacia"}}
	d.worktrees = map[string]*worktree{"/r/b": b, "/r/a": a, "/r/c": empty, "/r/unscanned": {root: "/r/unscanned"}}
	d.notifyIfChanged()
	remote := core.Side{Daemon: "feedfacecafebeef", Workstream: "feedfacecafebeef:1", Branch: "feat/auth", Intent: "old"}
	c := core.Conflict{ID: "conflict-1", Repo: "app", Path: "y.go", Kind: "text",
		Sides: [2]core.Side{{Daemon: d.id, Workstream: a.ws, Branch: "feat"}, remote},
		Lines: [2][]core.Range{{{Start: 3, Len: 3}}, nil}}
	d.conflicts[c.ID] = c
	if err := os.WriteFile(statedir.PausedPath(d.cfg.StateDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	st := statusOf(t, d)
	if st.Daemon != string(d.id) || st.Server != "" || st.Connected || st.Paused != "local" || len(st.Workstreams) != 2 {
		t.Fatalf("status = %+v", st)
	}
	wa, wb := st.Workstreams[0], st.Workstreams[1]
	if wa.Root != "/r/a" || wb.Root != "/r/b" || wa.Repo != "app" || wa.Branch != "feat" || wa.Intent != "fix the login" || len(wa.Files) != 2 {
		t.Fatalf("workstreams = %+v", st.Workstreams)
	}
	if len(wa.Conflicts) != 1 || wa.Conflicts[0] != (wire.StatusConflict{Path: "y.go", Branch: "feat/auth", Daemon: "feedfacecafebeef", Lines: "your lines 3-5", Intent: "old"}) {
		t.Fatalf("conflicts = %+v", wa.Conflicts)
	}
	if len(wa.Overlaps) != 1 || wa.Overlaps[0] != (wire.StatusOverlap{Path: "x.go", Branch: "other", Daemon: string(d.id), Intent: "charge 'new'"}) {
		t.Fatalf("overlaps = %+v", wa.Overlaps)
	}
	if len(wb.Overlaps) != 1 || wb.Overlaps[0].Branch != "feat" || wb.Overlaps[0].Intent != "fix the login" || len(wb.Conflicts) != 0 {
		t.Fatalf("b = %+v", wb)
	}
	if strings.Contains(wa.Intent+wb.Intent, `"`) {
		t.Fatal("intent not sanitized")
	}

	// A team pause wins.
	d.paused.Store(true)
	if got := statusOf(t, d).Paused; got != "team" {
		t.Fatalf("paused = %q, want team", got)
	}
}

func TestStatusNothingInFlight(t *testing.T) {
	st := statusOf(t, idle(t, shortDir(t)))
	if st.Workstreams == nil || len(st.Workstreams) != 0 || st.Paused != "" {
		t.Fatalf("status = %+v", st)
	}
}
