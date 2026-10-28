package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

// doctorWorld is the sandbox doctor runs in: a temp HOME and PATH with fake
// security, secret-tool, launchctl, systemctl and claude first, so nothing
// real is ever consulted.
type doctorWorld struct {
	home, secDir, store string
	path                string // the PATH in env
	env                 []string
}

func newDoctorWorld(t *testing.T, claudeOut string) *doctorWorld {
	t.Helper()
	secDir, store, _ := fakeSecurity(t)
	lcDir, _ := fakeLaunchctl(t, false)
	writeFakeTool(t, lcDir, "systemctl", filepath.Join(t.TempDir(), "systemctl.log"), "")
	clDir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2\" = \"plugin list\" ] && echo '" + claudeOut + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(clDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := shortDir(t)
	path := secDir + ":" + lcDir + ":" + clDir + ":/bin:/usr/bin"
	env := []string{"HOME=" + home, statedir.EnvHome + "=" + home, "PATH=" + path, envServer + "=", wire.EnvToken + "="}
	guardEnv(t, env, secDir)
	return &doctorWorld{home: home, secDir: secDir, store: store, path: path, env: env}
}

// with returns the world's environment plus (and overriding) kv pairs.
func (w *doctorWorld) with(kv ...string) []string {
	return append(append([]string{}, w.env...), kv...)
}

// writePlist installs a LaunchAgent (on Linux, a unit) for exe and server
// whose PATH is the world's.
func (w *doctorWorld) writePlist(t *testing.T, exe, server string) {
	t.Helper()
	w.writePlistPath(t, exe, server, w.path)
}

func (w *doctorWorld) writePlistPath(t *testing.T, exe, server, pathEnv string) {
	t.Helper()
	writeService(t, w.home, exe, server, pathEnv)
}

// login stores tok in the world's fake Keychain for server.
func (w *doctorWorld) login(t *testing.T, server, tok string) {
	t.Helper()
	if _, errOut, code := runIn(t, w.env, tok+"\n", "login", "-server", server); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("missing line %q in:\n%s", l, out)
		}
	}
}

