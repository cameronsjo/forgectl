//go:build !unix

package pr

import osexec "os/exec"

// setProbeProcessGroup is a no-op off unix; WaitDelay still bounds the probe.
func setProbeProcessGroup(*osexec.Cmd) {}
