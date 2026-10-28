// Package hook is `dugout hook <event>`: a thin, fail-open client that
// forwards a Claude Code hook event to the local daemon and prints the
// daemon's answer. It never blocks the agent: if the daemon is missing or
// slow it prints {} and tells the human once per session.
package hook

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/systemd"
	"github.com/joacofg/dugout/internal/wire"
)

// Timeout is the hard cap on the whole exchange with the daemon.
const Timeout = 150 * time.Millisecond

// DegradedMessage is the systemMessage shown once per session when the
// daemon does not answer.
const DegradedMessage = "Dugout: the local daemon is not responding, so this session goes on without shared awareness. Run dugout doctor to see what is missing."

// Env is what Run needs from the outside world.
type Env struct {
	Socket    string     // daemon socket
	TempDir   string     // where the once-per-session degraded markers go
	ClaudePID func() int // looked up on SessionStart only
	ClaudeDir string     // Claude Code's config dir, whose registry names the session (peers.Dir)
	Home      string     // the user's home, which holds the LaunchAgent or the systemd unit; "" never kickstarts it
	// Terminal writes an escalation the desktop could not show to the
	// session's terminal; nil writes it to /dev/tty (TerminalNotify).
	Terminal func(text string)
}

// goos is a var so a test can take the Linux kickstart on a Mac.
var goos = runtime.GOOS

// DefaultClaudePID finds the Claude Code process above this hook.
func DefaultClaudePID() int {
	lookup := SysctlLookup
	if goos == "linux" {
		lookup = ProcLookup
	}
	return FindClaudePID(os.Getppid(), lookup)
}

// Run handles one hook event and returns its exit code. It always writes a
// JSON object to stdout and returns 0, even after a panic: Go exits 2 on one,
// which Claude Code reads as "block the tool".
func Run(event string, stdin io.Reader, stdout, _ io.Writer, env Env) int {
	out := []byte("{}")
	func() {
		defer func() { _ = recover() }()
		out = handle(event, stdin, env)
	}()
	stdout.Write(out)
	return 0
}

// forwarded is the part of the hook input the daemon uses. Everything else
// stays in this process: the user's prompt, the file contents that
// PostToolUse carries in tool_input and tool_response, and the text of a
// SendMessage. EditHash stands for what an edit's PreToolUse would write, so
// the daemon can tell whether the edit after a held one changed
// without seeing it.
type forwarded struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd,omitempty"`
	HookEventName string `json:"hook_event_name,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	ToolInput     *struct {
		FilePath     string `json:"file_path,omitempty"`
		NotebookPath string `json:"notebook_path,omitempty"`
		// To is a SendMessage's address, the session it messages.
		To string `json:"to,omitempty"`
	} `json:"tool_input,omitempty"`
	EditHash string `json:"edit_hash,omitempty"`
}

// editTools are the tools whose PreToolUse gets an EditHash: the ones the
// daemon may hold.
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// editHash is an FNV-64a of an edit's tool_input without its path, with its
// keys sorted: the same edit of the same file hashes the same, and the hash
// says nothing of what the edit writes. "" when the input is not an object.
func editHash(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return ""
	}
	delete(fields, "file_path")
	delete(fields, "notebook_path")
	b, err := json.Marshal(fields) // map keys marshal sorted
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf("%016x", h.Sum64())
}

// handle returns what goes to stdout.
func handle(event string, stdin io.Reader, env Env) []byte {
	var raw struct {
		forwarded
		ToolInput json.RawMessage `json:"tool_input"`
	}
	_ = json.NewDecoder(stdin).Decode(&raw)
	in := raw.forwarded
	if len(raw.ToolInput) > 0 {
		_ = json.Unmarshal(raw.ToolInput, &in.ToolInput)
		if event == "PreToolUse" && editTools[in.ToolName] {
			in.EditHash = editHash(raw.ToolInput)
		}
	}
	input, _ := json.Marshal(in)
	session := in.SessionID

	req := wire.Request{Type: wire.TypeHook, Event: event, Input: input, ClaudeDir: env.ClaudeDir}
	if event == "SessionStart" && env.ClaudePID != nil {
		req.ClaudePID = env.ClaudePID()
	}
	resp, err := call(env.Socket, req)
	if err != nil {
		return degraded(env, session)
	}
	if resp.Terminal != "" {
		notify := env.Terminal
		if notify == nil {
			notify = TerminalNotify
		}
		notify(resp.Terminal)
	}
	if resp.Error != "" || len(resp.Output) == 0 || !json.Valid(resp.Output) {
		return []byte("{}")
	}
	return resp.Output
}

// OSC9 is text as an OSC 9 notification, which iTerm2, Ghostty, WezTerm,
// kitty and Windows Terminal show on the desktop: without control characters,
// so the text cannot end the sequence or start another.
func OSC9(text string) string {
	return "\x1b]9;" + strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text) + "\x07"
}

// TerminalNotify writes text as OSC 9 to the session's terminal, the one
// Claude Code runs in: the hook's stdout is Claude Code's, never the
// terminal's. A terminal that ignores OSC 9, or no terminal at all, shows
// nothing, which is why the session is told as well (package daemon).
func TerminalNotify(text string) {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer tty.Close()
	_, _ = tty.WriteString(OSC9(text)) // best effort: the session is told as well
}

func call(sock string, req wire.Request) (wire.Response, error) {
	var resp wire.Response
	deadline := time.Now().Add(Timeout)
	c, err := net.DialTimeout("unix", sock, Timeout)
	if err != nil {
		return resp, err
	}
	defer c.Close()
	if err := c.SetDeadline(deadline); err != nil {
		return resp, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return resp, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return resp, err
	}
	return resp, json.Unmarshal(line, &resp)
}

// degraded returns the systemMessage the first time it runs for a session
// and {} after that, using a marker file per session. That first time
// it also kickstarts the LaunchAgent or the systemd unit.
func degraded(env Env, session string) []byte {
	marker := filepath.Join(env.TempDir, "dugout-degraded-"+fileSafe(session))
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return []byte("{}")
	}
	f.Close()
	kickstart(env.Home)
	b, _ := json.Marshal(map[string]string{"systemMessage": DegradedMessage})
	return b
}

// kickstart asks launchd, or systemd on Linux, to start the daemon when its
// LaunchAgent or unit is installed. It does not wait for launchctl or
// systemctl, whose output goes to the null device: the hook's stdout stays
// the hook's.
func kickstart(home string) {
	if cmd := kickstartCmd(home); cmd != nil && cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// kickstartCmd is the command that starts the installed daemon, or nil.
func kickstartCmd(home string) *exec.Cmd {
	if home == "" {
		return nil
	}
	switch goos {
	case "darwin":
		if _, err := os.Stat(launchd.PlistPath(home)); err != nil {
			return nil
		}
		return exec.Command("launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchd.Label))
	case "linux":
		if _, err := os.Stat(systemd.UnitPath(home, os.Getenv)); err != nil {
			return nil
		}
		// --no-block queues the start and returns, so systemctl goes away
		// soon after the hook even when the daemon is slow to come up.
		return exec.Command("systemctl", "--user", "--no-block", "start", systemd.Unit)
	}
	return nil
}

func fileSafe(s string) string {
	if s == "" {
		return "nosession"
	}
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, s)
}
