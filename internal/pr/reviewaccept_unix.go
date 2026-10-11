//go:build unix

package pr

import (
	"errors"
	"os"
	osexec "os/exec"
	"syscall"
)

// setProbeProcessGroup starts the probe in a process group of its own and
// makes its deadline kill the whole group, so a helper claude forked is not
// left running, or holding the output pipe, after a timeout. It mirrors
// internal/exec's setProcessGroup.
func setProbeProcessGroup(cmd *osexec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
