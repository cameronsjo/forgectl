package sockstat

import "syscall"

// changeTime reads Darwin's inode change time.
//
// Ctimespec rather than Birthtimespec, even though a birth time is the more
// precise witness for "this socket was recreated": Birthtimespec exists only on
// Darwin, and a fingerprint field that is populated on one platform and zero on
// another is worse than one populated consistently — it would make the same
// server fingerprint differently depending on where forgectl runs, which is a
// difference the digest cannot distinguish from a restart.
//
// ctime moves on any metadata change, not only on creation, so it is noisier
// than a birth time. Noise is the safe direction here: a fingerprint that
// changes when nothing restarted refuses a close that would have succeeded,
// which costs a manual cleanup. The opposite — a fingerprint that survives a
// restart — is what lets a rollback destroy a stranger's workspace.
func changeTime(sys *syscall.Stat_t) int64 {
	return sys.Ctimespec.Sec*1e9 + sys.Ctimespec.Nsec
}
