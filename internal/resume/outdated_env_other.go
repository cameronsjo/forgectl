//go:build !darwin

package resume

import (
	"os"
	"strconv"
	"strings"
)

// readProcessEnv reads /proc/<pid>/environ, which holds only the environment
// (no argv) as NUL-terminated KEY=VALUE entries. The kernel refuses another
// user's process.
func readProcessEnv(pid int) ([]string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ") // #nosec G304 -- fixed /proc path, pid is an int
	if err != nil {
		return nil, err
	}
	return splitNUL(data), nil
}

// splitNUL splits NUL-terminated strings, dropping empty entries.
func splitNUL(b []byte) []string {
	var out []string
	for _, f := range strings.Split(string(b), "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
