// Command dugout is Dugout's single binary: the office server, the
// per-machine daemon, the Claude Code hook, the plugin's MCP server and the
// track, init, mode and status commands.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joacofg/dugout/internal/daemon"
	"github.com/joacofg/dugout/internal/hook"
	"github.com/joacofg/dugout/internal/judge"
	"github.com/joacofg/dugout/internal/keychain"
	"github.com/joacofg/dugout/internal/mcp"
	"github.com/joacofg/dugout/internal/peers"
	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

const (
	defaultAddr  = "127.0.0.1:7457"
	envServer    = "DUGOUT_SERVER"
	trackTimeout = 30 * time.Second
	usageText    = `usage:
  dugout serve  [-addr 127.0.0.1:7457] [-allow-host name,...] [-state serve.json] [-log-format text|json]   (without -state, token in $DUGOUT_TOKEN)
  dugout admin  init -state serve.json
  dugout admin  token issue|revoke <name> | token list      ($DUGOUT_SERVER, $DUGOUT_ADMIN_TOKEN)
  dugout admin  invite [-expires 24h] <name>                 (prints a one-time line that joins name to the team)
  dugout admin  repo add <repo> [person...] | repo remove <repo> | repo list
  dugout admin  repo grant|ungrant <repo> <person>           (a repo with members is seen only by them)
  dugout admin  pause | resume | metrics
  dugout daemon [-server URL|off] [-insecure] [-poll 10s] [-resend 60s] [-metrics-grace 2m] [-log-format text|json]
  dugout hook   <SessionStart|UserPromptSubmit|PreToolUse|PostToolUse|Stop|SessionEnd>
  dugout mcp
  dugout track  <path>
  dugout init   [-mode auto|scoped|supervised] [path]        (follows the repo and asks, once, for the autonomy mode)
  dugout mode   [auto|scoped|supervised]                     (the mode of this directory's repo, or changes it)
  dugout status                                              (what Dugout sees on this machine)
  dugout office [-server URL] [-no-open]                     (opens the live office view, read-only)
  dugout login  -server URL [-insecure]                      (the token is read without echo and goes to the keychain)
  dugout pause | resume                                      (pauses this machine only)
  dugout setup  [-server URL|off] [-insecure] [-mode auto|scoped|supervised] [-marketplace joacofg/dugout] [--remove]
                                                             (daemon, Claude Code plugin, init here, doctor; --remove undoes it)
  dugout join   [-insecure] [-mode ...] '<server>#<code>'      (redeems an invite for your token, then runs setup)
  dugout install -server URL [-insecure] [-dry-run]
  dugout uninstall
  dugout doctor [--bundle [-o file]]                       (with --bundle, a .tar.gz to send to the admin)
  dugout version
`
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "admin":
		return cmdAdmin(args[1:], stdout, stderr)
	case "daemon":
		return cmdDaemon(args[1:], stderr)
	case "hook":
		return cmdHook(args[1:], stdin, stdout, stderr)
	case "mcp":
		return cmdMCP(stdin, stdout, stderr)
	case "track":
		return cmdTrack(args[1:], stdout, stderr)
	case "init":
		return cmdInit(args[1:], stdin, stdout, stderr)
	case "mode":
		return cmdMode(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "setup":
		return cmdSetup(args[1:], stdin, stdout, stderr)
	case "join":
		return cmdJoin(args[1:], stdin, stdout, stderr)
	case "install":
		return cmdInstall(args[1:], stdout, stderr)
	case "uninstall":
		return cmdUninstall(stdout, stderr)
	case "login":
		return cmdLogin(args[1:], stdin, stdout, stderr)
	case "pause":
		return cmdPause(stdout, stderr)
	case "resume":
		return cmdResume(stdout, stderr)
	case "office":
		return cmdOffice(args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "version":
		return cmdVersion(stdout)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n%s", args[0], usageText)
		return 2
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultAddr, "HTTP address")
	allowHosts := fs.String("allow-host", "", "Host names accepted besides loopback and -addr, comma-separated")
	statePath := fs.String("state", "", "state file (per-person tokens, allowed repos and pause); without it, in memory")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		fmt.Fprintln(stderr, "dugout serve:", err)
		return 2
	}
	// With -state each person has a token, so off loopback that is the auth
	// and the shared token is not taken.
	token := os.Getenv(wire.EnvToken)
	if *statePath == "" && !isLoopback(host) && token == "" {
		fmt.Fprintf(stderr, "dugout serve: listening on %s, outside loopback, needs a shared token in $%s or -state\n", *addr, wire.EnvToken)
		return 1
	}
	handler, err := logHandler("serve", *logFormat, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	log := slog.New(handler)
	slog.SetDefault(log)
	office := serve.New()
	if *statePath != "" {
		stored, err := serve.LoadState(*statePath)
		if err != nil {
			fmt.Fprintln(stderr, "dugout serve: could not load the state file:", err)
			return 1
		}
		log.Info("state loaded", "path", *statePath, "users", len(stored.Users), "repos", len(stored.Repos), "daemons", len(stored.Owners))
		if token != "" {
			log.Warn("shared token ignored: with -state each person uses their own", "env", wire.EnvToken)
		}
		office.UseState(*statePath, stored)
	} else {
		office.SetToken(token)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(stderr, "dugout serve:", err)
		return 1
	}
	// Loopback Host headers always pass; a specific -addr host passes too.
	// With an unspecified address (0.0.0.0) only loopback and -allow-host do.
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		office.AllowHost(host)
	}
	for _, h := range strings.Split(*allowHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			office.AllowHost(h)
		}
	}
	srv := &http.Server{Handler: office.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signalContext()
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("serve listening", "addr", ln.Addr().String())
	select {
	case <-ctx.Done():
		srv.Close() // streams never finish on their own, so no graceful Shutdown
		return 0
	case err := <-errc:
		fmt.Fprintln(stderr, "dugout serve:", err)
		return 1
	}
}

