package launchd

import (
	"strings"
	"testing"
)

func TestPlist(t *testing.T) {
	got := string(Plist("/Users/a b/go/bin/dugout&co", "http://10.0.0.1:7457/?x=1&y=<2>", "/Users/a b", "/opt/a&b/bin:/usr/bin:/bin", false))
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.dugout.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/a b/go/bin/dugout&amp;co</string>
		<string>daemon</string>
		<string>-server</string>
		<string>http://10.0.0.1:7457/?x=1&amp;y=&lt;2&gt;</string>
		<string>-log-format</string>
		<string>json</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/a&amp;b/bin:/usr/bin:/bin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/Users/a b/Library/Logs/dugout/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/a b/Library/Logs/dugout/daemon.log</string>
</dict>
</plist>
`
	if got != want {
		t.Fatalf("plist mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlistEscapesQuotes(t *testing.T) {
	got := string(Plist(`/b/"x"'y'`, "s", "/h", "/p", false))
	if want := "<string>/b/&quot;x&quot;&apos;y&apos;</string>"; !strings.Contains(got, want) {
		t.Fatalf("missing %s in %s", want, got)
	}
}

func TestPaths(t *testing.T) {
	if got := PlistPath("/h"); got != "/h/Library/LaunchAgents/com.dugout.daemon.plist" {
		t.Fatal(got)
	}
	if got := LogPath("/h"); got != "/h/Library/Logs/dugout/daemon.log" {
		t.Fatal(got)
	}
}
