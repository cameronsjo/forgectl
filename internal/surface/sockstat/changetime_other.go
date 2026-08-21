// Every unix that is neither Darwin nor Linux. The BSDs spell this field
// differently again, and none of them is a platform this project builds or tests
// on, so rather than guess at a selector that would break their build the
// timestamp is simply not read there.
//
// Returning zero is the documented contract, not a degradation: IncarnationInput
// says ChangedAtUnixNano is "the socket's creation or change timestamp where the
// platform reports one, and zero where it does not". A backend on such a
// platform keeps exactly the witnesses it had before this file existed — the
// device and inode — so nothing regresses; it only fails to gain the second one.
//
// The `unix` tag matters: without it this would also compile on Windows, where
// syscall.Stat_t does not exist, and the failure would surface as an undefined
// symbol in a file whose whole job is to prevent that.
//go:build unix && !darwin && !linux

package sockstat

import "syscall"

func changeTime(*syscall.Stat_t) int64 { return 0 }
