//go:build !darwin

package hook

import "errors"

// SysctlLookup is macOS's; Linux reads /proc instead (ProcLookup), and
// elsewhere the PID is unknown.
func SysctlLookup(int) (int, string, error) {
	return 0, "", errors.New("process lookup not supported on this platform")
}
