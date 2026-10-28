package daemoncore_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbidden are the packages the pure outbox must never reach, directly or
// transitively: network, filesystem, wall clock and randomness live at the
// edges. Note that fmt and encoding/hex pull in os.
var forbidden = []string{"net", "os", "time", "math/rand"}

func TestDaemoncoreImportGuard(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/joacofg/dugout/internal/daemoncore").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if dep == f || strings.HasPrefix(dep, f+"/") {
				t.Errorf("internal/daemoncore depends on %s (forbidden: %s)", dep, f)
			}
		}
	}
}
