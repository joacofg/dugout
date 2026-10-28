package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/wire"
)

const insecureServer = "http://10.0.0.5:7457"

// TestInstallRejectsInsecureServer: an http:// server outside this machine
// sends the token in the clear, so install refuses it, names -insecure and
// writes nothing.
func TestInstallRejectsInsecureServer(t *testing.T) {
	home, log := linuxWorld(t, "", "", "")
	out, errOut, code := runHere("install", "-server", insecureServer)
	if code != 2 || out != "" || !strings.Contains(errOut, "-insecure") || !strings.Contains(errOut, insecureServer) {
		t.Fatalf("code=%d out=%q err=%q, want exit 2 and a message naming the server and -insecure", code, out, errOut)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("install called systemctl")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("install touched HOME: %v", entries)
	}
}

// TestInstallInsecureFlag: with -insecure install goes ahead, warns, and
// hands the flag on to the daemon, which would refuse the server without it.
func TestInstallInsecureFlag(t *testing.T) {
	home, _ := linuxWorld(t, "", "", "")
	out, errOut, code := runHere("install", "-insecure", "-server", insecureServer)
	if code != 0 || !strings.Contains(out, "Dugout installed") || !strings.Contains(out, "Warning: "+insecureServer+" has no TLS") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "dugout.service"))
	if err != nil || !strings.Contains(string(raw), `"-server" "`+insecureServer+`" "-log-format" "json" "-insecure"`) {
		t.Fatalf("unit = %q, %v, want the daemon started with -insecure", raw, err)
	}
}

// TestInstallNeedsNoFlagOnLoopbackOrTLS: a loopback server, by name or by
// address, and any https:// one install without -insecure, and the daemon's
// command line stays as it was.
func TestInstallNeedsNoFlagOnLoopbackOrTLS(t *testing.T) {
	linuxWorld(t, "", "", "")
	for _, server := range []string{"http://127.0.0.1:7457", "http://localhost:7457", "http://LOCALHOST:7457", "http://[::1]:7457", "https://10.0.0.5:7457", "https://dugout.example.internal"} {
		out, errOut, code := runHere("install", "-server", server, "-dry-run")
		if code != 0 || strings.Contains(out, "-insecure") || strings.Contains(errOut, "-insecure") {
			t.Errorf("install -server %s: code=%d out=%q err=%q, want it to work without a flag", server, code, out, errOut)
		}
	}
}

// TestLoginInsecureServer: login stores a token for the same servers install
// accepts, and for an http:// one outside this machine only with -insecure.
func TestLoginInsecureServer(t *testing.T) {
	secDir, store, argsLog := fakeSecurity(t)
	env := secEnv(t, secDir, shortDir(t))

	out, errOut, code := runIn(t, env, loginToken+"\n", "login", "-server", insecureServer)
	if code != 2 || out != "" || !strings.Contains(errOut, "-insecure") {
		t.Fatalf("login: code=%d out=%q err=%q, want exit 2 naming -insecure", code, out, errOut)
	}
	if _, err := os.Stat(argsLog); err == nil {
		t.Fatalf("login reached the keychain: %s", readLog(t, argsLog))
	}

	out, errOut, code = runIn(t, env, loginToken+"\n", "login", "-insecure", "-server", insecureServer)
	if code != 0 || !strings.Contains(out, "Token saved") || !strings.Contains(out, "Warning: "+insecureServer+" has no TLS") ||
		!strings.Contains(out, "dugout install -insecure -server "+insecureServer) {
		t.Fatalf("login -insecure: code=%d out=%q err=%q", code, out, errOut)
	}
	if b, _ := os.ReadFile(store); !strings.Contains(string(b), insecureServer+"\t"+loginToken) {
		t.Fatalf("store = %q", b)
	}

	for _, server := range []string{"http://127.0.0.1:7457", "http://localhost:7457", "https://10.0.0.5:7457"} {
		out, errOut, code := runIn(t, env, loginToken+"\n", "login", "-server", server)
		if code != 0 || strings.Contains(out, "Warning") {
			t.Errorf("login -server %s: code=%d out=%q err=%q, want it to work without a flag or a warning", server, code, out, errOut)
		}
	}
}

