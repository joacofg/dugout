package gitscan

import (
	"testing"

	"github.com/joacofg/dugout/internal/testgit"
)

func isolateGit(t *testing.T)                              { testgit.Isolate(t) }
func gitT(t *testing.T, dir string, args ...string) string { return testgit.Git(t, dir, args...) }
func writeFile(t *testing.T, path, body string)            { testgit.WriteFile(t, path, body) }

type fixture struct{ base, origin, seed, clone string }

func newFixture(t *testing.T) fixture {
	f := testgit.NewFixture(t)
	return fixture{base: f.Base, origin: f.Origin, seed: f.Seed, clone: f.Clone}
}
