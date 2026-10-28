package serve

import (
	"io"
	"io/fs"
	"net/http"
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

// TestOfficePixelPage: the pixel office is another page of the same files,
// at /office/pixel, under the same headers, loading the shared client.
func TestOfficePixelPage(t *testing.T) {
	_, srv, _ := stateServer(t, repoApp)
	for _, p := range []string{"/office/pixel"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s: status %d, type %q, want 200 text/html", p, resp.StatusCode, resp.Header.Get("Content-Type"))
			continue
		}
		if csp := resp.Header.Get("Content-Security-Policy"); csp != webCSP {
			t.Errorf("GET %s: CSP %q, want the page CSP", p, csp)
		}
		for _, want := range []string{`<canvas id="floor"`, `/office/client.js`, `/office/pixel-art.js`, `/office/pixel.js`, `href="/office"`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("GET %s: page lacks %q", p, want)
			}
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

// TestOfficeClientOnlyReads: the pages make one kind of request, the office
// stream, from one place; nothing they ship can open another channel.
func TestOfficeClientOnlyReads(t *testing.T) {
	fetches := 0
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		src := string(b)
		for _, banned := range []string{"XMLHttpRequest", "EventSource", "sendBeacon", "WebSocket", "serviceWorker"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s contains %q", p, banned)
			}
		}
		for i := strings.Index(src, "fetch("); i >= 0; i = strings.Index(src, "fetch(") {
			fetches++
			if p != "web/client.js" || !strings.HasPrefix(src[i:], `fetch("/v1/office/stream"`) {
				t.Errorf("%s: a fetch other than the office stream: %.60q", p, src[i:])
			}
			src = src[i+len("fetch("):]
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Errorf("%d fetch calls in the pages, want the one in client.js", fetches)
	}
}

// TestOfficeViewsLinkEachOther: each view links to the other, and the clear
// view loads the shared client before its own script.
func TestOfficeViewsLinkEachOther(t *testing.T) {
	read := func(name string) string {
		b, err := fs.ReadFile(webFS, "web/"+name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	index, pixel := read("index.html"), read("pixel.html")
	if !strings.Contains(index, `id="to-pixel" href="/office/pixel"`) {
		t.Error("index.html does not link to the pixel office")
	}
	if !strings.Contains(pixel, `id="to-clear" href="/office"`) {
		t.Error("pixel.html does not link to the clear view")
	}
	c, a := strings.Index(index, "/office/client.js"), strings.Index(index, "/office/app.js")
	if c < 0 || a < 0 || c > a {
		t.Errorf("index.html loads client.js at %d and app.js at %d, want client.js first", c, a)
	}
}
