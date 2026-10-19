//go:build darwin

package goalnative

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const sandboxExecPath = "/usr/bin/sandbox-exec"

func processContainmentProfile() string {
	return fmt.Sprintf(
		"(version 1) (allow default) (deny syscall-unix (syscall-number %d) (syscall-number %d)) (deny syscall-unix (syscall-number %d) (with errno %d))",
		unix.SYS_SETSID,      //nolint:staticcheck // Darwin sandbox policy requires syscall numbers.
		unix.SYS_SETPGID,     //nolint:staticcheck // Darwin sandbox policy requires syscall numbers.
		unix.SYS_POSIX_SPAWN, //nolint:staticcheck // Darwin sandbox policy requires syscall numbers.
		unix.ENOSYS,
	)
}

func confinedCommand(command []string) ([]string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("confined command requires an executable")
	}
	for _, argument := range command {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("confined command contains a NUL byte")
		}
	}
	info, err := os.Stat(sandboxExecPath)
	if err != nil {
		return nil, fmt.Errorf("strict process containment requires %s: %w", sandboxExecPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("strict process containment executable %s is not a regular file", sandboxExecPath)
	}
	wrapped := make([]string, 0, len(command)+3)
	wrapped = append(wrapped, sandboxExecPath, "-p", processContainmentProfile())
	wrapped = append(wrapped, command...)
	return wrapped, nil
}
