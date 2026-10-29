package serve

import (
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestOfficePageNeedsNoToken: the page carries no data, so it loads without
// a token, under a CSP that keeps every resource on serve's own origin.
func TestOfficePageNeedsNoToken(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	for _, p := range []string{"/office", "/office/", "/office/app.js", "/office/app.css", "/office/pixel", "/office/pixel.css"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a token: status %d, want 200", p, resp.StatusCode)
			continue
		}
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s: CSP %q lacks %q", p, csp, want)
			}
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s: headers %v", p, resp.Header)
		}
	}
}

// TestOfficePageKeepsHostCheck: the page answers only on an allowed Host,
// so a rebound DNS name cannot load it to read the office.
func TestOfficePageKeepsHostCheck(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/office/", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /office/ with Host evil.example: status %d, want 403", resp.StatusCode)
	}
}

// TestOfficePageNoForms: nothing the page ships can send anything but a GET
// or write markup from data.
func TestOfficePageNoForms(t *testing.T) {
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) {
			return err
		}
		b, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		src := strings.ToLower(string(b))
		for _, banned := range []string{"<form", "method:", "innerhtml", "outerhtml", "insertadjacenthtml", "document.write", "eval(", "new function"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q", p, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestOfficeAssetsGetOnly: the page's routes take nothing but GET.
func TestOfficeAssetsGetOnly(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	for _, p := range []string{"/office/app.js", "/office/pixel"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			if code, _ := as(t, srv, "").do(m, p, "application/json", []byte("{}")); code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 405", m, p, code)
			}
		}
	}
}

// TestOfficeUnknownAsset: a file the page does not ship is a 404, and a path
// out of the page's folder reaches nothing else.
func TestOfficeUnknownAsset(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	for _, p := range []string{"/office/nope.js", "/office/../v1/state", "/office/%2e%2e/v1/state"} {
		code, b := as(t, srv, "").do(http.MethodGet, p, "", nil)
		if code == http.StatusOK || strings.Contains(string(b), "workstreams") {
			t.Errorf("GET %s: status %d %s, want no file and no data", p, code, b)
		}
	}
}

// TestOfficeFontsSameOrigin: a browser loads the page's fonts with an Origin
// header naming the page itself, which the page's files take; any other
// Origin, and any Origin on the API, is still refused.
func TestOfficeFontsSameOrigin(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	font := "/office/fonts/ibm-plex-sans-latin-400-normal.woff2"
	for _, c := range []struct {
		path, origin string
		want         int
	}{
		{font, srv.URL, http.StatusOK},
		{font, "http://evil.example", http.StatusForbidden},
		{"/v1/office", srv.URL, http.StatusForbidden},
	} {
		code, _ := as(t, srv, anaTok).do(http.MethodGet, c.path, "", nil, "Origin", c.origin)
		if code != c.want {
			t.Errorf("GET %s with Origin %s: status %d, want %d", c.path, c.origin, code, c.want)
		}
	}
}

// pageRefs is what a served page asks the browser to load: script and
// stylesheet URLs in document order, and its anchors.
type pageRefs struct{ scripts, styles, links []string }

var tagRe = regexp.MustCompile(`<(script|link|a)\s[^>]*>`)
var attrRe = regexp.MustCompile(`\b(src|href|rel)="([^"]*)"`)

func getPage(t *testing.T, base, path string) (pageRefs, *http.Response) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET %s: status %d, type %q, want 200 text/html", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var refs pageRefs
	for _, m := range tagRe.FindAllStringSubmatch(string(b), -1) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(m[0], -1) {
			attrs[a[1]] = a[2]
		}
		switch {
		case m[1] == "script":
			refs.scripts = append(refs.scripts, attrs["src"])
		case m[1] == "link" && attrs["rel"] == "stylesheet":
			refs.styles = append(refs.styles, attrs["href"])
		case m[1] == "a":
			refs.links = append(refs.links, attrs["href"])
		}
	}
	return refs, resp
}

// TestOfficeViewsServeTheirParts: each view is served under the page CSP,
// loads only same-origin scripts from /office/ with the shared client first,
// links to the other view, and every script and stylesheet it names answers.
func TestOfficeViewsServeTheirParts(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	for _, c := range []struct{ path, own, other string }{
		{"/office", "/office/app.js", "/office/pixel"},
		{"/office/pixel", "/office/pixel.js", "/office"},
	} {
		refs, resp := getPage(t, srv.URL, c.path)
		if csp := resp.Header.Get("Content-Security-Policy"); csp != webCSP {
			t.Errorf("GET %s: CSP %q, want the page CSP", c.path, csp)
		}
		if len(refs.scripts) == 0 || refs.scripts[0] != "/office/client.js" || refs.scripts[len(refs.scripts)-1] != c.own {
			t.Errorf("GET %s: scripts %v, want client.js first and %s last", c.path, refs.scripts, c.own)
		}
		if !slices.Contains(refs.links, c.other) {
			t.Errorf("GET %s: links %v, want one to %s", c.path, refs.links, c.other)
		}
		for _, u := range append(append([]string{}, refs.scripts...), refs.styles...) {
			if !strings.HasPrefix(u, "/office/") {
				t.Errorf("GET %s: loads %q, want a same-origin /office/ file", c.path, u)
				continue
			}
			r, err := http.Get(srv.URL + u)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if r.StatusCode != http.StatusOK || r.Header.Get("Content-Security-Policy") != webCSP {
				t.Errorf("GET %s (from %s): status %d, CSP %q, want 200 under the page CSP", u, c.path, r.StatusCode, r.Header.Get("Content-Security-Policy"))
			}
		}
	}
}