func cmdAdmin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if args[0] != "init" {
		return runAdminCmd(args, stdout, stderr)
	}
	flags := flag.NewFlagSet("admin init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statePath := flags.String("state", "", "state file to create")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *statePath == "" {
		fmt.Fprintln(stderr, "dugout admin init: missing -state")
		return 2
	}
	if _, err := os.Stat(*statePath); err == nil {
		fmt.Fprintf(stderr, "the state file already exists: %s\n", *statePath)
		return 1
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stderr, "dugout admin init:", err)
		return 1
	}
	token, hash, err := serve.NewToken()
	if err != nil {
		fmt.Fprintln(stderr, "dugout admin init:", err)
		return 1
	}
	if err := serve.SaveState(*statePath, serve.Stored{Version: 1, AdminHash: hash}); err != nil {
		fmt.Fprintln(stderr, "dugout admin init:", err)
		return 1
	}
	fmt.Fprintf(stdout, "admin token (shown only once): %s\n", token)
	return 0
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// logHandler picks the slog handler for -log-format.
func logHandler(cmd, format string, w io.Writer) (slog.Handler, error) {
	switch format {
	case "text":
		return slog.NewTextHandler(w, nil), nil
	case "json":
		return slog.NewJSONHandler(w, nil), nil
	}
	return nil, fmt.Errorf("dugout %s: unknown log format: %s (text or json)", cmd, format)
}

func cmdDaemon(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := os.Getenv(envServer)
	if server == "" {
		server = "http://" + defaultAddr
	}
	serverURL := fs.String("server", server, "URL of dugout serve, or off to run on this machine only (or $"+envServer+")")
	poll := fs.Duration("poll", daemon.DefaultPoll, "worktree scan interval")
	resend := fs.Duration("resend", daemon.DefaultResend, "full-state resend interval")
	grace := fs.Duration("metrics-grace", daemon.DefaultMetricsGrace, "how long an ended conflict may come back as the same one before its metrics are final")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	insecure := fs.Bool("insecure", false, "allow an http:// server outside this machine: the token travels in the clear")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *serverURL == "off" {
		*serverURL = "" // serverless: no uplink, stage 2 only
	}
	handler, err := logHandler("daemon", *logFormat, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	log := slog.New(handler)
	slog.SetDefault(log)
	dir, err := statedir.Dir()
	var sock string
	if err == nil {
		sock, err = statedir.SocketPath()
	}
	if err != nil {
		fmt.Fprintln(stderr, "dugout daemon:", err)
		return 1
	}
	if wire.InsecureServer(*serverURL) && !*insecure {
		// Local detection and hooks go on, as with a missing token; only the
		// uplink is refused, and it logs why on every start.
		log.Error("not connecting to "+*serverURL, "err", wire.ErrInsecureServer, "hint", "run dugout install -insecure -server "+*serverURL+", or use https")
		*serverURL = ""
	}
	token, err := resolveToken(*serverURL)
	if err != nil {
		// A locked or stuck keychain may answer on the next try: exit for
		// launchd or systemd to restart the daemon rather than run without
		// uplink.
		log.Error("could not read the token from the keychain", "err", err)
		return 1
	}
	if *serverURL != "" && os.Getenv(wire.EnvToken) != "" && envToken(*serverURL) == "" {
		log.Warn("$"+wire.EnvToken+" is not sent to "+*serverURL+": it belongs to $"+envServer+" and to a server on this machine", "hint", "run dugout login -server "+*serverURL)
	}
	if token == "" && *serverURL != "" && !wire.IsLoopbackURL(*serverURL) {
		// Local detection and hooks go on; only the uplink needs the token.
		log.Warn("no token for " + *serverURL + ": run dugout login")
		*serverURL = ""
	}
	// The judge is opt-in and read from the environment only, key included.
	j, err := judge.FromEnv(os.Getenv)
	judgeOff := ""
	if err != nil {
		judgeOff = err.Error()
		if !errors.Is(err, judge.ErrNotConfigured) {
			log.Warn("duplicate detection off", "err", err)
		}
	}
	d, err := daemon.New(daemon.Config{StateDir: dir, Socket: sock, Server: *serverURL, Insecure: *insecure, Token: token, Poll: *poll, Resend: *resend, MetricsGrace: *grace, Log: log,
		Judge: j, JudgeOff: judgeOff})
	if err != nil {
		fmt.Fprintln(stderr, "dugout daemon:", err)
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "dugout daemon:", err)
		return 1
	}
	return 0
}

