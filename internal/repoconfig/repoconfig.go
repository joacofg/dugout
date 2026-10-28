// Package repoconfig reads a repo's optional .dugout.yml and decides which
// in-flight files are too noisy to warn about.
package repoconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// FileName is the per-repo config file, at the worktree root.
const FileName = ".dugout.yml"

// Config is the content of .dugout.yml. Every field is optional.
type Config struct {
	// RepoID overrides the repo identity derived from origin (forks).
	RepoID string `yaml:"repo_id"`
	// TargetBranch overrides origin/HEAD as the branch in-flight diffs are
	// measured against, e.g. "origin/develop".
	TargetBranch string `yaml:"target_branch"`
	Ignore       struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"ignore"`
	// EscalateAfter is how long a conflict may stand after both agents were
	// warned before Dugout takes it to both humans: a duration such as "15m",
	// or "off" for never. Unset, DefaultEscalateAfter.
	EscalateAfter string `yaml:"escalate_after"`
}

// DefaultEscalateAfter is escalate_after when the repo sets none: long enough
// for two agents to settle a conflict on the channel, short enough that their
// humans hear of one they could not before the work moves on.
const DefaultEscalateAfter = 10 * time.Minute

// EscalationDelay is c's escalate_after: 0 for "off", DefaultEscalateAfter
// when unset, and DefaultEscalateAfter with an error when it is not a
// positive duration.
func (c Config) EscalationDelay() (time.Duration, error) {
	v := strings.TrimSpace(c.EscalateAfter)
	switch strings.ToLower(v) {
	case "":
		return DefaultEscalateAfter, nil
	case "off":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return DefaultEscalateAfter, fmt.Errorf("%s: escalate_after %q is not a positive duration such as 15m, nor off: using %s", FileName, v, DefaultEscalateAfter)
	}
	return d, nil
}

// Load reads <root>/.dugout.yml. A missing file is an empty Config.
func Load(root string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", FileName, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	return cfg, nil
}

// DefaultIgnore are files that change in almost every branch, so sharing
// them says nothing about a real conflict.
var DefaultIgnore = []string{
	"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb",
	"go.sum", "Cargo.lock", "Gemfile.lock", "poetry.lock", "uv.lock", "composer.lock",
	"CHANGELOG*", "*.snap", "generated/", "dist/",
}

// Matcher decides whether a repo-relative path is ignored. A pattern
// ending in "/" matches a directory of that name at any depth; a pattern
// without "/" matches the base name; any other pattern matches the whole
// path. Matching uses path.Match, so "*" never crosses "/" and there is
// no "**".
type Matcher struct {
	patterns []string
}

// NewMatcher builds the default list minus cfg's removals plus its additions.
func NewMatcher(cfg Config) Matcher {
	var ps []string
	for _, p := range DefaultIgnore {
		if !slices.Contains(cfg.Ignore.Remove, p) {
			ps = append(ps, p)
		}
	}
	return Matcher{patterns: append(ps, cfg.Ignore.Add...)}
}

// Ignored reports whether rel matches any pattern.
func (m Matcher) Ignored(rel string) bool {
	segments := strings.Split(rel, "/")
	base := segments[len(segments)-1]
	for _, p := range m.patterns {
		switch {
		case strings.HasSuffix(p, "/"):
			if slices.Contains(segments[:len(segments)-1], strings.TrimSuffix(p, "/")) {
				return true
			}
		case !strings.Contains(p, "/"):
			if ok, _ := path.Match(p, base); ok {
				return true
			}
		default:
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
		}
	}
	return false
}

// Filter returns the files that are not ignored, in their original order.
func (m Matcher) Filter(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if !m.Ignored(f) {
			out = append(out, f)
		}
	}
	return out
}
