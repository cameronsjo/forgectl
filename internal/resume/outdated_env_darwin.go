package resume

import "golang.org/x/sys/unix"

// readProcessEnv reads a process's environment through the kern.procargs2
// sysctl — the same source ps(1) uses, without spawning ps or capturing its
// output. The kernel refuses another user's process.
func readProcessEnv(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	_, env, err := parseProcArgs2(buf)
	return env, err
}
