//go:build darwin

package desk

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// szomb is p_stat for a zombie (sys/proc.h).
const szomb = 5

// procStart returns pid's start time in microseconds since the epoch, and
// whether it is a zombie, from sysctl kern.proc.pid.
func procStart(pid int) (start int64, zombie bool, err error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false, err
	}
	if int(kp.Proc.P_pid) != pid {
		return 0, false, errNoProcess // the kernel answers a missing pid with a zeroed record
	}
	tv := kp.Proc.P_starttime
	return tv.Sec*1_000_000 + int64(tv.Usec), kp.Proc.P_stat == szomb, nil
}

// fileBirth is the file's creation time, when the platform records one.
func fileBirth(f *os.File) (time.Time, bool) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return time.Time{}, false
	}
	return time.Unix(st.Btim.Unix()).UTC(), true
}
