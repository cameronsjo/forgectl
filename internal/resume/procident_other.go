//go:build !darwin

package resume

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicksPerSecond is USER_HZ, the unit of /proc/<pid>/stat's starttime.
// debt: assumes 100 (every mainstream Linux build); read sysconf(_SC_CLK_TCK) if a target ever differs.
const clockTicksPerSecond = 100

// readProcessIdentity reads /proc/<pid>/exe (the resolved binary) and derives
// the start time from /proc/<pid>/stat's starttime plus /proc/stat's btime.
// /proc/<pid>/cmdline is deliberately not read: argv can carry a prompt or a
// token, and the exec path answers the question. Off Linux the files do not
// exist and the error makes the caller refuse, which is the safe reading.
func readProcessIdentity(pid int) (ProcIdentity, error) {
	base := "/proc/" + strconv.Itoa(pid)
	execPath, err := os.Readlink(base + "/exe")
	if err != nil {
		return ProcIdentity{}, err
	}
	stat, err := os.ReadFile(base + "/stat") // #nosec G304 -- fixed /proc path, pid is an int
	if err != nil {
		return ProcIdentity{}, err
	}
	ticks, err := statStartTicks(string(stat))
	if err != nil {
		return ProcIdentity{}, err
	}
	sys, err := os.ReadFile("/proc/stat")
	if err != nil {
		return ProcIdentity{}, err
	}
	btime, err := bootTime(string(sys))
	if err != nil {
		return ProcIdentity{}, err
	}
	start := btime.Add(time.Duration(ticks) * time.Second / clockTicksPerSecond)
	return ProcIdentity{ExecPath: execPath, Start: start.UTC()}, nil
}

// statStartTicks returns field 22 (starttime) of a /proc/<pid>/stat line. The
// comm field is parenthesised and may itself hold spaces or parentheses, so
// fields are counted from after the LAST ')'.
func statStartTicks(stat string) (int64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("proc stat: no comm field")
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state), so field 22 is fields[19].
	const startIdx = 22 - 3
	if len(fields) <= startIdx {
		return 0, fmt.Errorf("proc stat: %d fields after comm, want more than %d", len(fields), startIdx)
	}
	return strconv.ParseInt(fields[startIdx], 10, 64)
}

// bootTime returns the btime line of /proc/stat.
func bootTime(stat string) (time.Time, error) {
	for _, line := range strings.Split(stat, "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("proc stat btime: %w", err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("proc stat: no btime line")
}
