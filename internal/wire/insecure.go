package wire

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// InsecureServer reports whether raw is an http:// URL to a host that is not
// loopback: the token would cross the network in the clear. Anything that is
// not such a URL (https, "off", an empty or unparsable value) is not.
func InsecureServer(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	return u.Hostname() != "" && !IsLoopbackURL(raw)
}

// ErrInsecureServer is a plain-http server outside this machine, used without
// the explicit opt-in: its token and bundles would cross the network in the
// clear.
var ErrInsecureServer = errors.New("the server is outside this machine and has no TLS: the token and the bundles would travel in the clear; use an https:// server, or pass -insecure to accept that")

// IsLoopbackURL reports whether raw is a URL whose host is this machine:
// localhost (any case) or a loopback IP. A URL without scheme or host is not.
func IsLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
