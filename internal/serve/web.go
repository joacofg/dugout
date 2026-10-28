package serve

import (
	"embed"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// The office page: static files embedded in the binary and served under
// /office. They carry no data, so they need no token; the page reads the
// office with a view token (office.go).

//go:embed web
var webFS embed.FS

// webCSP keeps every resource of the page on serve's own origin.
const webCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// isWebPath reports whether the mux routes p to the page, p cleaned as the
// mux cleans it.
func isWebPath(p string) bool {
	c := path.Clean(p)
	return c == p && (c == "/office" || strings.HasPrefix(c, "/office/")) || p == "/office/"
}

// webHandler serves the page's files, index.html at /office and /office/.
func webHandler() http.Handler {
	files, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	server := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", webCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		name := strings.TrimPrefix(r.URL.Path, "/office")
		if name == "" || name == "/" || name == "/index.html" {
			http.ServeFileFS(w, r, files, "index.html")
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = name
		server.ServeHTTP(w, r2)
	})
}

// sameOrigin reports whether Origin header origin names host itself, as a
// browser sends it on the page's own font loads. The scheme is not compared:
// serve may sit behind a TLS proxy and not know it.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && normalizeHost(u.Host) == normalizeHost(host) && u.Port() == portOf(host)
}

// portOf is hostport's port, "" without one.
func portOf(hostport string) string {
	if _, port, err := net.SplitHostPort(hostport); err == nil {
		return port
	}
	return ""
}
