package hook

import "strings"

const maxAncestors = 32

// FindClaudePID returns the first process from start upwards whose
// executable name contains "claude", or 0. Since Claude Code 2.1.303 it is
// the hook's direct parent, so start is getppid() and the walk usually
// stops at the first lookup.
func FindClaudePID(start int, lookup func(pid int) (ppid int, comm string, err error)) int {
	pid := start
	for range maxAncestors {
		if pid <= 1 {
			return 0
		}
		ppid, comm, err := lookup(pid)
		if err != nil {
			return 0
		}
		if strings.Contains(strings.ToLower(comm), "claude") {
			return pid
		}
		pid = ppid
	}
	return 0
}
