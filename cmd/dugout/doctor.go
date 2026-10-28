package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joacofg/dugout/internal/judge"
	"github.com/joacofg/dugout/internal/keychain"
	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/snapshot"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const doctorTimeout = 10 * time.Second

// doctor prints one line per check and remembers whether any failed.
type doctor struct {
	out    io.Writer
	failed bool
}

func (d *doctor) ok(format string, a ...any)   { fmt.Fprintf(d.out, "✓ "+format+"\n", a...) }
func (d *doctor) skip(format string, a ...any) { fmt.Fprintf(d.out, "– "+format+"\n", a...) }
func (d *doctor) bad(format string, a ...any) {
	d.failed = true
	fmt.Fprintf(d.out, "✗ "+format+"\n", a...)
}

// cmdDoctor checks the install end to end and says how to fix what is missing.
// It never prints the token. With --bundle it then writes the diagnostic
// bundle, and exits with doctor's own code unless the bundle cannot be written.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bundle := flags.Bool("bundle", false, "also write a diagnostic bundle (.tar.gz) to send to the admin")
	out := flags.String("o", "", "bundle path (with --bundle; default dugout-diag-<date>-<time>.tar.gz in your HOME)")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		return 2
	}
	if *out != "" && !*bundle {
		fmt.Fprintln(stderr, "dugout doctor: -o only goes with --bundle")
		return 2
	}
	if !*bundle {
		return runDoctor(stdout)
	}
	var captured bytes.Buffer
	code := runDoctor(io.MultiWriter(stdout, &captured))
	path, err := writeBundle(*out, captured.Bytes(), time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "dugout doctor: could not write the diagnostic bundle:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Diagnostic bundle: %s. Send it to the admin over a private channel.\n", path)
	return code
}

func runDoctor(stdout io.Writer) int {
	d := &doctor{out: stdout}
	d.ok("dugout %s", version)
	a := d.service()
	d.daemonSocket()
	d.duplicates()
	d.localPause()
	d.serverAndProtocol(a)
	d.git(a)
	d.plugin()
	if d.failed {
		return 1
	}
	return 0
}

// agent is what doctor knows of the daemon launchd or systemd runs. Without
// one installed it is the shell's: server is $DUGOUT_SERVER.
type agent struct {
	installed bool   // a readable LaunchAgent or unit is installed
	kind      string // what runs it, for the messages: "LaunchAgent" or "systemd service"
	server    string // its -server
	insecure  bool   // it was installed with -insecure
	path      string // its PATH
}

// service checks the LaunchAgent, or the systemd unit on Linux.
func (d *doctor) service() agent {
	switch goos {
	case "darwin":
		return d.launchAgent()
	case "linux":
		return d.systemdUnit()
	}
	d.skip("installed daemon: only on macOS and Linux")
	return agent{server: os.Getenv(envServer)}
}

// launchAgent checks the LaunchAgent and reads back the -server and PATH it
// was installed with.
func (d *doctor) launchAgent() agent {
	a := agent{kind: "LaunchAgent", server: os.Getenv(envServer)}
	home, err := os.UserHomeDir()
	if err != nil {
		d.bad("could not locate your HOME: %v", err)
		return a
	}
	raw, err := os.ReadFile(launchd.PlistPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		d.bad("the daemon is not installed: run dugout install -server <url>")
		return a
	}
	p, perr := parsePlist(raw)
	if err != nil || perr != nil || len(p.args) == 0 {
		d.bad("the LaunchAgent is unreadable: run dugout install again")
		return a
	}
	a.installed, a.path = true, p.env["PATH"]
	if s := argAfter(p.args, "-server"); s != "" {
		a.server = s
	}
	a.insecure = slices.Contains(p.args, "-insecure")
	switch {
	case !fileExists(p.args[0]):
		d.bad("the LaunchAgent points to a binary that no longer exists: run dugout install again")
	case launchctl("print", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchd.Label)) != nil:
		d.bad("the LaunchAgent is not loaded: run dugout install again")
	default:
		d.ok("the daemon is installed as a LaunchAgent")
	}
	return a
}

