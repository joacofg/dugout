package serve

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// TestOfficePageNeedsNoToken: the page carries no data, so it loads without
// a token, under a CSP that keeps every resource on serve's own origin.
func TestOfficePageNeedsNoToken(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	for _, p := range []string{"/office", "/office/", "/office/app.js", "/office/app.css"} {
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
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if code, _ := as(t, srv, "").do(m, "/office/app.js", "application/json", []byte("{}")); code != http.StatusMethodNotAllowed {
			t.Errorf("%s /office/app.js: status %d, want 405", m, code)
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
