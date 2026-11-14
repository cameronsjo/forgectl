// Package progstatus reports a forgectl command's progress to the terminal
// over the Program Status Protocol (OSC 7501, spec rev 0.4), so cmux's sidebar
// and herdr's status line show it the way they show an agent.
//
// Writing is blind: the spec says a terminal MUST ignore an unknown OSC, so no
// support probe is sent. A Reporter is a no-op unless stderr is a terminal,
// TERM is not dumb, the operator has not opted out, and the process is not
// inside a forgectl worker pane (forgectl reads OSC 7501 from those panes to
// decide readiness, so it must never write it there).
package progstatus

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// EnvOptOut turns reporting off when set to any non-empty value.
const EnvOptOut = "FORGECTL_NO_PROGRAM_STATUS"

const (
	// programID is both the record id and the app name. A report with no id is
	// the terminal's root record, and a clear with no id would wipe every
	// program's records on that terminal.
	programID = "forgectl"
	// maxMsgRunes keeps msg far under the spec's 2048-byte decoded cap.
	maxMsgRunes = 200
	// maxSeqBytes is the spec's whole-sequence cap; a longer report is
	// discarded whole by the terminal, so it is not sent.
	maxSeqBytes = 4096
)

// isTerminal is the test seam for the terminal gate, the same pattern as
// deskHasTerminal in internal/cli.
var isTerminal = func(fd uintptr) bool { return term.IsTerminal(int(fd)) }

// Reporter writes OSC 7501 reports. The zero value and a nil *Reporter are
// disabled no-ops.
type Reporter struct {
	w   io.Writer
	tmx bool
}

// New builds a Reporter over f (callers pass os.Stderr, so piped stdout and
// --json output stay clean). It is disabled unless every gate holds.
func New(f *os.File, getenv func(string) string) *Reporter {
	if f == nil || !isTerminal(f.Fd()) {
		return &Reporter{}
	}
	if getenv("TERM") == "dumb" || getenv(EnvOptOut) != "" {
		return &Reporter{}
	}
	if getenv(mail.EnvWorker) != "" || getenv(mail.EnvDrainWorker) != "" {
		return &Reporter{}
	}
	return &Reporter{w: f, tmx: getenv("TMUX") != ""}
}

// ForWriter builds a Reporter that writes to w and skips every gate. It is for
// tests and for a caller that has already decided to report; commands use New.
func ForWriter(w io.Writer, tmux bool) *Reporter { return &Reporter{w: w, tmx: tmux} }

// Enabled reports whether this Reporter will write.
func (r *Reporter) Enabled() bool { return r != nil && r.w != nil }

// Working reports state=working. progress < 0 omits the progress key.
func (r *Reporter) Working(msg string, progress int) {
	extra := ""
	if progress >= 0 {
		extra = fmt.Sprintf(":progress=%d", min(progress, 100))
	}
	r.send("working", msg, extra)
}

// Done reports state=done.
func (r *Reporter) Done(msg string) { r.send("done", msg, "") }

// Error reports state=error.
func (r *Reporter) Error(msg string) { r.send("error", msg, "") }

// Blocked reports state=blocked: the program waits on something other than
// itself.
func (r *Reporter) Blocked(msg string) { r.send("blocked", msg, "") }

// BlockedAuth reports state=blocked with kind=auth: the program waits on a
// credential or presence ceremony (Touch ID, sudo).
func (r *Reporter) BlockedAuth(msg string) { r.send("blocked", msg, ":kind=auth") }

// Clear removes forgectl's own record, and only that one.
func (r *Reporter) Clear() { r.send("clear", "", "") }

func (r *Reporter) send(state, msg, extra string) {
	if !r.Enabled() {
		return
	}
	var b strings.Builder
	b.WriteString("state=" + state)
	b.WriteString(":id=" + programID + ":app=" + programID)
	b.WriteString(extra)
	if msg = cleanMsg(msg); msg != "" {
		b.WriteString(":msg=" + base64.StdEncoding.EncodeToString([]byte(msg)))
	}
	// ESC from the ansi package, not a literal: the repo bans escape literals
	// outside internal/theme and internal/termsafe (theme_literals_test.go).
	esc := string(rune(ansi.ESC))
	seq := esc + "]7501;" + b.String() + esc + "\\"
	if len(seq) > maxSeqBytes {
		return
	}
	if r.tmx {
		seq = ansi.TmuxPassthrough(seq)
	}
	_, _ = io.WriteString(r.w, seq)
}

// cleanMsg caps the text and strips every control rune, so the spec's
// discard-the-whole-report rule cannot eat the report.
func cleanMsg(msg string) string {
	msg = termsafe.SafeLineMax(msg, maxMsgRunes)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, msg)
}
