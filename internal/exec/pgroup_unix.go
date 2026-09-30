//go:build unix

package exec

import (
	"errors"
	"os"
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
		// ESRCH means the group already exited (the deadline raced a normal
		// exit); report it the way os/exec expects, so the run is not
		// recorded as a cancellation it never was.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