func TestDoctorAllGood(t *testing.T) {
	skipUnlessService(t)
	addr, _, anaTok := startStateServe(t)
	server := "http://" + addr
	w := newDoctorWorld(t, "dugout@dugout")
	w.writePlist(t, bin, server)
	w.login(t, server, anaTok)
	// A judge that is configured but never called: nothing to compare. The
	// base URL keeps any call off the real API.
	start(t, append(w.env, "DUGOUT_JUDGE=anthropic", "ANTHROPIC_API_KEY=sk-ant-doctor-test", "ANTHROPIC_BASE_URL=http://127.0.0.1:1"),
		"daemon", "-server", "off", "-poll", "300ms")
	waitDaemonSocket(t, w.home)
	out, errOut, code := runIn(t, w.env, "", "doctor")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	wantLines(t, out,
		"✓ dugout "+version,
		serviceInstalled(),
		"✓ the daemon responds",
		"✓ duplicate detection is on, judge anthropic claude-haiku-5-5",
		"✓ the server "+server+" responds and accepts your token",
		"✓ the server speaks the same protocol ("+strconv.Itoa(wire.ProtocolVersion)+")",
		"✓ the Claude Code plugin is installed")
	if strings.Contains(out, "✗") || strings.Contains(out, "–") || !strings.Contains(out, "✓ git ") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// TestDoctorLaunchAgentIgnoresShellToken: the LaunchAgent (or the unit) never
// sees the shell's DUGOUT_TOKEN, so with one installed doctor checks the
// keychain item for its server and says the shell's token does not count.
func TestDoctorLaunchAgentIgnoresShellToken(t *testing.T) {
	skipUnlessService(t)
	addr, _, anaTok := startStateServe(t)
	server := "http://" + addr
	ignored := "– DUGOUT_TOKEN from your shell does not reach the " + serviceKind() + ": using the keychain token"

	w := newDoctorWorld(t, "dugout@dugout")
	w.writePlist(t, bin, server)
	out, _, code := runIn(t, w.with(envServer+"=http://192.0.2.1:7457", wire.EnvToken+"="+anaTok), "", "doctor")
	if code != 1 {
		t.Fatalf("a good shell token with an empty Keychain: exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, ignored, "✗ no token for "+server+": run dugout login -server "+server)

	w = newDoctorWorld(t, "dugout@dugout")
	w.writePlist(t, bin, server)
	w.login(t, server, anaTok)
	out, _, _ = runIn(t, w.with(wire.EnvToken+"="+loginToken), "", "doctor")
	wantLines(t, out, ignored, "✓ the server "+server+" responds and accepts your token")
}

// TestDoctorGitFromLaunchAgentPath: with a plist (or a unit), doctor probes
// the git the daemon runs, the first on its PATH, not the shell's.
func TestDoctorGitFromLaunchAgentPath(t *testing.T) {
	skipUnlessService(t)
	old := t.TempDir()
	if err := os.WriteFile(filepath.Join(old, "git"), []byte("#!/bin/sh\n[ \"$1\" = version ] && echo 'git version 1.0-old' && exit 0\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := newDoctorWorld(t, "dugout@dugout")
	w.writePlistPath(t, bin, "off", old+":/bin")
	out, _, code := runIn(t, w.env, "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, "✗ git 1.0-old does not support merge-tree --write-tree --merge-base: update git (tested with 2.50)")

	w.writePlistPath(t, bin, "off", t.TempDir())
	out, _, _ = runIn(t, w.env, "", "doctor")
	wantLines(t, out, "✗ could not find git on the "+serviceKind()+" PATH: run dugout install again from a terminal with git on the PATH")
}

// TestDoctorGitNotOnPath: without a LaunchAgent, doctor looks for git on the
// shell's PATH.
func TestDoctorGitNotOnPath(t *testing.T) {
	w := newDoctorWorld(t, "dugout@dugout")
	out, _, code := runIn(t, w.with("PATH="+w.secDir+":"+t.TempDir()), "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, "✗ could not find git on the PATH: install git (tested with 2.50)")
}

// TestDoctorLocalPause: the local pause is no failure, but doctor says it and
// how to lift it.
func TestDoctorLocalPause(t *testing.T) {
	w := newDoctorWorld(t, "dugout@dugout")
	const paused = "– paused on this machine: dugout resume"
	if out, _, _ := runIn(t, w.env, "", "doctor"); strings.Contains(out, paused) {
		t.Fatalf("doctor reported a pause before dugout pause:\n%s", out)
	}
	if _, errOut, code := runIn(t, w.env, "", "pause"); code != 0 {
		t.Fatalf("pause: %d %s", code, errOut)
	}
	out, _, _ := runIn(t, w.env, "", "doctor")
	wantLines(t, out, paused)
}

func TestDoctorStaleBinary(t *testing.T) {
	skipUnlessService(t)
	w := newDoctorWorld(t, "dugout@dugout")
	w.writePlist(t, filepath.Join(w.home, "moved", "dugout"), "http://10.0.0.1:7457")
	out, _, code := runIn(t, w.env, "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, "✗ the "+serviceKind()+" points to a binary that no longer exists: run dugout install again")
}

func TestDoctorNotInstalled(t *testing.T) {
	skipUnlessService(t)
	w := newDoctorWorld(t, "")
	out, _, code := runIn(t, w.env, "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out,
		"✗ the daemon is not installed: run dugout install -server <url>",
		"✗ the plugin is not installed: run dugout setup, or in Claude Code /plugin marketplace add joacofg/dugout and /plugin install dugout@dugout",
		"– server: the daemon runs without a server")
}

func TestDoctorNoToken(t *testing.T) {
	addr, _, _ := startStateServe(t)
	server := "http://" + addr
	w := newDoctorWorld(t, "dugout@dugout")
	out, _, code := runIn(t, w.with(envServer+"="+server), "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, "✗ no token for "+server+": run dugout login -server "+server)
}

func TestDoctorServerDown(t *testing.T) {
	server := "http://" + freeAddr(t)
	w := newDoctorWorld(t, "dugout@dugout")
	out, _, code := runIn(t, w.with(envServer+"="+server, wire.EnvToken+"="+loginToken), "", "doctor")
	if code != 1 || !strings.Contains(out, "✗ the server "+server+" is not responding: ") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestDoctorBadToken(t *testing.T) {
	addr, _, _ := startStateServe(t)
	w := newDoctorWorld(t, "dugout@dugout")
	out, _, code := runIn(t, w.with(envServer+"=http://"+addr, wire.EnvToken+"="+loginToken), "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, "✗ the server rejected your token: ask the admin for a new one and run dugout login")
}

// TestDoctorKeychainToken: with no DUGOUT_TOKEN, doctor uses the Keychain item.
func TestDoctorKeychainToken(t *testing.T) {
	addr, _, anaTok := startStateServe(t)
	server := "http://" + addr
	w := newDoctorWorld(t, "dugout@dugout")
	if _, errOut, code := runIn(t, w.env, anaTok+"\n", "login", "-server", server); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	out, _, _ := runIn(t, w.with(envServer+"="+server), "", "doctor")
	wantLines(t, out, "✓ the server "+server+" responds and accepts your token")
}

func TestDoctorNeverPrintsToken(t *testing.T) {
	addr, _, anaTok := startStateServe(t)
	server := "http://" + addr
	w := newDoctorWorld(t, "dugout@dugout")
	if _, errOut, code := runIn(t, w.env, anaTok+"\n", "login", "-server", server); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	for name, env := range map[string][]string{
		"good":     w.with(envServer + "=" + server),
		"bad":      w.with(envServer+"="+server, wire.EnvToken+"="+loginToken),
		"down":     w.with(envServer+"=http://"+freeAddr(t), wire.EnvToken+"="+anaTok),
		"loopback": w.with(envServer+"="+server, wire.EnvToken+"="+anaTok),
	} {
		out, errOut, _ := runIn(t, env, "", "doctor")
		for _, tok := range []string{anaTok, loginToken} {
			if strings.Contains(out+errOut, tok) {
				t.Errorf("%s: doctor printed a token:\n%s%s", name, out, errOut)
			}
		}
	}
}

func TestDoctorProgramArguments(t *testing.T) {
	raw := launchd.Plist("/a b/dugout&co", "http://h:1/?x=1&y=2", "/h", "/o&p/bin:/usr/bin", false)
	p, err := parsePlist(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.args, "|"); got != "/a b/dugout&co|daemon|-server|http://h:1/?x=1&y=2|-log-format|json" {
		t.Fatalf("args = %q", got)
	}
	if argAfter(p.args, "-server") != "http://h:1/?x=1&y=2" {
		t.Fatal("argAfter")
	}
	if path, ok := p.env["PATH"]; !ok || path != "/o&p/bin:/usr/bin" || len(p.env) != 1 {
		t.Fatalf("env = %q", p.env)
	}
}

func doctorProtocol(theirs string) (string, bool) {
	var b strings.Builder
	d := &doctor{out: &b}
	d.protocol(theirs)
	return b.String(), d.failed
}

// TestDoctorProtocolOlderServer: an older serve is not a failure here.
func TestDoctorProtocolOlderServer(t *testing.T) {
	old := strconv.Itoa(wire.ProtocolVersion - 1)
	out, failed := doctorProtocol(old)
	want := "– the server speaks protocol " + old + " and your dugout " + strconv.Itoa(wire.ProtocolVersion) +
		": update the server\n"
	if out != want || failed {
		t.Fatalf("older serve: %q (failed %v), want %q", out, failed, want)
	}
}

// TestDoctorProtocolNewerServer: a newer serve means this dugout must update.
func TestDoctorProtocolNewerServer(t *testing.T) {
	newer := strconv.Itoa(wire.ProtocolVersion + 1)
	out, failed := doctorProtocol(newer)
	want := "✗ the server speaks protocol " + newer + " and your dugout " + strconv.Itoa(wire.ProtocolVersion) +
		": update dugout (docs/install.md, Update)\n"
	if out != want || !failed {
		t.Fatalf("newer serve: %q (failed %v), want %q", out, failed, want)
	}
	if out, failed := doctorProtocol(strconv.Itoa(wire.ProtocolVersion)); !strings.HasPrefix(out, "✓ ") || failed {
		t.Fatalf("equal protocol: %q (failed %v), want a ✓", out, failed)
	}
}

// TestDoctorInsecureServer: a daemon installed against an http:// server
// outside this machine without -insecure refuses it, so doctor says so and does
// not send the token there.
func TestDoctorInsecureServer(t *testing.T) {
	skipUnlessService(t)
	const server = "http://192.0.2.1:7457"
	w := newDoctorWorld(t, "dugout@dugout")
	w.writePlist(t, bin, server)
	out, _, code := runIn(t, w.env, "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wantLines(t, out, "✗ "+server+" has no TLS and is outside this machine: the daemon only talks to it when installed with -insecure, and the token is not sent to it from here (use https, or dugout install -insecure -server "+server+")")
	if strings.Contains(out, "the server "+server) {
		t.Fatalf("doctor contacted the insecure server:\n%s", out)
	}
}
