package systemd

import (
	"slices"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	got := string(Render("/home/a b/go/bin/dugout", "http://10.0.0.1:7457/?x=1&y=\"2\"", "/home/a b/.local/state/dugout/daemon.log", false,
		[][2]string{{"PATH", "/opt/a$b/bin:/usr/bin:/bin"}, {"XDG_STATE_HOME", "/x/100%"}}))
	want := `# Written by dugout install; dugout uninstall removes it.
[Unit]
Description=Dugout daemon
StartLimitIntervalSec=0

[Service]
ExecStart="/home/a b/go/bin/dugout" "daemon" "-server" "http://10.0.0.1:7457/?x=1&y=\"2\"" "-log-format" "json"
Environment="PATH=/opt/a$b/bin:/usr/bin:/bin"
Environment="XDG_STATE_HOME=/x/100%%"
Restart=always
RestartSec=10
StandardOutput=append:/home/a b/.local/state/dugout/daemon.log
StandardError=append:/home/a b/.local/state/dugout/daemon.log

[Install]
WantedBy=default.target
`
	if got != want {
		t.Fatalf("unit mismatch:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderEscapes: what systemd would expand or split is escaped: $ in
// ExecStart (not in Environment, where it means nothing), %, quotes,
// backslashes and newlines.
func TestRenderEscapes(t *testing.T) {
	got := string(Render(`/b/$HOME/%h/"x"\y`, "s\nt", "/l/%i.log", false, [][2]string{{"PATH", `/p/$X/%u/"q"`}}))
	for _, want := range []string{
		`ExecStart="/b/$$HOME/%%h/\"x\"\\y" "daemon" "-server" "s\nt" `,
		`Environment="PATH=/p/$X/%%u/\"q\""`,
		"StandardOutput=append:/l/%%i.log\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}

// TestParseRoundTrip: doctor reads back exactly what Render wrote, however
// odd the binary, server and PATH.
func TestParseRoundTrip(t *testing.T) {
	for _, tc := range []struct{ bin, server, path string }{
		{"/usr/local/bin/dugout", "https://dugout.example.internal", "/usr/bin:/bin"},
		{`/home/a b/$x/%h/"q"\z/dugout`, "http://h:1/?x=1&y=2 3", "/o p/$Q/100%/bin:/usr/bin"},
		{"/t\tab/dugout", "s\nt", ""},
	} {
		s, err := Parse(Render(tc.bin, tc.server, "/l/daemon.log", false, [][2]string{{"PATH", tc.path}, {"XDG_STATE_HOME", "/s t"}}))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{tc.bin, "daemon", "-server", tc.server, "-log-format", "json"}; !slices.Equal(s.Args, want) {
			t.Errorf("args = %q, want %q", s.Args, want)
		}
		if s.Env["PATH"] != tc.path || s.Env["XDG_STATE_HOME"] != "/s t" || len(s.Env) != 2 {
			t.Errorf("env = %q, want PATH %q", s.Env, tc.path)
		}
	}
}

func TestParseRejectsOpenQuote(t *testing.T) {
	if _, err := Parse([]byte("[Service]\nExecStart=\"/bin/dugout daemon\n")); err == nil {
		t.Fatal("an open quote parsed")
	}
	s, err := Parse([]byte("[Service]\nRestart=always\n"))
	if err != nil || len(s.Args) != 0 {
		t.Fatalf("a unit without ExecStart = %+v, %v", s, err)
	}
}

func TestPaths(t *testing.T) {
	for _, tc := range []struct{ xdg, want string }{
		{"", "/h/.config/systemd/user/dugout.service"},
		{"/xdg/config", "/xdg/config/systemd/user/dugout.service"},
		{"relative", "/h/.config/systemd/user/dugout.service"},
	} {
		getenv := func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return tc.xdg
			}
			return ""
		}
		if got := UnitPath("/h", getenv); got != tc.want {
			t.Errorf("UnitPath(XDG_CONFIG_HOME=%q) = %q, want %q", tc.xdg, got, tc.want)
		}
	}
	if got := LogPath("/s"); got != "/s/daemon.log" {
		t.Fatal(got)
	}
}
