package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/launchd"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/systemd"
)

// onPlatform makes install, uninstall, doctor and the bundle take platform
// p's paths for an in-process test.
func onPlatform(t *testing.T, p string) {
	t.Helper()
	old := goos
	goos = p
	t.Cleanup(func() { goos = old })
}

func skipUnlessService(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the daemon is installed on macOS and Linux only")
	}
}

// noEnv is the getenv of a built dugout run with an explicit env: no XDG
// vars, so the defaults under HOME.
func noEnv(string) string { return "" }

// The service of this platform, as the built dugout sees it in a test env
// without XDG vars: its kind in doctor's lines, its file in the bundle, and
// where it and its log go for home.
func serviceKind() string {
	if runtime.GOOS == "linux" {
		return "systemd service"
	}
	return "LaunchAgent"
}

func serviceFile() string {
	if runtime.GOOS == "linux" {
		return systemd.Unit
	}
	return "LaunchAgent.plist"
}

func serviceInstalled() string {
	if runtime.GOOS == "linux" {
		return "✓ the daemon is installed as a systemd --user service"
	}
	return "✓ the daemon is installed as a LaunchAgent"
}

func serviceLog(home string) string {
	if runtime.GOOS == "linux" {
		return systemd.LogPath(statedir.DefaultDir("linux", home, noEnv))
	}
	return launchd.LogPath(home)
}

// writeService installs this platform's LaunchAgent or unit under home for
// exe and server with pathEnv as its PATH, as install would, without loading
// it.
func writeService(t *testing.T, home, exe, server, pathEnv string) {
	t.Helper()
	path, body := launchd.PlistPath(home), launchd.Plist(exe, server, home, pathEnv, false)
	if runtime.GOOS == "linux" {
		path = systemd.UnitPath(home, noEnv)
		body = systemd.Render(exe, server, serviceLog(home), false, [][2]string{{"PATH", pathEnv}})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// linuxWorld runs install, uninstall and doctor in-process as on Linux: a temp
// HOME, the XDG vars given (or unset), and a fake systemctl first on PATH
// that runs script. It returns the home and the systemctl log.
func linuxWorld(t *testing.T, xdgConfig, xdgState, script string) (home, log string) {
	t.Helper()
	onPlatform(t, "linux")
	home = t.TempDir()
	dir, log := fakeTool(t, "systemctl", script)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("XDG_STATE_HOME", xdgState)
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return home, log
}

func runHere(args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errb)
	return out.String(), errb.String(), code
}

func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestInstallSystemdTwice: on Linux install writes the unit, the log dir and
// the bin link, and has systemd reload, enable and restart it, each time: a
// second install restarts the daemon, as bootout and bootstrap do on a Mac.
func TestInstallSystemdTwice(t *testing.T) {
	home, log := linuxWorld(t, "", "", "")
	for i := range 2 {
		out, errOut, code := runHere("install", "-server", "http://127.0.0.1:7457")
		if code != 0 || out != "Dugout installed: the daemon starts with your session and talks to http://127.0.0.1:7457.\n" {
			t.Fatalf("run %d: code=%d out=%q err=%q", i, code, out, errOut)
		}
	}
	one := []string{"--user daemon-reload", "--user enable dugout.service", "--user restart dugout.service"}
	if got := readLog(t, log); !slices.Equal(got, append(one, one...)) {
		t.Fatalf("systemctl calls = %q", got)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "dugout.service")
	raw, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	exe := testExe(t)
	s, err := systemd.Parse(raw)
	if err != nil || !slices.Equal(s.Args, []string{exe, "daemon", "-server", "http://127.0.0.1:7457", "-log-format", "json"}) {
		t.Fatalf("ExecStart = %q, %v:\n%s", s.Args, err, raw)
	}
	if s.Env["PATH"] != os.Getenv("PATH") || len(s.Env) != 1 {
		t.Fatalf("Environment = %q, want only this PATH", s.Env)
	}
	state := filepath.Join(home, ".local", "state", "dugout")
	for _, want := range []string{"Restart=always\n", "StandardOutput=append:" + state + "/daemon.log\n", "WantedBy=default.target\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("unit lacks %q:\n%s", want, raw)
		}
	}
	if st, err := os.Stat(unit); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("unit mode: %v %v", st, err)
	}
	if st, err := os.Stat(state); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v, want a 0700 dir", st, err)
	}
	if got, err := os.Readlink(filepath.Join(state, "bin", "dugout")); err != nil || got != exe {
		t.Fatalf("bin link -> %q, %v; want %q", got, err, exe)
	}
}