// agentPlist is the part of the LaunchAgent doctor reads back.
type agentPlist struct {
	args []string          // ProgramArguments
	env  map[string]string // EnvironmentVariables
}

// plistNode is any plist element, with its text and children in order.
type plistNode struct {
	XMLName xml.Name
	Text    string      `xml:",chardata"`
	Nodes   []plistNode `xml:",any"`
}

// parsePlist reads ProgramArguments and EnvironmentVariables from the
// plist's top dict, where keys and values alternate.
func parsePlist(raw []byte) (agentPlist, error) {
	var doc struct {
		Dict plistNode `xml:"dict"`
	}
	var p agentPlist
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return p, err
	}
	top := doc.Dict.Nodes
	for i := 0; i+1 < len(top); i += 2 {
		if top[i].XMLName.Local != "key" {
			continue
		}
		val := top[i+1].Nodes
		switch top[i].Text {
		case "ProgramArguments":
			for _, n := range val {
				p.args = append(p.args, n.Text)
			}
		case "EnvironmentVariables":
			p.env = map[string]string{}
			for j := 0; j+1 < len(val); j += 2 {
				p.env[val[j].Text] = val[j+1].Text
			}
		}
	}
	return p, nil
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// daemonSocket sends a hook with no session, which the daemon answers with {}
// and no side effect: any reply means it is alive.
func (d *doctor) daemonSocket() {
	sock, err := statedir.SocketPath()
	if err == nil {
		_, err = ask(sock, wire.Request{Type: wire.TypeHook, Event: "SessionEnd"})
	}
	if err != nil {
		d.bad("the daemon is not responding: check %s", logHint())
		return
	}
	d.ok("the daemon responds")
}

// duplicates says whether the daemon detects duplicate work, with which
// judge, or why not. A judge never configured is no failure; one configured
// that cannot run, or that failed every comparison, is.
func (d *doctor) duplicates() {
	sock, err := statedir.SocketPath()
	var resp wire.Response
	if err == nil {
		resp, err = ask(sock, wire.Request{Type: wire.TypeStatus})
	}
	var st wire.Status
	if err != nil || resp.Error != "" || json.Unmarshal(resp.Output, &st) != nil {
		d.skip("duplicate detection: the daemon did not say")
		return
	}
	j := st.Judge
	switch {
	case j.Name != "" && j.Errors > 0 && j.Errors == j.Comparisons:
		d.bad("duplicate detection: the judge %s failed every comparison (%d), the last: %s", j.Name, j.Errors, j.LastError)
	case j.Name != "":
		d.ok("duplicate detection is on, judge %s", j.Name)
	case j.Off == "":
		d.skip("duplicate detection: this daemon does not report it; update dugout")
	case j.Off == judge.ErrNotConfigured.Error():
		d.skip("duplicate detection is off: %s", j.Off)
	default:
		d.bad("duplicate detection is off: %s", j.Off)
	}
}

// logHint is the daemon's log for a message, with ~ for the home.
func logHint() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonLog("~")
	}
	if rel, err := filepath.Rel(home, daemonLog(home)); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return daemonLog(home)
}

// localPause is no failure, but a paused machine looks broken.
func (d *doctor) localPause() {
	dir, err := statedir.Dir()
	if err != nil {
		return
	}
	if _, err := os.Stat(statedir.PausedPath(dir)); err == nil {
		d.skip("paused on this machine: dugout resume")
	}
}

