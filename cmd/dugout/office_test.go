package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/serve"
	"github.com/joacofg/dugout/internal/statedir"
	"github.com/joacofg/dugout/internal/wire"
)

func office(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(append([]string{"office"}, args...), strings.NewReader(""), &out, &errb)
	return out.String(), errb.String(), code
}

// TestOfficePrintsURL: dugout office asks serve for a view token and prints
// the page's URL with it in the fragment, which never reaches a server log.
func TestOfficePrintsURL(t *testing.T) {
	srv := httptest.NewServer(serve.New().Handler())
	t.Cleanup(srv.Close)
	t.Setenv(wire.EnvToken, "dgt_test")
	out, errb, code := office(t, "-server", srv.URL, "-no-open")
	if code != 0 || !strings.Contains(out, srv.URL+"/office#t="+wire.ViewTokenPrefix) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the URL with a view token", code, out, errb)
	}
}

// TestOfficeOpensBrowser: without -no-open the URL goes to the browser too.
func TestOfficeOpensBrowser(t *testing.T) {
	srv := httptest.NewServer(serve.New().Handler())
	t.Cleanup(srv.Close)
	t.Setenv(wire.EnvToken, "dgt_test")
	var opened string
	openURL = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { openURL = defaultOpenURL })
	if _, errb, code := office(t, "-server", srv.URL); code != 0 || !strings.HasPrefix(opened, srv.URL+"/office#t=") {
		t.Fatalf("exit %d, stderr %q, opened %q", code, errb, opened)
	}
}

// TestOfficeOldServer: a serve that predates the view says so.
func TestOfficeOldServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	t.Setenv(wire.EnvToken, "dgt_test")
	_, errb, code := office(t, "-server", srv.URL, "-no-open")
	if code != 1 || !strings.Contains(errb, "older than the office view") {
		t.Fatalf("exit %d, stderr %q; want 1 and the old-server message", code, errb)
	}
}

// TestOfficeNoServer: with no -server, no $DUGOUT_SERVER and no daemon to
// ask, it says what is missing.
func TestOfficeNoServer(t *testing.T) {
	t.Setenv(envServer, "")
	t.Setenv(statedir.EnvHome, t.TempDir())
	_, errb, code := office(t, "-no-open")
	if code != 2 || !strings.Contains(errb, "missing -server") {
		t.Fatalf("exit %d, stderr %q; want 2 and missing -server", code, errb)
	}
}

// TestOfficePixelURL: -pixel opens the pixel office with the same view token.
func TestOfficePixelURL(t *testing.T) {
	srv := httptest.NewServer(serve.New().Handler())
	t.Cleanup(srv.Close)
	t.Setenv(wire.EnvToken, "dgt_test")
	out, errb, code := office(t, "-server", srv.URL, "-no-open", "-pixel")
	if code != 0 || !strings.Contains(out, srv.URL+"/office/pixel#t="+wire.ViewTokenPrefix) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the pixel office's URL with a view token", code, out, errb)
	}
}

// TestOfficePixelOldServer: a serve with the office view but no pixel office
// says so instead of opening a page that is not there.
func TestOfficePixelOldServer(t *testing.T) {
	real := serve.New().Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/office/pixel" {
			http.NotFound(w, r)
			return
		}
		real.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(wire.EnvToken, "dgt_test")
	out, errb, code := office(t, "-server", srv.URL, "-no-open", "-pixel")
	if code != 1 || !strings.Contains(errb, "older than the pixel office") || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 and the old-server message", code, out, errb)
	}
}
