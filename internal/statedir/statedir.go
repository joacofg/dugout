// Package statedir owns the daemon's local state directory: the unix
// socket, the persisted daemon_id and epoch, the tracked
// worktrees and the autonomy mode chosen for each repo.
package statedir

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/core"
)

// EnvHome overrides the state directory, so tests and a second daemon on
// the same machine stay isolated.
const EnvHome = "DUGOUT_HOME"

// MaxSocketPath leaves room for the NUL in macOS's 104-byte sun_path, the
// shorter of the two platforms' (Linux has 108).
const MaxSocketPath = 103

// goos is a var so a test can take the Linux paths on a Mac.
var goos = runtime.GOOS

// Dir returns the state directory without creating it.
func Dir() (string, error) {
	if d := os.Getenv(EnvHome); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return DefaultDir(goos, home, os.Getenv), nil
}

// DefaultDir is the state directory when DUGOUT_HOME is unset. On Linux it
// is $XDG_STATE_HOME/dugout, else ~/.local/state/dugout: the XDG base dirs
// keep in the state home what outlives a restart but is not worth backing up
// or carrying to another machine, and that is all the daemon keeps (its
// daemon_id and epoch, the tracked worktrees, its git store, its log). A
// relative XDG_STATE_HOME is invalid by the spec and ignored. getenv is a
// parameter so install, which writes these paths into the unit, resolves
// them the same way.
func DefaultDir(goos, home string, getenv func(string) string) string {
	if goos == "linux" {
		if x := getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
			return filepath.Join(x, "dugout")
		}
		return filepath.Join(home, ".local", "state", "dugout")
	}
	return filepath.Join(home, "Library", "Application Support", "dugout")
}

// Ensure creates dir with mode 0700.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Socket is the unix socket in the state dir dir.
func Socket(dir string) string { return filepath.Join(dir, "dugout.sock") }

// SocketPath is the unix socket the daemon listens on and hooks dial:
// Socket(Dir()), except on Linux with DUGOUT_HOME unset (see SocketFor).
func SocketPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return SocketFor(goos, dir, os.Getenv), nil
}

// SocketFor is the socket for the state dir dir. On Linux it is
// $XDG_RUNTIME_DIR/dugout.sock when that is set: a per-user 0700 tmpfs that
// systemd gives both the login session and the user's units, with a short
// path (/run/user/<uid>), and emptied at logout, so no stale socket outlives
// the session. A DUGOUT_HOME keeps the socket inside it, as tests and a
// second daemon need; so do a relative XDG_RUNTIME_DIR and one too long for
// a socket.
func SocketFor(goos, dir string, getenv func(string) string) string {
	if goos == "linux" && getenv(EnvHome) == "" {
		if rt := getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(rt) {
			if s := filepath.Join(rt, "dugout.sock"); len(s) <= MaxSocketPath {
				return s
			}
		}
	}
	return Socket(dir)
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// LoadOrCreateID returns the daemon_id persisted in dir. When the file is
// missing, unreadable or not a lowercase UUID v4, it writes a new one and
// reports created=true, so the caller can log the regeneration.
func LoadOrCreateID(dir string) (id core.DaemonID, created bool, err error) {
	path := filepath.Join(dir, "daemon_id")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); uuidV4.MatchString(s) {
			return core.DaemonID(s), false, nil
		}
	}
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", false, err
	}
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(u[:])
	s := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	if err := WriteAtomic(path, []byte(s+"\n")); err != nil {
		return "", false, fmt.Errorf("write daemon_id: %w", err)
	}
	return core.DaemonID(s), true, nil
}

// NextEpoch returns the epoch of the daemon's new run and persists it. serve
// orders a daemon's runs by it and ignores a run below the last it took,
// so it must never go back: it is one more than the epoch persisted in dir,
// and at least now in milliseconds, which keeps it above the runs before
// when the file was lost or restored from an older copy. A clock before 1970
// gives no floor; the first epoch is 1, never 0. reset reports a file that
// held no number: the epoch then comes from the clock alone.
func NextEpoch(dir string, now time.Time) (epoch uint64, reset bool, err error) {
	path := filepath.Join(dir, "epoch")
	var last uint64
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, false, err
	default:
		if last, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err != nil {
			last, reset = 0, true
		}
	}
	if last == math.MaxUint64 {
		return 0, false, fmt.Errorf("%s: no epoch after %d", path, last)
	}
	epoch = max(last+1, uint64(max(now.UnixMilli(), 0)))
	if err := WriteAtomic(path, []byte(strconv.FormatUint(epoch, 10)+"\n")); err != nil {
		return 0, false, fmt.Errorf("write %s: %w", path, err)
	}
	return epoch, reset, nil
}

type worktreesFile struct {
	Worktrees []string `json:"worktrees"`
}

// LoadWorktrees returns the tracked worktree roots; a missing file is nil.
func LoadWorktrees(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "worktrees.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f worktreesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse worktrees.json: %w", err)
	}
	return f.Worktrees, nil
}

// SaveWorktrees replaces the tracked worktree roots, sorted.
func SaveWorktrees(dir string, roots []string) error {
	sorted := slices.Clone(roots)
	slices.Sort(sorted)
	b, err := json.MarshalIndent(worktreesFile{Worktrees: slices.Compact(sorted)}, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(dir, "worktrees.json"), append(b, '\n'))
}

type modesFile struct {
	Modes map[string]string `json:"modes"`
}

// LoadModes returns the autonomy mode chosen for each repo, by repo id; a
// missing file is none chosen.
func LoadModes(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "modes.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f modesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse modes.json: %w", err)
	}
	if f.Modes == nil {
		f.Modes = map[string]string{}
	}
	return f.Modes, nil
}

// SaveModes replaces the autonomy mode of each repo.
func SaveModes(dir string, modes map[string]string) error {
	b, err := json.MarshalIndent(modesFile{Modes: modes}, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(dir, "modes.json"), append(b, '\n'))
}

// rename is a seam so tests can fail the swap step.
var rename = os.Rename

// WriteAtomic writes b to a temp file next to path with mode 0600, syncs it
// and renames it over path, then syncs the directory so the rename outlives
// a crash.
func WriteAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// PausedPath is the file whose presence pauses Dugout on this machine.
func PausedPath(dir string) string { return filepath.Join(dir, "paused") }
