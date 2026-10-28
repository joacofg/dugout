package serve

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/joacofg/dugout/internal/core"
	"github.com/joacofg/dugout/internal/wire"
)

// metricValue is the value of the sample line starting with series, or -1.
func metricValue(t *testing.T, body, series string) int {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(l, series+" "); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("%s: value %q: %v", series, v, err)
			}
			return n
		}
	}
	return -1
}

func scrape(t *testing.T, admin client) string {
	t.Helper()
	code, b := admin.do(http.MethodGet, "/v1/admin/metrics", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/admin/metrics: status %d %s", code, b)
	}
	return string(b)
}

var metricSeries = []string{
	"dugout_daemons_connected", "dugout_workstreams", "dugout_warnings", "dugout_conflicts",
	`dugout_messages_total{type="full_state"}`, `dugout_messages_total{type="conflicts"}`,
	`dugout_messages_total{type="other"}`,
	"dugout_bundles_relayed_total", "dugout_stale_epochs_total",
	`dugout_messages_refused_total{reason="too_old"}`, "dugout_paused", "dugout_persons",
}

// TestAdminMetrics: the admin token opens the metrics, nobody else does, the
// body is Prometheus text with every metric, and the values move with what
// the daemons send.
func TestAdminMetrics(t *testing.T) {
	s, srv, _ := stateServer(t, "r")
	for _, tok := range []string{"", anaTok, "dgt_nope"} {
		if code, _ := as(t, srv, tok).do(http.MethodGet, "/v1/admin/metrics", "", nil); code != http.StatusUnauthorized {
			t.Errorf("metrics with %q: status %d, want 401", tok, code)
		}
	}
	admin := as(t, srv, adminTok)
	code, _ := admin.do(http.MethodGet, "/v1/admin/metrics", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	before := scrape(t, admin)
	for _, s := range metricSeries {
		if metricValue(t, before, s) < 0 {
			t.Errorf("no sample %s in:\n%s", s, before)
		}
		name, _, _ := strings.Cut(s, "{")
		if !strings.Contains(before, "# HELP "+name+" ") || !strings.Contains(before, "# TYPE "+name+" ") {
			t.Errorf("no HELP/TYPE of %s", name)
		}
	}
	if strings.Count(before, "# TYPE dugout_messages_total ") != 1 || strings.Count(before, "# TYPE dugout_messages_refused_total ") != 1 ||
		!strings.Contains(before, "# HELP dugout_messages_total Messages received") || !strings.Contains(before, "restarts") {
		t.Errorf("one TYPE per metric, and counters say they reset on restart:\n%s", before)
	}
	if got := metricValue(t, before, "dugout_persons"); got != 2 {
		t.Errorf("persons = %d, want 2 (carla is revoked)", got)
	}

	ana, beto := as(t, srv, anaTok), as(t, srv, betoTok)
	if code, _ := ana.stream("d1"); code != http.StatusOK {
		t.Fatalf("stream: status %d", code)
	}
	if c := ana.fullState("d1", 5, 1, w("d1:ws", "x.go")); c != http.StatusNoContent {
		t.Fatalf("ana's state: %d", c)
	}
	if c := beto.fullState("d2", 0, 0, w("d2:ws", "x.go")); c != http.StatusNoContent {
		t.Fatalf("beto's state: %d", c)
	}
	ana.fullState("d1", 3, 1, w("d1:ws", "x.go")) // an earlier run: stale
	if c := ana.conflicts("d1"); c != http.StatusNoContent {
		t.Fatalf("conflicts: %d", c)
	}
	if c := ana.upload("d1:ws", "d1", []byte("bundle")); c != http.StatusNoContent {
		t.Fatalf("upload: %d", c)
	}
	if c, _ := beto.download("d1:ws", "d2"); c != http.StatusOK {
		t.Fatalf("download: %d", c)
	}
	// A rating from an older daemon is a type serve no longer knows.
	ana.do(http.MethodPost, "/v1/msg", "application/json", []byte(legacyRating))
	ana.msg(wire.Message{Type: "delivered", FullState: core.FullState{Daemon: "d1"}})
	// A daemon under the minimum protocol is refused with 426, and counted
	// apart: it is not a message taken.
	s.minProtocol = 3
	if code := ana.msg(wire.Message{Type: wire.TypeFullState, ProtocolVersion: 2, FullState: core.FullState{Daemon: "d1"}}); code != http.StatusUpgradeRequired {
		t.Fatalf("a protocol-2 message under a minimum of 3: %d, want 426", code)
	}
	if code, _ := admin.do(http.MethodPost, "/v1/admin/pause", "application/json", []byte(`{"paused":true}`)); code != http.StatusNoContent {
		t.Fatalf("pause: %d", code)
	}

	after := scrape(t, admin)
	for series, want := range map[string]int{
		"dugout_daemons_connected":                        1,
		"dugout_workstreams":                              2,
		"dugout_warnings":                                 1,
		"dugout_conflicts":                                0,
		`dugout_messages_total{type="full_state"}`:        3,
		`dugout_messages_total{type="conflicts"}`:         1,
		`dugout_messages_total{type="other"}`:             2,
		"dugout_bundles_relayed_total":                    1,
		"dugout_stale_epochs_total":                       1,
		`dugout_messages_refused_total{reason="too_old"}`: 1,
		"dugout_paused":                                   1,
		"dugout_persons":                                  2,
	} {
		if got := metricValue(t, after, series); got != want {
			t.Errorf("%s = %d, want %d", series, got, want)
		}
	}
}

// TestAdminMetricsNoSecrets: whatever the daemons sent, no token and no code
// reaches the body.
func TestAdminMetricsNoSecrets(t *testing.T) {
	_, srv, _ := stateServer(t, "r")
	ana := as(t, srv, anaTok)
	ana.fullState("d1", 0, 0, w("d1:ws", "secret_path.go"))
	ana.upload("d1:ws", "d1", []byte("secret() code"))
	body := scrape(t, as(t, srv, adminTok))
	for _, bad := range []string{"dgt_", adminTok, anaTok, "secret", "x.go", "d1"} {
		if strings.Contains(body, bad) {
			t.Errorf("the metrics contain %q:\n%s", bad, body)
		}
	}
}

// TestAdminMetricsNeedsState: without a state file there is no admin API.
func TestAdminMetricsNeedsState(t *testing.T) {
	srv := newServer(t)
	if code, _ := as(t, srv, "").do(http.MethodGet, "/v1/admin/metrics", "", nil); code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", code)
	}
}