// serverAndProtocol checks checks 4 and 5: GET /v1/state with the person's
// token, then the protocol header of that same response. The token is the
// one the daemon uses: under launchd or systemd that is the keychain's, since
// the shell's $DUGOUT_TOKEN never reaches the LaunchAgent or the unit.
func (d *doctor) serverAndProtocol(a agent) {
	server := a.server
	if server == "" || server == "off" {
		d.skip("server: the daemon runs without a server")
		return
	}
	server = keychainAccount(server)
	if wire.InsecureServer(server) && !a.insecure {
		d.bad("%s has no TLS and is outside this machine: the daemon only talks to it when installed with -insecure, and the token is not sent to it from here (use https, or dugout install -insecure -server %s)", server, server)
		d.skip("protocol: not asked, the server was not contacted")
		return
	}
	token := envToken(server)
	if a.installed && token != "" {
		d.skip("DUGOUT_TOKEN from your shell does not reach the %s: using the keychain token", a.kind)
		token = ""
	}
	if token == "" {
		t, err := keychain.Get(server)
		switch {
		case err == nil:
			token = t
		case errors.Is(err, keychain.ErrNotFound):
		default:
			d.bad("could not read the token from the keychain for %s: %v", server, err)
			return
		}
	}
	noToken := func() { d.bad("no token for %s: run dugout login -server %s", server, server) }
	// A loopback serve may run without auth, so there it is worth asking.
	if token == "" && !wire.IsLoopbackURL(server) {
		noToken()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/v1/state", nil)
	if err != nil {
		d.bad("the server URL is not usable (%s): %v", server, err)
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.bad("the server %s is not responding: %v", server, err)
		d.skip("protocol: no response from the server")
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		d.ok("the server %s responds and accepts your token", server)
	case resp.StatusCode == http.StatusUnauthorized && token == "":
		noToken()
	case resp.StatusCode == http.StatusUnauthorized:
		d.bad("the server rejected your token: ask the admin for a new one and run dugout login")
	default:
		d.bad("the server %s answered HTTP %d: check the server log", server, resp.StatusCode)
	}
	d.protocol(resp.Header.Get(wire.HeaderProtocol))
}

func (d *doctor) protocol(theirs string) {
	if theirs == "" {
		d.skip("protocol: the server does not report its version")
		return
	}
	n, err := strconv.Atoi(theirs)
	switch {
	case err == nil && n == wire.ProtocolVersion:
		d.ok("the server speaks the same protocol (%d)", n)
	case err == nil && n < wire.ProtocolVersion:
		d.skip("the server speaks protocol %d and your dugout %d: update the server", n, wire.ProtocolVersion)
	default:
		d.bad("the server speaks protocol %s and your dugout %d: update dugout (docs/install.md, Update)", theirs, wire.ProtocolVersion)
	}
}

// git checks the version and runs the store's own capability probe on a
// throwaway bare repo, with the git the daemon runs: under launchd or
// systemd, the first on the plist's or the unit's PATH.
func (d *doctor) git(a agent) {
	if a.installed {
		// Both this check and the store look "git" up on this process's PATH.
		old := os.Getenv("PATH")
		os.Setenv("PATH", a.path)
		defer os.Setenv("PATH", old)
	}
	out, err := exec.Command("git", "version").Output()
	if err != nil && a.installed {
		d.bad("could not find git on the %s PATH: run dugout install again from a terminal with git on the PATH", a.kind)
		return
	}
	if err != nil {
		d.bad("could not find git on the PATH: install git (tested with 2.50)")
		return
	}
	ver := strings.TrimPrefix(strings.TrimSpace(string(out)), "git version ")
	tmp, err := os.MkdirTemp("", "dugout-doctor")
	if err == nil {
		defer os.RemoveAll(tmp)
		var st *snapshot.Store
		if st, err = snapshot.Open(tmp, "probe"); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
			defer cancel()
			err = st.Probe(ctx)
		}
	}
	if err != nil {
		d.bad("git %s does not support merge-tree --write-tree --merge-base: update git (tested with 2.50)", ver)
		return
	}
	d.ok("git %s supports conflict detection", ver)
}

func (d *doctor) plugin() {
	path, err := exec.LookPath("claude")
	if err != nil {
		d.skip("plugin: could not find claude on the PATH")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, "plugin", "list").Output()
	if !strings.Contains(string(out), "dugout") {
		d.bad("the plugin is not installed: run dugout setup, or in Claude Code /plugin marketplace add joacofg/dugout and /plugin install dugout@dugout")
		return
	}
	d.ok("the Claude Code plugin is installed")
}