// TestInstallSystemdXDG: an absolute XDG_CONFIG_HOME takes the unit and an
// absolute XDG_STATE_HOME the state dir, the log and the link; the unit
// passes XDG_STATE_HOME on, since the user's systemd does not read the
// shell's rc files.
func TestInstallSystemdXDG(t *testing.T) {
	config, stateHome := t.TempDir(), t.TempDir()
	home, _ := linuxWorld(t, config, stateHome, "")
	if _, errOut, code := runHere("install", "-server", "http://127.0.0.1:1"); code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(config, "systemd", "user", "dugout.service"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := systemd.Parse(raw)
	if s.Env["XDG_STATE_HOME"] != stateHome {
		t.Fatalf("Environment = %q, want XDG_STATE_HOME=%s", s.Env, stateHome)
	}
	if !strings.Contains(string(raw), "StandardOutput=append:"+filepath.Join(stateHome, "dugout", "daemon.log")+"\n") {
		t.Fatalf("the log is not in XDG_STATE_HOME:\n%s", raw)
	}
	if _, err := os.Readlink(filepath.Join(stateHome, "dugout", "bin", "dugout")); err != nil {
		t.Fatalf("no bin link in XDG_STATE_HOME: %v", err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("install wrote in HOME with both XDG vars set: %v", entries)
	}
}

// TestInstallSystemdFails: a systemctl that cannot reach the user's systemd
// (an ssh session without one) fails install with its own words, and stops
// at the first failing step.
func TestInstallSystemdFails(t *testing.T) {
	_, log := linuxWorld(t, "", "", `[ "$2" = daemon-reload ] && { echo 'Failed to connect to bus: No medium found' >&2; exit 1; }
`)
	out, errOut, code := runHere("install", "-server", "http://127.0.0.1:1")
	if code != 1 || out != "" || errOut != "dugout install: systemctl --user daemon-reload: exit status 1: Failed to connect to bus: No medium found\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got := readLog(t, log); !slices.Equal(got, []string{"--user daemon-reload"}) {
		t.Fatalf("systemctl calls = %q", got)
	}
}

// TestInstallSystemdDryRun prints the unit and touches nothing.
func TestInstallSystemdDryRun(t *testing.T) {
	home, log := linuxWorld(t, "", "", "")
	out, _, code := runHere("install", "-server", "http://127.0.0.1:1", "-dry-run")
	if code != 0 || !strings.Contains(out, "[Service]\nExecStart=") || !strings.Contains(out, `"-server" "http://127.0.0.1:1"`) {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("dry-run called systemctl")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("dry-run touched HOME: %v", entries)
	}
}

// TestUninstallSystemd stops and disables the unit before removing it and
// the link, then has systemd forget it; a second uninstall finds nothing.
func TestUninstallSystemd(t *testing.T) {
	home, log := linuxWorld(t, "", "", "")
	if _, errOut, code := runHere("install", "-server", "http://127.0.0.1:1"); code != 0 {
		t.Fatalf("install: %s", errOut)
	}
	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runHere("uninstall")
	if code != 0 || out != "Dugout uninstalled.\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got := readLog(t, log); !slices.Equal(got, []string{"--user disable --now dugout.service", "--user daemon-reload"}) {
		t.Fatalf("systemctl calls = %q", got)
	}
	for _, p := range []string{
		filepath.Join(home, ".config", "systemd", "user", "dugout.service"),
		filepath.Join(home, ".local", "state", "dugout", "bin", "dugout"),
	} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s still exists", p)
		}
	}
	if out, _, code := runHere("uninstall"); code != 0 || out != "Dugout was not installed.\n" {
		t.Fatalf("second uninstall: code=%d out=%q", code, out)
	}
}

