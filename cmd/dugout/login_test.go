package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/testgit"
	"github.com/joacofg/dugout/internal/wire"
)

// fakeSecurity puts a security(1) in a temp dir that keeps its items in a
// file and logs its arguments, so the real Keychain is never touched. It
// exits 44 for a missing item like the real one. Beside it goes a
// secret-tool with the same store and log, which Linux uses instead, so the
// real keyring is never touched either.
func fakeSecurity(t *testing.T) (dir, store, argsLog string) {
	t.Helper()
	dir, store, argsLog = fakeSecurityOnly(t)
	secretTool := `#!/bin/sh
echo "$*" >>` + argsLog + `
STORE=` + store + `
case "$1" in
store)
  [ "$2 $3 $4 $5" = "--label=Dugout $6 service dugout account" ] || exit 2
  tok=$(cat)
  touch "$STORE"
  grep -v "^$6	" "$STORE" > "$STORE.new"
  printf '%s\t%s\n' "$6" "$tok" >> "$STORE.new"
  mv "$STORE.new" "$STORE" ;;
lookup)
  [ "$2 $3 $4" = "service dugout account" ] || exit 2
  [ -f "$STORE" ] && awk -F'\t' -v a="$5" '$1==a{printf "%s", $2; f=1} END{exit f?0:1}' "$STORE" && exit 0
  exit 1 ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte(secretTool), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, store, argsLog
}

// storeCall is the logged call that stores token for server on this
// platform's keychain: Linux's secret-tool takes the token on stdin.
func storeCall(server, token string) string {
	if runtime.GOOS == "linux" {
		return "store --label=Dugout " + server + " service dugout account " + server
	}
	return "add-generic-password -U -s dugout -a " + server + " -w " + token
}

func fakeSecurityOnly(t *testing.T) (dir, store, argsLog string) {
	t.Helper()
	dir, argsLog = t.TempDir(), filepath.Join(t.TempDir(), "security.log")
	store = filepath.Join(t.TempDir(), "store")
	script := `#!/bin/sh
echo "$*" >>` + argsLog + `
STORE=` + store + `
case "$1" in
add-generic-password)
  [ "$2 $3 $4 $5 $7" = "-U -s dugout -a -w" ] || exit 2
  touch "$STORE"
  grep -v "^$6	" "$STORE" > "$STORE.new"
  printf '%s\t%s\n' "$6" "$8" >> "$STORE.new"
  mv "$STORE.new" "$STORE" ;;
find-generic-password)
  [ "$2 $3 $4 $6" = "-s dugout -a -w" ] || exit 2
  [ -f "$STORE" ] && awk -F'\t' -v a="$5" '$1==a{print $2; f=1} END{exit f?0:1}' "$STORE" && exit 0
  exit 44 ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, store, argsLog
}

// runIn runs the built dugout with stdin and the given environment.
func runIn(t *testing.T, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// secEnv is the only environment a test may give login or a daemon: the fake
// security first on PATH, a temp HOME, and no inherited DUGOUT_SERVER or
// DUGOUT_TOKEN, so nothing can reach the developer's real Keychain.
func secEnv(t *testing.T, secDir, home string) []string {
	t.Helper()
	env := []string{"HOME=" + home, statedir.EnvHome + "=" + home, "PATH=" + secDir + ":/bin:/usr/bin",
		envServer + "=", wire.EnvToken + "="}
	guardEnv(t, env, secDir)
	return env
}

// guardEnv fails the test unless env resolves security and secret-tool to
// the fakes in secDir, the first entry of PATH.
func guardEnv(t *testing.T, env []string, secDir string) {
	t.Helper()
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	if secDir == "" || !strings.HasPrefix(path, secDir+":") {
		t.Fatalf("test env would not use the fake security first: PATH=%q", path)
	}
	for _, tool := range []string{"security", "secret-tool"} {
		if _, err := os.Stat(filepath.Join(secDir, tool)); err != nil {
			t.Fatalf("no fake %s in %s: %v", tool, secDir, err)
		}
	}
}

const loginToken = "dgt_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ01234-_"

func TestLoginStoresToken(t *testing.T) {
	secDir, store, argsLog := fakeSecurity(t)
	const server = "http://127.0.0.1:7457"
	out, errOut, code := runIn(t, secEnv(t, secDir, shortDir(t)), loginToken+"\n", "login", "-server", server)
	if code != 0 {
		t.Fatalf("login: exit %d: %s", code, errOut)
	}
	if want := "Token saved in the keychain for " + server + ".\nIf the daemon is already running, restart it with dugout install -server " + server + ".\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if strings.Contains(out, loginToken) || strings.Contains(errOut, loginToken) {
		t.Fatalf("login echoed the token:\n%s\n%s", out, errOut)
	}
	if log := readLog(t, argsLog); len(log) != 1 || log[0] != storeCall(server, loginToken) {
		t.Fatalf("keychain calls = %q", log)
	}
	if b, _ := os.ReadFile(store); !strings.Contains(string(b), server+"\t"+loginToken) {
		t.Fatalf("store = %q", b)
	}
}

