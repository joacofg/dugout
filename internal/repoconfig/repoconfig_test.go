package repoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".dugout.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDefaultIgnore(t *testing.T) {
	m := NewMatcher(Config{})
	for _, p := range []string{
		"package-lock.json",
		"web/yarn.lock",
		"CHANGELOG.md",
		"ui/__snapshots__/a.snap",
		"pkg/generated/x.go",
		"dist/app.js",
	} {
		if !m.Ignored(p) {
			t.Errorf("%s should be ignored by default", p)
		}
	}
	for _, p := range []string{"main.go", "distro/x.go", "docs/changelog-notes.md"} {
		if m.Ignored(p) {
			t.Errorf("%s should not be ignored", p)
		}
	}
}

func TestConfigAddRemove(t *testing.T) {
	root := writeConfig(t, "ignore:\n  add: [\"docs/*.md\"]\n  remove: [\"*.snap\"]\n")
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMatcher(cfg)
	if !m.Ignored("docs/a.md") {
		t.Error("added glob docs/*.md should be ignored")
	}
	if m.Ignored("a.snap") {
		t.Error("removed default *.snap should warn again")
	}
	if !m.Ignored("go.sum") {
		t.Error("untouched defaults should still apply")
	}
}

func TestFilterKeepsOrder(t *testing.T) {
	got := NewMatcher(Config{}).Filter([]string{"b.go", "go.sum", "a.go"})
	if strings.Join(got, ",") != "b.go,a.go" {
		t.Fatalf("Filter = %v", got)
	}
}

func TestLoad(t *testing.T) {
	root := writeConfig(t, "repo_id: github.com/acme/app\ntarget_branch: origin/develop\n")
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepoID != "github.com/acme/app" || cfg.TargetBranch != "origin/develop" {
		t.Fatalf("Load = %+v", cfg)
	}

	cfg, err = Load(t.TempDir())
	if err != nil || cfg.RepoID != "" || cfg.TargetBranch != "" || len(cfg.Ignore.Add) != 0 {
		t.Fatalf("missing file: cfg %+v err %v", cfg, err)
	}

	_, err = Load(writeConfig(t, "ignore: [unclosed\n"))
	if err == nil || !strings.Contains(err.Error(), ".dugout.yml") {
		t.Fatalf("broken yaml: want error naming .dugout.yml, got %v", err)
	}
}

// TestEscalationDelay: escalate_after takes a positive duration or "off";
// unset is the default, and anything else is the default with an error that
// names the file.
func TestEscalationDelay(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultEscalateAfter, false},
		{"15m", 15 * time.Minute, false},
		{" 90s ", 90 * time.Second, false},
		{"off", 0, false},
		{"OFF", 0, false},
		{"0", DefaultEscalateAfter, true},
		{"-5m", DefaultEscalateAfter, true},
		{"ten minutes", DefaultEscalateAfter, true},
	}
	for _, tc := range cases {
		got, err := Config{EscalateAfter: tc.raw}.EscalationDelay()
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("escalate_after %q: %v, %v; want %v, error %v", tc.raw, got, err, tc.want, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), ".dugout.yml") {
			t.Errorf("escalate_after %q: error %q does not name .dugout.yml", tc.raw, err)
		}
	}
	cfg, err := Load(writeConfig(t, "escalate_after: 20m\n"))
	if err != nil || cfg.EscalateAfter != "20m" {
		t.Fatalf("Load: %+v, %v", cfg, err)
	}
}
