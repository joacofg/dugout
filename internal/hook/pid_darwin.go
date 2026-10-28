package hook

import "golang.org/x/sys/unix"

// SysctlLookup reads a process's parent and executable name with one
// sysctl(kern.proc.pid), without forking ps (which costs about 20 ms).
func SysctlLookup(pid int) (int, string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, "", err
	}
	comm := kp.Proc.P_comm[:]
	for i, b := range comm {
		if b == 0 {
			comm = comm[:i]
			break
		}
	}
	return int(kp.Eproc.Ppid), string(comm), nil
}
