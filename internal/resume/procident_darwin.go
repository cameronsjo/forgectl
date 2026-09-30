package resume

import (
	"time"

	"golang.org/x/sys/unix"
)

// readProcessIdentity reads a process's exec path from the kern.procargs2
// sysctl and its start time from kern.proc.pid — both in-process, so nothing
// about the process passes through a subprocess's output. The exec path is the
// path handed to execve (a symlink stays a symlink), which is why IsClaudeExec
// accepts both the link name and the versions/<X> target. The kernel refuses
// another user's process.
func readProcessIdentity(pid int) (ProcIdentity, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return ProcIdentity{}, err
	}
	execPath, _, err := parseProcArgs2(buf)
	if err != nil {
		return ProcIdentity{}, err
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcIdentity{}, err
	}
	tv := kp.Proc.P_starttime
	return ProcIdentity{ExecPath: execPath, Start: time.Unix(tv.Sec, int64(tv.Usec)*1000).UTC()}, nil
}
