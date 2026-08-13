package tmux

import (
	"strconv"
	"strings"
	"time"
)

// FieldSep is the ASCII unit separator (0x1f). tmux -F formats join fields
// with it so session/window/pane names containing spaces (or even tabs) never
// break the split. It is NOT a separator a name cannot hold — `tmux rename-window
// $'a\x1fb'` is accepted — so the parsers below defend with an exact field
// count rather than trusting the separator to be unique to the format.
//
// Exported because internal/pr splits dispatch identities produced by this
// package's formats; a re-hardcoded "\x1f" there could drift from the format
// that produced the value, and the failure mode is silent (VerifyDispatched
// matches nothing and reports every healthy review gone).
const FieldSep = "\x1f"

// escapedFieldSep is how tmux 3.5a and older RENDER FieldSep back to a
// non-attached client: they run command output through strnvis(3) with
// VIS_OCTAL, so the 0x1f byte we asked for arrives as the four printable
// characters \, 0, 3, 7. tmux 3.7b emits the raw byte instead. Measured
// directly — same command, same isolated socket, stdout a pipe in every case:
//
//	tmux next-3.4 (alpine 3.19)  ->  "16\037@0"   (escaped)
//	tmux 3.5a     (alpine 3.21)  ->  "16\037@0"   (escaped)
//	tmux 3.7b     (alpine edge)  ->  "16<0x1f>@0" (raw)
//
// This is a tmux-version fact, not a platform one — the CI runner just
// happens to ship 3.4 while a Homebrew Mac ships 3.7b. Both renderings are
// live in the wild well above the 2.2 floor CheckGenerationCapability
// enforces, and that floor gates on the FIELDS EXISTING, not on how they come
// back, so every parser here must read both.
const escapedFieldSep = `\037`

// SplitFields splits one -F output line into its fields, accepting either
// rendering of FieldSep (see escapedFieldSep). Exported for internal/pr, which
// parses dispatch identities produced by this package's formats.
//
// Raw wins when present, and the two renderings can never be confused: a tmux
// that escapes never emits a bare 0x1f — it escapes every control byte, and
// the backslash itself — so a line carrying the raw separator can only have
// come from a non-escaping tmux, where a literal `\037` inside a name is just
// four ordinary characters.
//
// Splitting can only ever ADD fields, never remove them, which is what lets
// the exact field-count checks at the call sites stand as the real defense: a
// name carrying either rendering of the separator pushes its row past the
// expected count and the row is dropped. It can never be shifted into
// impersonating a shorter, differently-aligned row.
func SplitFields(line string) []string {
	if strings.Contains(line, FieldSep) {
		return strings.Split(line, FieldSep)
	}
	return strings.Split(line, escapedFieldSep)
}

// splitLines splits command output into non-empty trimmed lines.
func splitLines(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// splitFields is the package-internal spelling of SplitFields.
func splitFields(line string) []string {
	return SplitFields(line)
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
