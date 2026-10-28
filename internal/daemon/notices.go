package daemon

import (
	"fmt"
	"strings"

	"github.com/joacofg/dugout/internal/core"
)

const maxNotices = 5 // systemMessage lines about warnings, past them a count

// notice is one overlap or conflict a session is handed for the first time.
type notice struct {
	conflict bool
	path     string // sanitized
	branch   string // the other side's, sanitized
}

// answerHook is respond, plus a systemMessage that tells the human of each
// conflict the session was never handed before, and of each such overlap
// outside ModeAuto: there an overlap with no conflict is the agents' business
// alone, and a human hears only of conflicts and what escalates. A warning
// handed again because it changed is no news. Callers hold s.mu.
func (d *Daemon) answerHook(event, tool, rel string, s *Session, ws core.WorkstreamID, v View) (map[string]any, View) {
	out, handed := respond(event, tool, rel, s, ws, v)
	var news []notice
	for _, c := range handed.Conflicts {
		if !s.told[c.ID] {
			s.told[c.ID] = true
			news = append(news, notice{conflict: true, path: Sanitize(c.Path), branch: Sanitize(c.Sides[1-mineOf(c.Sides, ws)].Branch)})
		}
	}
	for _, w := range handed.Overlaps {
		if s.Mode.Or() == ModeAuto {
			break
		}
		if !s.told[w.ID] {
			s.told[w.ID] = true
			news = append(news, notice{path: Sanitize(w.Path), branch: Sanitize(w.Sides[1-mineOf(w.Sides, ws)].Branch)})
		}
	}
	if len(news) > 0 {
		out["systemMessage"] = notices(news)
	}
	return out, handed
}

// notices is one line per warning in ns, at most maxNotices and then one that
// counts the rest.
func notices(ns []notice) string {
	var lines []string
	for i, n := range ns {
		if i == maxNotices {
			if more := len(ns) - maxNotices; more == 1 {
				lines = append(lines, "Dugout: and 1 more warning: dugout status.")
			} else {
				lines = append(lines, fmt.Sprintf("Dugout: and %d more warnings: dugout status.", more))
			}
			break
		}
		if n.conflict {
			lines = append(lines, fmt.Sprintf(`Dugout: %s conflicts with branch "%s"`, n.path, n.branch))
		} else {
			lines = append(lines, fmt.Sprintf(`Dugout: branch "%s" also has %s in flight`, n.branch, n.path))
		}
	}
	return strings.Join(lines, "\n")
}