// envToken is $DUGOUT_TOKEN when it may go to server, else "". The token is
// bound to the server of the person's configuration, $DUGOUT_SERVER: it is
// never sent to another -server, which could be anyone's. A server on this
// machine takes it too, for the default one and for local development.
func envToken(server string) string {
	t := os.Getenv(wire.EnvToken)
	if t == "" || wire.IsLoopbackURL(server) {
		return t
	}
	if configured := os.Getenv(envServer); configured != "" && keychainAccount(configured) == keychainAccount(server) {
		return t
	}
	return ""
}

// resolveToken is $DUGOUT_TOKEN when envToken lets it go to server, else the
// keychain's token for server, which login stores under that server's URL. A
// missing item is no token and no error, and so is a missing security(1) on
// a Mac. Any other keychain error (locked, security or secret-tool failing
// or timing out, an unreadable token file) is returned; it never holds the
// token.
func resolveToken(server string) (string, error) {
	if server == "" {
		return os.Getenv(wire.EnvToken), nil
	}
	if t := envToken(server); t != "" {
		return t, nil
	}
	t, err := keychain.Get(keychainAccount(server))
	if err != nil && !errors.Is(err, keychain.ErrNotFound) && !errors.Is(err, exec.ErrNotFound) {
		return "", err
	}
	return t, nil
}

// cmdHook always exits 0: a hook must never break the agent's turn.
func cmdHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	event := ""
	if len(args) > 0 {
		event = args[0]
	}
	sock, _ := statedir.SocketPath() // "" fails the dial, and the hook degrades
	home, _ := os.UserHomeDir()      // "" only skips the kickstart
	return hook.Run(event, stdin, stdout, stderr, hook.Env{Socket: sock, TempDir: os.TempDir(), ClaudePID: hook.DefaultClaudePID, Home: home,
		ClaudeDir: peers.Dir(os.Getenv, home)})
}

// cmdMCP is the plugin's MCP server on stdin and stdout, until Claude Code
// closes stdin.
func cmdMCP(stdin io.Reader, stdout, stderr io.Writer) int {
	sock, err := statedir.SocketPath()
	if err != nil {
		fmt.Fprintln(stderr, "dugout mcp:", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "dugout mcp:", err)
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	if err := mcp.Serve(ctx, stdin, stdout, sockDaemon(sock), cwd, version); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "dugout mcp:", err)
		return 1
	}
	return 0
}

// sockDaemon is the daemon on its socket, for `dugout mcp`: like ask, but the
// whole call, dial included, within mcp.CallTimeout.
type sockDaemon string

func (s sockDaemon) Call(ctx context.Context, req wire.Request) (wire.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, mcp.CallTimeout)
	defer cancel()
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", string(s))
	if err != nil {
		return wire.Response{}, err
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	return roundTrip(c, req)
}

func cmdTrack(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	path, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "dugout track:", err)
		return 1
	}
	sock, err := statedir.SocketPath()
	if err != nil {
		fmt.Fprintln(stderr, "dugout track:", err)
		return 1
	}
	resp, err := ask(sock, wire.Request{Type: wire.TypeTrack, Path: path})
	if err != nil {
		fmt.Fprintln(stderr, "dugout track: the daemon is not responding:", err)
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintln(stderr, "dugout track:", resp.Error)
		return 1
	}
	var out struct {
		Root string `json:"root"`
	}
	_ = json.Unmarshal(resp.Output, &out)
	fmt.Fprintln(stdout, "tracked", out.Root)
	return 0
}

func ask(sock string, req wire.Request) (wire.Response, error) {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return wire.Response{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(trackTimeout))
	return roundTrip(c, req)
}

// roundTrip sends req on c and reads the daemon's one-line answer.
func roundTrip(c net.Conn, req wire.Request) (wire.Response, error) {
	var resp wire.Response
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return resp, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return resp, err
	}
	return resp, json.Unmarshal(line, &resp)
}
