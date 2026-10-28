package textual

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joacofg/dugout/internal/testgit"
)

// TestRunInterruptedOnCancel: a canceled git gets SIGINT, on which git drops
// its lock files, not SIGKILL.
func TestRunInterruptedOnCancel(t *testing.T) {
	dir, ready, interrupted := testgit.InterruptibleGit(t)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				return
			}
		}
	}()
	if _, err := repo(t.TempDir()).run(ctx, nil, "status"); err == nil {
		t.Fatal("a canceled git returned no error")
	}
	if _, err := os.Stat(interrupted); err != nil {
		t.Fatalf("git did not get SIGINT: %v", err)
	}
}

// TestGitEnvScrubsAndPins: what redirects git or injects config or
// attributes is dropped, from the process environment and from extra alike,
// and the pins come out once each, so nothing can override them.
func TestGitEnvScrubsAndPins(t *testing.T) {
	for name, value := range map[string]string{
		"GIT_DIR":               "/elsewhere/.git",
		"GIT_OBJECT_DIRECTORY":  "/elsewhere/objects",
		"GIT_ATTR_SOURCE":       "HEAD",
		"GIT_ATTR_NOSYSTEM":     "0",
		"GIT_CONFIG_PARAMETERS": "'merge.evil.driver'='touch /tmp/x'",
		"GIT_CONFIG_COUNT":      "4",
		"GIT_CONFIG_KEY_3":      "core.attributesFile",
		"GIT_CONFIG_VALUE_3":    "/elsewhere/attributes",
		"DUGOUT_KEPT":           "yes",
	} {
		t.Setenv(name, value)
	}
	env := GitEnv("GIT_AUTHOR_NAME=dugout", "GIT_CONFIG_COUNT=2", "GIT_ATTR_SOURCE=HEAD", "GIT_CONFIG_KEY_1=x")

	seen := map[string][]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		seen[name] = append(seen[name], value)
	}
	for _, name := range []string{"GIT_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ATTR_SOURCE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_1", "GIT_CONFIG_KEY_3", "GIT_CONFIG_VALUE_3"} {
		if v, ok := seen[name]; ok {
			t.Errorf("%s = %q, want it scrubbed", name, v)
		}
	}
	for name, want := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "core.attributesFile",
		"GIT_CONFIG_VALUE_0":  os.DevNull,
		"GIT_AUTHOR_NAME":     "dugout",
		"DUGOUT_KEPT":         "yes",
	} {
		if got := seen[name]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want exactly [%q]", name, got, want)
		}
	}
}
