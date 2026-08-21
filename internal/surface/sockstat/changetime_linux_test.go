package sockstat

import "syscall"

// statWithChangeTime builds a Linux stat carrying an exact change time. See the
// Darwin sibling: same helper, different selector.
func statWithChangeTime(inode uint64, nsec int64) *syscall.Stat_t {
	return &syscall.Stat_t{
		Ino:  inode,
		Uid:  501,
		Ctim: syscall.Timespec{Sec: 0, Nsec: nsec},
	}
}
