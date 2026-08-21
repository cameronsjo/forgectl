package sockstat

import "syscall"

// statWithChangeTime builds a Darwin stat carrying an exact change time, so a
// test can vary that field alone. It lives beside changeTime_darwin.go for the
// same reason that file exists: the selector differs by platform, and a test
// fixture that hardcoded one would fail to compile on the other.
func statWithChangeTime(inode uint64, nsec int64) *syscall.Stat_t {
	return &syscall.Stat_t{
		Ino:       inode,
		Uid:       501,
		Ctimespec: syscall.Timespec{Sec: 0, Nsec: nsec},
	}
}
