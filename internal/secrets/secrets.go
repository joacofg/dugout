// Package secrets finds credentials in a snapshot before it is uploaded.
// Scan returns the name of the first rule that matches and never the matched
// value, so a caller can log the rule and the path without leaking the secret.
package secrets

import (
	"net"
	"regexp"
	"strings"
)

type rule struct {
	name string
	re   *regexp.Regexp
	// benign, when set, clears a match from its submatches: a hit blocks the
	// upload, and the teammate never hears of the conflict.
	benign func(sub [][]byte) bool
}

// rules is ordered: Scan reports the first rule that matches. An Anthropic
// key matches the OpenAI rule too, so it goes first.
var rules = []rule{
	{name: "private-key", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{name: "gitlab-token", re: regexp.MustCompile(`\bgl(?:pat|dt|rt|ptt)-[A-Za-z0-9_\-]{20,}`)},
	{name: "npm-token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "slack-token", re: regexp.MustCompile(`\b(?:xox[abposr]|xapp)-[A-Za-z0-9-]{10,}`)},
	{name: "slack-webhook", re: regexp.MustCompile(`hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9+/_\-]{20,}`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{name: "stripe-key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{24,}`)},
	{name: "anthropic-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`)},
	{name: "openai-key", re: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_\-]{20,}`)},
	// scheme://user:password@host, with the user, the password and the host
	// captured.
	{
		name:   "url-password",
		re:     regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://([^\s/:@'"` + "`" + `]*):([^\s/@'"` + "`" + `]+)@(\[[0-9a-f:]+\]|[^\s/:?#'"` + "`" + `]+)`),
		benign: localOrPlaceholder,
	},
	// header.payload.signature, both JSON objects; an unsigned token is no
	// credential.
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{16,}`)},
}

// devPasswords are the passwords of dev containers and tutorials, which a
// docker-compose file names with its service's host (postgres:postgres@db).
var devPasswords = map[string]bool{
	"password": true, "pass": true, "postgres": true, "secret": true, "changeme": true,
	"root": true, "admin": true, "test": true, "dev": true, "example": true,
}

// localOrPlaceholder is a URL password that is a template ($VAR, ${VAR},
// <password>, {{ }}, %s, ****), the user's name or a dev default, or one for
// a loopback or reserved example host: the dev and docs URLs a repo is full
// of.
func localOrPlaceholder(sub [][]byte) bool {
	user, pass := strings.ToLower(string(sub[1])), strings.ToLower(string(sub[2]))
	if strings.IndexByte("$<{%*", pass[0]) >= 0 || pass == user || devPasswords[pass] {
		return true
	}
	host := strings.ToLower(string(sub[3]))
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return true
	}
	// RFC 2606 and 6761 names, and their subdomains.
	for _, d := range []string{"localhost", "example.com", "example.org", "example.net", "example", "test", "invalid"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Scan reports the name of the first rule that matches content, or false when
// none does.
func Scan(content []byte) (rule string, found bool) {
	for _, r := range rules {
		if r.benign == nil {
			if r.re.Match(content) {
				return r.name, true
			}
			continue
		}
		for _, sub := range r.re.FindAllSubmatch(content, -1) {
			if !r.benign(sub) {
				return r.name, true
			}
		}
	}
	return "", false
}
