package hook

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// ProcLookup reads a process's parent and executable name on Linux from
// /proc/<pid>/stat, one small read like the sysctl on a Mac. The name there
// is the same as /proc/<pid>/comm (the executable's, cut to 15 bytes), which
// is what P_comm holds on a Mac.
func ProcLookup(pid int) (int, string, error) { return procLookup("/proc", pid) }

// procLookup is ProcLookup on a /proc mounted at root, so a test can fake it.
func procLookup(root string, pid int) (int, string, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", err
	}
	return parseStat(b)
}

var errStat = errors.New("/proc stat unreadable")

// parseStat reads "pid (comm) state ppid ...". comm may hold spaces and
// parentheses, so it ends at the last ')'.
func parseStat(b []byte) (int, string, error) {
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return 0, "", errStat
	}
	fields := bytes.Fields(b[end+1:])
	if len(fields) < 2 {
		return 0, "", errStat
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0, "", errStat
	}
	return ppid, string(b[open+1 : end]), nil
}