// TestDaemonInsecureServer: the daemon, which is what the uplink runs in,
// does not connect to an http:// server outside this machine unless it was
// started with -insecure; then it connects and logs the warning.
func TestDaemonInsecureServer(t *testing.T) {
	secDir, _, _ := fakeSecurity(t)
	home := shortDir(t)
	// A shell token for the configured server, so the only thing in the way is
	// the missing flag.
	env := append(secEnv(t, secDir, home), envServer+"="+insecureServer, wire.EnvToken+"="+loginToken)

	refused := start(t, env, "daemon", "-poll", "300ms")
	waitDaemonSocket(t, home)
	eventually(t, 5*time.Second, "the refusal in the log", func() bool { return strings.Contains(refused.log.String(), "not connecting to "+insecureServer) })
	log := refused.log.String()
	if !strings.Contains(log, "-insecure") || !strings.Contains(log, `server=""`) || strings.Contains(log, "server without TLS") {
		t.Fatalf("a daemon without -insecure:\n%s", log)
	}
	refused.stop()

	allowed := start(t, env, "daemon", "-insecure", "-poll", "300ms")
	waitDaemonSocket(t, home)
	eventually(t, 5*time.Second, "the TLS warning in the log", func() bool { return strings.Contains(allowed.log.String(), "server without TLS") })
	if log := allowed.log.String(); strings.Contains(log, "not connecting") || !strings.Contains(log, `server=`+insecureServer) {
		t.Fatalf("a daemon with -insecure:\n%s", log)
	}
}

// TestEnvTokenIsBoundToItsServer: $DUGOUT_TOKEN goes to the server of the
// configuration, $DUGOUT_SERVER, and to a server on this machine, never to
// another -server.
func TestEnvTokenIsBoundToItsServer(t *testing.T) {
	const configured = "https://dugout.example.internal"
	for _, tc := range []struct {
		name, configured, server string
		want                     bool
	}{
		{"the configured server", configured, configured, true},
		{"the configured server, slash and spaces", configured, " " + configured + "/ ", true},
		{"another server", configured, "https://other.example", false},
		{"another server, no configuration", "", "https://other.example", false},
		{"an insecure server it was not issued for", configured, insecureServer, false},
		{"the default server, no configuration", "", "http://127.0.0.1:7457", true},
		{"localhost", configured, "http://localhost:9000", true},
		{"a loopback address", "", "http://[::1]:7457", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envServer, tc.configured)
			t.Setenv(wire.EnvToken, loginToken)
			want := ""
			if tc.want {
				want = loginToken
			}
			if got := envToken(tc.server); got != want {
				t.Errorf("envToken(%q) = %q, want %q", tc.server, got, want)
			}
		})
	}
	t.Run("no token", func(t *testing.T) {
		t.Setenv(envServer, configured)
		t.Setenv(wire.EnvToken, "")
		if got := envToken(configured); got != "" {
			t.Errorf("envToken = %q, want none", got)
		}
	})
}

// TestResolveTokenDoesNotLeakEnvToken: with the shell's token set and a
// -server other than the configured one, resolveToken falls to that server's
// own keychain item rather than hand the shell's token over.
func TestResolveTokenDoesNotLeakEnvToken(t *testing.T) {
	secDir, _, _ := fakeSecurity(t)
	env := secEnv(t, secDir, shortDir(t))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	t.Setenv(envServer, "https://configured.example")
	t.Setenv(wire.EnvToken, loginToken)
	if tok, err := resolveToken("https://configured.example"); err != nil || tok != loginToken {
		t.Fatalf("resolveToken(configured) = %q, %v, want the shell's token", tok, err)
	}
	if tok, err := resolveToken("https://elsewhere.example"); err != nil || tok != "" {
		t.Fatalf("resolveToken(elsewhere) = %q, %v, want no token", tok, err)
	}
	const other = "dgt_ZYXWVUTSRQPONMLKJIHGFEDCBAzyxwvutsrq98765-_"
	if _, errOut, code := runIn(t, env, other+"\n", "login", "-server", "https://elsewhere.example"); code != 0 {
		t.Fatalf("login: %d %s", code, errOut)
	}
	if tok, err := resolveToken("https://elsewhere.example"); err != nil || tok != other {
		t.Fatalf("resolveToken(elsewhere) after login = %q, %v, want its own keychain token", tok, err)
	}
}