// TestLoginNormalizesServer: https://h/ and https://h are one Keychain item.
func TestLoginNormalizesServer(t *testing.T) {
	secDir, _, argsLog := fakeSecurity(t)
	if _, errOut, code := runIn(t, secEnv(t, secDir, shortDir(t)), loginToken+"\n", "login", "-server", " https://office.example/ "); code != 0 {
		t.Fatalf("login: exit %d: %s", code, errOut)
	}
	if log := readLog(t, argsLog); log[0] != storeCall("https://office.example", loginToken) {
		t.Fatalf("keychain calls = %q", log)
	}
}

// TestLoginTokenFile: on Linux without secret-tool the token goes to a 0600
// file in the state dir, login says where, and the daemon reads it back.
// In-process, with a PATH that has no secret-tool, so the real one is never
// reached. internal/keychain covers the file on a Mac.
func TestLoginTokenFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the token file is Linux's")
	}
	home := shortDir(t)
	t.Setenv(statedir.EnvHome, home)
	t.Setenv(wire.EnvToken, "")
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"login", "-server", "https://h/"}, strings.NewReader(loginToken+"\n"), &out, &errb); code != 0 {
		t.Fatalf("login: exit %d: %s", code, errb.String())
	}
	file := filepath.Join(home, "tokens.json")
	if want := "Token saved in " + file + " for https://h.\n"; !strings.HasPrefix(out.String(), want) {
		t.Fatalf("stdout = %q, want it to start with %q", out.String(), want)
	}
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v", st, err)
	}
	if tok, err := resolveToken("https://h"); err != nil || tok != loginToken {
		t.Fatalf("resolveToken after login = %q, %v", tok, err)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	secDir, store, _ := fakeSecurity(t)
	for _, bad := range []string{"", "hello\n", "dgt_short\n", loginToken + "x\n", "xgt_" + loginToken[4:] + "\n"} {
		out, errOut, code := runIn(t, secEnv(t, secDir, shortDir(t)), bad, "login", "-server", "http://127.0.0.1:7457")
		if code != 1 || !strings.Contains(errOut, "that token does not look like a Dugout token (it starts with dgt_)") {
			t.Fatalf("token %q: exit %d, stderr %q", bad, code, errOut)
		}
		if tok := strings.TrimSpace(bad); tok != "" && strings.Contains(out+errOut, tok) {
			t.Fatalf("login echoed the rejected token %q", bad)
		}
	}
	if _, err := os.Stat(store); err == nil {
		t.Fatal("a rejected token reached the keychain")
	}
}

