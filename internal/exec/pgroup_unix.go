//go:build unix

package exec

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own group (pgid = its pid) and makes
// a context cancellation kill the group. Killing -pid reaches the child and
// every descendant still in the group.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
