package tmux

import (
	"errors"
	"fmt"
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
// characters \, 0, 3, 7.
//
// The rendering varies by tmux VERSION *and* by LOCALE — it is not a platform
// fact, and it is not a version fact alone. Measured directly, same command,
// same isolated socket, stdout a pipe, format
// `#{window_id}<0x1f>#{window_name}<0x1f>END` in every cell:
//
//	tmux                 | LANG=C     | LANG=C.UTF-8 | LANG=en_US.UTF-8
//	---------------------+------------+--------------+-----------------
//	3.5a  (alpine 3.21)  | \037       | \037         | \037
//	3.7b  (alpine edge)  | _  (0x5f)  | raw 0x1f     | raw 0x1f
//
// (tmux next-3.4 on alpine 3.19 matches the 3.5a row.) Both the escaped and
// the raw rendering are live in the wild well above the 2.2 floor
// CheckGenerationCapability enforces, and that floor gates on the FIELDS
// EXISTING, not on how they come back, so every parser here must read both.
//
// SplitFields deliberately does NOT read the third cell. Under a non-UTF-8
// locale tmux 3.7b does not ESCAPE the byte, it SUBSTITUTES an underscore for
// it — lossily and irreversibly, the same substitution it applies to every
// other unprintable byte, so the original is unrecoverable from the output.
// And `_` is legal in a session name, a window name, and a pane title, so
// splitting on it to "recover" a locale misconfiguration would shred ordinary
// rows (`my_session`, `pr-o-r-1_pad`) into forged ones — a far worse failure
// than the one it repairs. There is no rendering-side fix; the defence is the
// zero-row contract at parsedRows below, which covers this rendering and
// whatever FOURTH one a future tmux or locale invents.
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

// ErrUnreadableFields reports that tmux produced output but not one line of it
// split into the expected number of fields — so the field separator did not
// survive the round trip and NOTHING in that output can be trusted.
//
// Exported so a caller can tell this apart from an ordinary tmux failure with
// errors.Is; the distinction matters because the remediation is the operator's
// locale, not the tmux install.
var ErrUnreadableFields = errors.New("tmux field separator did not survive the -F round trip")

// parsedRows enforces the fail-CLOSED contract every -F parser in this package
// shares: a non-empty tmux output that yields ZERO usable rows is an error, not
// an empty result.
//
// This is the general defence, and it is the load-bearing one. SplitFields
// knows two renderings of FieldSep (see escapedFieldSep) and there is at least
// a third it cannot read — tmux 3.7b under LANG=C substitutes `_` for the byte,
// so every row collapses to a single field and every exact-count check drops
// it. Without this contract that is silent, and it fails OPEN in the worst
// possible direction: WindowsLive returns ok=true with an all-false map,
// contradicting its own promise to report ok=false when the list could not be
// read; LiveReviews counts 0 and the concurrency cap grants a full batch on a
// machine already saturated; VerifyDispatched finds no live keys and reports
// every healthy dispatch gone. Turning total parse failure into a loud refusal
// converts that whole class — including renderings nobody has found yet — from
// a fail-open into a fail-closed.
//
// PARTIAL loss stays a silent drop, deliberately. A single row can legitimately
// fail its count because a name carries the separator (see parseWindows), and
// erroring on that would let anyone who can name a window take down `pr list`
// for the whole server. Total loss cannot be caused that way: every row failing
// at once means the separator itself is gone.
func parsedRows[T any](rows []T, lines []string, command string, want int) ([]T, error) {
	if len(rows) > 0 || len(lines) == 0 {
		return rows, nil
	}
	return nil, fmt.Errorf(
		"%w: %s returned %d line(s), none of which split into %d fields (first line %q); "+
			"tmux renders the separator lossily outside a UTF-8 locale — set LANG/LC_ALL to a UTF-8 locale and retry",
		ErrUnreadableFields, command, len(lines), want, lines[0])
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
