// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import "syscall"

// streamProcAttr starts a --follow-cmd command in a session of its own, with
// no controlling terminal; on Linux the kernel also kills it if forgectl
// dies without stopping it.
func streamProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}
}
