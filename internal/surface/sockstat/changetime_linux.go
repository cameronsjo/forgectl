package sockstat

import "syscall"

// changeTime reads Linux's inode change time.
//
// Same field, different spelling: Linux calls it Ctim where Darwin calls it
// Ctimespec, which is the whole reason this is a build-tagged pair rather than
// one line in Fill. Both are a syscall.Timespec, so the arithmetic is identical
// and only the selector differs.
func changeTime(sys *syscall.Stat_t) int64 {
	return sys.Ctim.Sec*1e9 + sys.Ctim.Nsec
}
