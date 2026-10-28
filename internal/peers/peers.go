// Package peers reads Claude Code's registry of live sessions, to learn the
// name another session messages a session at with SendMessage: the one
// --name or /rename sets, or the one Claude Code derives when nobody does.
//
// Claude Code writes the registry to <config dir>/sessions/<pid>.json while
// the session runs (the config dir is $CLAUDE_CONFIG_DIR, else ~/.claude),
// keeps "name" current across a /rename, and removes the file when the
// process exits. The file is not a documented interface: a session whose
// entry cannot be read or does not match simply has no name, and Dugout
// names no address for it.
package peers

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxEntry caps one registry file read; a real one is under a kilobyte.
const maxEntry = 64 << 10

// Peer is one live session in the registry.
type Peer struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	StartedAt int64  `json:"startedAt"` // Unix milliseconds
}

// Dir is the config dir whose registry holds the sessions of a hook's
// process: getenv("CLAUDE_CONFIG_DIR"), else home's .claude; "" when neither
// is known.
func Dir(getenv func(string) string, home string) string {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// Lookup finds session in dir's registry: at pid's entry when pid is known,
// else by scanning every entry. It reports false when dir is "", or when no
// entry for session, with a name, is there: the session ended, its process
// was replaced (a /clear starts a new session in the same process), or the
// registry changed shape.
func Lookup(dir, session string, pid int) (Peer, bool) {
	if dir == "" || session == "" {
		return Peer{}, false
	}
	reg := filepath.Join(dir, "sessions")
	if pid > 0 {
		return read(filepath.Join(reg, strconv.Itoa(pid)+".json"), session)
	}
	entries, err := os.ReadDir(reg)
	if err != nil {
		return Peer{}, false
	}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".json") && e.Type().IsRegular() {
			if p, ok := read(filepath.Join(reg, name), session); ok {
				return p, true
			}
		}
	}
	return Peer{}, false
}

func read(path, session string) (Peer, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Peer{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxEntry))
	if err != nil {
		return Peer{}, false
	}
	var p Peer
	if json.Unmarshal(b, &p) != nil || p.SessionID != session || strings.TrimSpace(p.Name) == "" {
		return Peer{}, false
	}
	return p, true
}