func TestLoginNeedsServer(t *testing.T) {
	secDir, _, _ := fakeSecurity(t)
	if _, errOut, code := runIn(t, secEnv(t, secDir, shortDir(t)), loginToken+"\n", "login"); code != 2 || !strings.Contains(errOut, "missing -server") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

// overlapRepo is a clone on its own branch, enough for a session to start in.
func overlapRepo(t *testing.T) string {
	t.Helper()
	f := testgit.NewFixture(t)
	testgit.Git(t, f.Clone, "switch", "-q", "-c", "feat-a")
	return f.Clone
}

// startStateServe runs serve -state on a free port and returns its address,
// the admin token and a token issued for ana.
func startStateServe(t *testing.T) (addr, adminTok, anaTok string) {
	t.Helper()
	path := filepath.Join(shortDir(t), "serve.json")
	out, errOut, code := runAdmin(t, "admin", "init", "-state", path)
	if code != 0 {
		t.Fatalf("admin init: exit %d: %s", code, errOut)
	}
	adminTok = strings.TrimSpace(out[strings.LastIndex(out, " ")+1:])
	addr = freeAddr(t)
	start(t, nil, "serve", "-addr", addr, "-state", path)
	post := func(route, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+route, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminTok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	var issued struct{ Token string }
	eventually(t, 5*time.Second, "serve issues a token", func() bool {
		code, body := post("/v1/admin/tokens", `{"name":"ana"}`)
		return code == http.StatusCreated && json.Unmarshal([]byte(body), &issued) == nil
	})
	return addr, adminTok, issued.Token
}

// TestTokenFromKeychain: a daemon with no DUGOUT_TOKEN takes the token from
// the Keychain for its -server, and serve's team pause reaches it.
func TestTokenFromKeychain(t *testing.T) {
	addr, adminTok, anaTok := startStateServe(t)
	secDir, _, _ := fakeSecurity(t)
	home := shortDir(t)
	server := "http://" + addr
	if _, errOut, code := runIn(t, secEnv(t, secDir, home), anaTok+"\n", "login", "-server", server); code != 0 {
		t.Fatalf("login: exit %d: %s", code, errOut)
	}
	// The slash is the same Keychain item as the login's.
	p := start(t, secEnv(t, secDir, home), "daemon", "-server", server+"/", "-poll", "300ms", "-resend", "2s")
	waitDaemonSocket(t, home)
	f := overlapRepo(t)
	runHook(t, home, "SessionStart", map[string]any{"session_id": "sA", "cwd": f})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server+"/v1/admin/pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause: %v %v", resp, err)
	}
	eventually(t, 5*time.Second, "the daemon takes the team pause with the keychain token", func() bool {
		return runHook(t, home, "UserPromptSubmit", map[string]any{"session_id": "sA", "cwd": f, "prompt": "x"}) ==
			`{"systemMessage":"Dugout is paused for the whole team."}`
	})
	if strings.Contains(p.log.String(), anaTok) {
		t.Fatalf("daemon logged the token:\n%s", p.log)
	}
}

// TestTokenMissingDaemonRuns: with neither $DUGOUT_TOKEN nor a Keychain item
// and a server off this machine, the daemon still answers hooks, says why
// there is no uplink, once, and does not start it: its listening line has no
// server, and the TLS warning that goes with a server never appears.
func TestTokenMissingDaemonRuns(t *testing.T) {
	secDir, _, _ := fakeSecurity(t)
	home := shortDir(t)
	const server = "http://192.0.2.1:7457"
	p := start(t, secEnv(t, secDir, home), "daemon", "-server", server, "-insecure", "-poll", "300ms", "-resend", "1s")
	waitDaemonSocket(t, home)
	f := overlapRepo(t)
	// The hook gives up after 150 ms, which a cold daemon on a slow runner can
	// exceed on its first answer: ask again until the daemon answers.
	var out string
	eventually(t, 5*time.Second, "the daemon answers the SessionStart hook", func() bool {
		out = runHook(t, home, "SessionStart", map[string]any{"session_id": "sA", "cwd": f})
		return strings.Contains(out, "office_declare_intent")
	})
	log := p.log.String()
	if n := strings.Count(log, "no token for "+server+": run dugout login"); n != 1 {
		t.Fatalf("warned %d times, want 1:\n%s", n, log)
	}
	if !strings.Contains(log, `msg="daemon listening"`) || !strings.Contains(log, `server=""`) {
		t.Fatalf("the daemon started with a server:\n%s", log)
	}
	if strings.Contains(log, "server without TLS") {
		t.Fatalf("the uplink's server was set:\n%s", log)
	}
}

