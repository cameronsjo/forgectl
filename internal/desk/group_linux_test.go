//go:build linux

package desk

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// groupHasLiveMember reports whether process group pgid still holds a process
// that has not exited. A zombie has exited: only its reaping is left, and
// that is the job of whichever process inherited it, not of the code under
// test. In a container whose pid 1 reaps slowly (or never), an orphan zombie
// keeps kill(-pgid, 0) succeeding for seconds after the group was killed, so
// signal 0 alone cannot tell a survivor from a corpse.
func groupHasLiveMember(pgid int) bool {
	if err := unix.Kill(-pgid, 0); errors.Is(err, unix.ESRCH) {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true // nothing more to learn: signal 0 found the group
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue // exited since the listing
		}
		// Fields after the command name, which sits in parentheses and may
		// itself hold spaces or parentheses: state, ppid, pgrp, ...
		i := bytes.LastIndexByte(data, ')')
		if i < 0 || i+2 > len(data) {
			continue
		}
		fields := strings.Fields(string(data[i+2:]))
		if len(fields) < 3 || fields[2] != strconv.Itoa(pgid) {
			continue
		}
		if fields[0] != "Z" && fields[0] != "X" {
			return true
		}
	}
	return false
}
