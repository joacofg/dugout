// Package systemd renders the systemd --user unit that keeps the Dugout
// daemon running on Linux, the counterpart of internal/launchd on a Mac, and
// reads it back for doctor. It is pure: it computes paths and text and
// touches nothing.
package systemd

import (
	"errors"
	"path/filepath"
	"strings"
)

// Unit is the unit's name, as systemctl takes it.
const Unit = "dugout.service"

// UnitPath is where the unit lives for the user whose home is home: under
// $XDG_CONFIG_HOME when it is absolute (the XDG spec ignores a relative
// one), else ~/.config, which is where systemd --user looks.
func UnitPath(home string, getenv func(string) string) string {
	config := filepath.Join(home, ".config")
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		config = x
	}
	return filepath.Join(config, "systemd", "user", Unit)
}

// LogPath is the file the unit sends the daemon's stdout and stderr to, in
// the state dir: logs are state by the XDG spec, and a file keeps doctor
// --bundle the same as on a Mac (journalctl needs no file of its own).
func LogPath(stateDir string) string { return filepath.Join(stateDir, "daemon.log") }

// Render renders the unit that runs `bin daemon -server <server>` with the
// user's session and restarts it if it dies, like the LaunchAgent's
// RunAtLoad and KeepAlive. env is the daemon's environment, in order: PATH,
// so it runs the same git as the shell that installed it, and whatever else
// install passes. Every value is quoted for systemd.
//
// Restart=always with RestartSec=10 and no start limit is launchd's
// KeepAlive: a daemon that exits at once (a locked keyring) is tried again
// every 10 s for as long as it takes, where systemd's default limit would
// give up after five quick restarts.
//
// insecure adds -insecure, which lets the daemon talk to an http:// server
// outside this machine.
func Render(bin, server, log string, insecure bool, env [][2]string) []byte {
	args := []string{bin, "daemon", "-server", server, "-log-format", "json"}
	if insecure {
		args = append(args, "-insecure")
	}
	var b strings.Builder
	b.WriteString(`# Written by dugout install; dugout uninstall removes it.
[Unit]
Description=Dugout daemon
StartLimitIntervalSec=0

[Service]
ExecStart=`)
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quote(a, true))
	}
	b.WriteByte('\n')
	for _, kv := range env {
		b.WriteString("Environment=" + quote(kv[0]+"="+kv[1], false) + "\n")
	}
	log = specifiers.Replace(log)
	b.WriteString(`Restart=always
RestartSec=10
StandardOutput=append:` + log + `
StandardError=append:` + log + `

[Install]
WantedBy=default.target
`)
	return []byte(b.String())
}

// specifiers escapes systemd's % specifiers, expanded in every setting.
var specifiers = strings.NewReplacer("%", "%%")

// quote double-quotes s with C escapes. In ExecStart systemd also expands
// $VAR, so exec doubles the $.
func quote(s string, exec bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '%':
			b.WriteString("%%")
		case '$':
			if exec {
				b.WriteString("$$")
			} else {
				b.WriteByte('$')
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Service is the part of the unit doctor reads back.
type Service struct {
	Args []string          // ExecStart
	Env  map[string]string // Environment
}

var errSyntax = errors.New("unit: unclosed quotes")

// Parse reads ExecStart and Environment from a unit Render wrote. It undoes
// Render's quoting and nothing more: it is no general unit parser.
func Parse(raw []byte) (Service, error) {
	s := Service{Env: map[string]string{}}
	for line := range strings.Lines(string(raw)) {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ExecStart":
			words, err := unquote(val, true)
			if err != nil {
				return s, err
			}
			s.Args = words
		case "Environment":
			words, err := unquote(val, false)
			if err != nil {
				return s, err
			}
			for _, w := range words {
				if k, v, ok := strings.Cut(w, "="); ok {
					s.Env[k] = v
				}
			}
		}
	}
	return s, nil
}

// unquote splits s into words as quote wrote them: double-quoted with C
// escapes, or bare.
func unquote(s string, exec bool) ([]string, error) {
	var words []string
	var w strings.Builder
	in, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case in && c == '\\' && i+1 < len(s):
			i++
			if s[i] == 'n' {
				w.WriteByte('\n')
			} else {
				w.WriteByte(s[i])
			}
		case c == '"':
			in, started = !in, true
		case (c == '%' || (exec && c == '$')) && i+1 < len(s) && s[i+1] == c:
			i++
			w.WriteByte(c)
		case !in && (c == ' ' || c == '\t'):
			if started {
				words = append(words, w.String())
				w.Reset()
				started = false
			}
		default:
			w.WriteByte(c)
			started = true
		}
	}
	if in {
		return nil, errSyntax
	}
	if started {
		words = append(words, w.String())
	}
	return words, nil
}