// TestKeychainErrorDaemonExits: a Keychain that fails other than "not found"
// (locked, or security timing out) may work on the next try, so the daemon
// logs it at Error and exits 1 for launchd to restart it, instead of running
// without uplink until someone restarts it.
func TestKeychainErrorDaemonExits(t *testing.T) {
	secDir, home := t.TempDir(), shortDir(t)
	for _, tool := range []string{"security", "secret-tool"} {
		if err := os.WriteFile(filepath.Join(secDir, tool), []byte("#!/bin/sh\necho 'User interaction is not allowed.' >&2\nexit 36\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := start(t, secEnv(t, secDir, home), "daemon", "-server", "http://192.0.2.1:7457", "-insecure", "-poll", "300ms")
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		<-done
		t.Fatalf("the daemon kept running with a failing Keychain:\n%s", p.log)
	}
	log := p.log.String()
	if code := p.cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, log)
	}
	if !strings.Contains(log, `level=ERROR msg="could not read the token from the keychain"`) || !strings.Contains(log, "User interaction is not allowed.") {
		t.Fatalf("log does not say why:\n%s", log)
	}
	if strings.Contains(log, "no token") {
		t.Fatalf("a failing Keychain is not a missing token:\n%s", log)
	}
}

// TestResolveTokenWithoutSecurity: with no security(1) at all there is no
// Keychain to retry, so that is no token rather than a failure; on Linux,
// with no secret-tool either, a missing token file is no token too.
// In-process, with a PATH that has neither, so the real ones are never
// reached, and a state dir of its own.
func TestResolveTokenWithoutSecurity(t *testing.T) {
	t.Setenv(wire.EnvToken, "")
	t.Setenv(statedir.EnvHome, t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if tok, err := resolveToken("http://192.0.2.1:7457"); tok != "" || err != nil {
		t.Fatalf("resolveToken = %q, %v; want no token and no error", tok, err)
	}
}

// TestTokenMissingLoopbackKeepsUplink: a dev daemon on loopback with no token
// anywhere keeps its server, and says nothing about a token.
func TestTokenMissingLoopbackKeepsUplink(t *testing.T) {
	secDir, _, _ := fakeSecurity(t)
	home := shortDir(t)
	addr := freeAddr(t)
	start(t, nil, "serve", "-addr", addr) // no state, no token: loopback takes anyone
	p := start(t, secEnv(t, secDir, home), "daemon", "-server", "http://"+addr, "-poll", "300ms", "-resend", "1s")
	waitDaemonSocket(t, home)
	log := p.log.String()
	if !strings.Contains(log, "server=http://"+addr) {
		t.Fatalf("the loopback daemon has no server:\n%s", log)
	}
	if strings.Contains(log, "no token") {
		t.Fatalf("a loopback dev daemon warned:\n%s", log)
	}
}

func TestPauseResumeCommands(t *testing.T) {
	home := shortDir(t)
	env := append(os.Environ(), statedir.EnvHome+"="+home)
	file := statedir.PausedPath(home)
	if out, errOut, code := runIn(t, env, "", "pause"); code != 0 || out != "Dugout paused on this machine.\n" {
		t.Fatalf("pause: exit %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("pause created no file:", err)
	}
	if out, _, code := runIn(t, env, "", "resume"); code != 0 || out != "Dugout resumed on this machine.\n" {
		t.Fatalf("resume: exit %d %q", code, out)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("resume left the file: %v", err)
	}
	if _, _, code := runIn(t, env, "", "resume"); code != 0 {
		t.Fatalf("resume twice: exit %d", code)
	}
}

// TestLocalPause: dugout pause silences a running daemon's hooks, one notice
// per session, and dugout resume brings them back.
func TestLocalPause(t *testing.T) {
	home := shortDir(t)
	startLocalDaemon(t, home)
	f := overlapRepo(t)
	runHook(t, home, "SessionStart", map[string]any{"session_id": "sA", "cwd": f})
	env := append(os.Environ(), statedir.EnvHome+"="+home)
	prompt := func() string {
		return runHook(t, home, "UserPromptSubmit", map[string]any{"session_id": "sA", "cwd": f, "prompt": "x"})
	}
	if _, _, code := runIn(t, env, "", "pause"); code != 0 {
		t.Fatal("pause failed")
	}
	if out := prompt(); out != `{"systemMessage":"Dugout is paused on this machine."}` {
		t.Fatalf("first hook of the pause = %s", out)
	}
	if out := prompt(); out != "{}" {
		t.Fatalf("second hook of the pause = %s", out)
	}
	if _, _, code := runIn(t, env, "", "resume"); code != 0 {
		t.Fatal("resume failed")
	}
	if out := prompt(); strings.Contains(out, "systemMessage") {
		t.Fatalf("hook after resume = %s", out)
	}
}