// doctorUnit runs doctor's service check alone, as on Linux.
func doctorUnit() (string, agent, bool) {
	var b strings.Builder
	d := &doctor{out: &b}
	a := d.service()
	return b.String(), a, d.failed
}

// TestDoctorSystemdUnit: on Linux doctor reads back the unit's -server and
// PATH and asks systemctl whether it is active, with a line for each way it
// can be wrong.
func TestDoctorSystemdUnit(t *testing.T) {
	home, log := linuxWorld(t, "", "", `[ "$2" = is-active ] && [ -f "$LOG.inactive" ] && exit 3
`)
	t.Setenv(envServer, "http://from-the-shell")
	out, a, failed := doctorUnit()
	if out != "✗ the daemon is not installed: run dugout install -server <url>\n" || !failed || a.installed || a.server != "http://from-the-shell" {
		t.Fatalf("no unit: %q failed=%v %+v", out, failed, a)
	}

	unit := systemd.UnitPath(home, os.Getenv)
	write := func(body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unit, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exe := testExe(t)
	write(systemd.Render(exe, "http://10.0.0.1:7457", "/l", false, [][2]string{{"PATH", "/opt/git/bin:/usr/bin"}}))
	out, a, failed = doctorUnit()
	if out != "✓ the daemon is installed as a systemd --user service\n" || failed {
		t.Fatalf("active unit: %q failed=%v", out, failed)
	}
	if want := (agent{installed: true, kind: "systemd service", server: "http://10.0.0.1:7457", path: "/opt/git/bin:/usr/bin"}); a != want {
		t.Fatalf("agent = %+v, want %+v", a, want)
	}
	if got := readLog(t, log); !slices.Equal(got, []string{"--user is-active dugout.service"}) {
		t.Fatalf("systemctl calls = %q", got)
	}

	if err := os.WriteFile(log+".inactive", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, failed := doctorUnit(); out != "✗ the systemd service is not active: run dugout install again; if it stays the same, check systemctl --user status dugout\n" || !failed {
		t.Fatalf("inactive unit: %q failed=%v", out, failed)
	}

	write(systemd.Render(filepath.Join(home, "moved", "dugout"), "http://x", "/l", false, nil))
	if out, _, _ := doctorUnit(); out != "✗ the systemd service points to a binary that no longer exists: run dugout install again\n" {
		t.Fatalf("stale binary: %q", out)
	}

	write([]byte("[Service]\nExecStart=\"/bin/dugout daemon\n"))
	if out, a, _ := doctorUnit(); out != "✗ the systemd service is unreadable: run dugout install again\n" || a.installed {
		t.Fatalf("unreadable unit: %q %+v", out, a)
	}
}

// TestDoctorLinuxMessages: on Linux doctor names the unit where the Mac names
// the LaunchAgent, and points to the log in the state dir.
func TestDoctorLinuxMessages(t *testing.T) {
	linuxWorld(t, "", "", "")
	t.Setenv(statedir.EnvHome, shortDir(t))
	var b strings.Builder
	d := &doctor{out: &b}
	d.daemonSocket()
	d.git(agent{installed: true, kind: "systemd service", path: t.TempDir()})
	d.serverAndProtocol(agent{installed: true, kind: "systemd service", server: "off"})
	want := "✗ the daemon is not responding: check ~/.local/state/dugout/daemon.log\n" +
		"✗ could not find git on the systemd service PATH: run dugout install again from a terminal with git on the PATH\n" +
		"– server: the daemon runs without a server\n"
	if b.String() != want {
		t.Fatalf("doctor said:\n%s\nwant:\n%s", b.String(), want)
	}
	onPlatform(t, "freebsd")
	if out, _, failed := doctorUnit(); out != "– installed daemon: only on macOS and Linux\n" || failed {
		t.Fatalf("freebsd: %q failed=%v", out, failed)
	}
}

// TestLogHintMac: on a Mac the line stays what it was.
func TestLogHintMac(t *testing.T) {
	onPlatform(t, "darwin")
	t.Setenv("HOME", t.TempDir())
	if got := logHint(); got != "~/Library/Logs/dugout/daemon.log" {
		t.Fatalf("logHint = %q", got)
	}
}
