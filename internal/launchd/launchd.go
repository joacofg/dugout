// Package launchd renders the LaunchAgent that keeps the Dugout daemon running
// on a Mac. It is pure: it computes paths and text and touches nothing.
package launchd

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Label is the job's label and the plist's file name without extension.
const Label = "com.dugout.daemon"

// PlistPath is where the LaunchAgent lives for the user whose home is home.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

// LogPath is the file launchd sends the daemon's stdout and stderr to.
func LogPath(home string) string {
	return filepath.Join(home, "Library", "Logs", "dugout", "daemon.log")
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// Plist renders the LaunchAgent that runs `bin daemon -server <server>` at
// login and restarts it if it dies. path is the daemon's PATH, so it runs the
// same git as the shell that installed it. insecure adds -insecure, which lets
// the daemon talk to an http:// server outside this machine. Every value is
// XML-escaped.
func Plist(bin, server, home, path string, insecure bool) []byte {
	log := xmlEscaper.Replace(LogPath(home))
	args := []string{bin, "daemon", "-server", server, "-log-format", "json"}
	if insecure {
		args = append(args, "-insecure")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range args {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscaper.Replace(a))
	}
	b.WriteString(`	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + xmlEscaper.Replace(path) + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + log + `</string>
	<key>StandardErrorPath</key>
	<string>` + log + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}
