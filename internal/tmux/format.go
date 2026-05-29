package tmux

import (
	"strconv"
	"strings"
	"time"
)

// fieldSep is the ASCII unit separator (0x1f). tmux -F formats join fields
// with it so session/window/pane names containing spaces (or even tabs) never
// break strings.Split — a single printable separator that no name will hold.
const fieldSep = "\x1f"

// splitLines splits command output into non-empty trimmed lines.
func splitLines(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// splitFields splits one -F output line into its fields on the unit separator.
func splitFields(line string) []string {
	return strings.Split(line, fieldSep)
}

// atoi parses an int, defaulting to 0 on garbage (tmux always emits valid
// integers for the count fields, but we never want a parse error to drop a row).
func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// parseUnix turns a tmux unix-timestamp field into a time.Time (zero on garbage).
func parseUnix(s string) time.Time {
	sec, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
