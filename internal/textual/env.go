package textual

import (
	"os"
	"os/exec"
	"strings"
	"time"
)

// redirects are the variables that change which repo, config or attributes
// git reads. The GIT_CONFIG_KEY_ and GIT_CONFIG_VALUE_ families are matched by
// prefix.
var redirects = map[string]bool{
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NAMESPACE":                    true,
	"GIT_DIFF_OPTS":                    true,
	"GIT_EXTERNAL_DIFF":                true,
	"GIT_ATTR_SOURCE":                  true,
	"GIT_ATTR_NOSYSTEM":                true,
}

// Redirects reports whether the environment variable name makes git read or
// write another repo, config or attributes than the one it is pointed at.
// Callers that run git on a repo of their own scrub these, whatever else
// they keep.
func Redirects(name string) bool {
	return redirects[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// isolated are the user's config switches: the daemon's own git ignores them.
var isolated = map[string]bool{"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_NOSYSTEM": true}

// pins turn off the user's global and system config and attributes. The
// global config being off does not stop git from reading the default
// core.attributesFile (under XDG_CONFIG_HOME or HOME), so it is pinned to
// nothing through the environment's own config.
var pins = []string{
	"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1",
	"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.attributesFile", "GIT_CONFIG_VALUE_0=" + os.DevNull,
}

// GitEnv is the environment for git on a repo the daemon owns: the process
// environment and extra, both without anything that could inject config or
// attributes or redirect git, then the pins.
func GitEnv(extra ...string) []string {
	var env []string
	for _, kv := range append(os.Environ(), extra...) {
		name, _, _ := strings.Cut(kv, "=")
		if isolated[name] || Redirects(name) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, pins...)
}

// waitDelay is how long a canceled git has to exit on SIGINT, and whatever
// it started to let go of its output, before it is killed.
const waitDelay = 5 * time.Second

// Interruptible makes a canceled cmd get SIGINT, on which git drops its lock
// files, instead of SIGKILL, which leaves them behind. cmd is killed if it
// is still running waitDelay later.
func Interruptible(cmd *exec.Cmd) *exec.Cmd {
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = waitDelay
	return cmd
}
