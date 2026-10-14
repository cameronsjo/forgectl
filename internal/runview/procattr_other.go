// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix && !linux

package runview

import "syscall"

// streamProcAttr starts a --follow-cmd command in a session of its own, with
// no controlling terminal.
func streamProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
