//go:build linux

package desk

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// procStart returns pid's start time in clock ticks since boot (field 22 of
// /proc/<pid>/stat), and whether it is a zombie.
func procStart(pid int) (start int64, zombie bool, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false, errNoProcess
	}
	// The command name sits in parentheses and may itself hold spaces or
	// parentheses, so fields are counted from the LAST ')'.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 > len(data) {
		return 0, false, fmt.Errorf("desk: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[i+2:]))
	const stateField, startField = 0, 19 // fields 3 and 22, counted after the name
	if len(fields) <= startField {
		return 0, false, fmt.Errorf("desk: short /proc/%d/stat", pid)
	}
	start, err = strconv.ParseInt(fields[startField], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("desk: /proc/%d/stat starttime: %w", pid, err)
	}
	state := fields[stateField]
	return start, state == "Z" || state == "X", nil
}

// fileBirth is the file's creation time, when the filesystem records one.
func fileBirth(f *os.File) (time.Time, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx); err != nil {
		return time.Time{}, false
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)).UTC(), true
}
