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

	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// EnvOptOut turns reporting off when set to any non-empty value.
const EnvOptOut = "FORGECTL_NO_PROGRAM_STATUS"

const (
	// programID is both the record id and the app name. A report with an id is
	// a child record; a report with no id is the terminal's root record, and a
	// clear with no id wipes every program's records on that terminal (measured
	// on hc 0.1.21: it also removed another program's id'd record).
	programID = meta.AppName
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
	// root sends reports with no id, as the terminal's root record. herdr-core
	// sets a terminal's status only from root reports (an id'd record is a
	// child record and leaves `status` empty), and herdr-app shows only that
	// status: a plain terminal with a root `blocked` is listed with a "Needs
	// you" badge, while the same report with an id shows nothing (measured on
	// hc 0.1.21 and herdr-app 0.1.15). A terminal's status clears by itself
	// when its interactive shell returns to the foreground, so the root record
	// does not outlive the command (measured on hc 0.1.21 with an interactive
	// shell: the status was set while a command ran and was gone when the
	// command returned to the prompt).
	//
	// Root mode makes forgectl own the terminal's whole status for as long as
	// it runs, and its Clear wipes every record on the terminal. That is right
	// where forgectl is the foreground program of a herdr-core terminal, and
	// wrong under tmux, where the terminal is shared by every pane: see New.
	root bool
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
	// A herdr-core or herdr-app terminal reports as the root record, by the
	// repo's own host detection (HC_TERMINAL_ID and none of the HERDR_*
	// variables: a herdr 0.9 pane keeps the id'd record, which that host was
	// not measured to need root for). Never under tmux, screen or zellij: a
	// tmux server copies its starting environment into every pane, so
	// HC_TERMINAL_ID can name an outer terminal that every pane shares, where
	// forgectl is not the foreground program (the outer shell never returns to
	// its prompt to clear the status) and a root clear would wipe other panes'
	// records. screen and zellij panes inherit the same variable (inferred,
	// not measured).
	tmux := getenv("TMUX") != ""
	// screen (STY) and zellij (ZELLIJ) panes inherit the same stale variable.
	multiplexed := tmux || getenv("STY") != "" || getenv("ZELLIJ") != ""
	root := !multiplexed && herdr.DetectHost(func(k string) (string, bool) { v := getenv(k); return v, v != "" }) == herdr.HostApp
	return &Reporter{w: f, tmx: tmux, root: root}
}

// ForWriter builds a Reporter that writes to w and skips every gate. It is for
// tests and for a caller that has already decided to report; commands use New.
func ForWriter(w io.Writer, tmux bool) *Reporter { return &Reporter{w: w, tmx: tmux} }

// ForWriterRoot is ForWriter for a host that needs root reports (see Reporter).
func ForWriterRoot(w io.Writer, tmux bool) *Reporter { return &Reporter{w: w, tmx: tmux, root: true} }

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

// Kind says what a blocked program waits for. The spec (rev 0.4) defines three
// and allows kind only with state=blocked; a host treats an unrecognized value
// as absent, so Blocked drops any other Kind rather than send it.
type Kind string

const (
	// KindNone sends no kind: blocked on something the three do not name.
	KindNone Kind = ""
	// KindPermission is an approval to do something (a pending desk item).
	KindPermission Kind = "permission"
	// KindQuestion is an answer the user must type.
	KindQuestion Kind = "question"
	// KindAuth is a login or credential: a presence ceremony (Touch ID, sudo).
	KindAuth Kind = "auth"
)

// valid reports whether k is one the spec defines.
func (k Kind) valid() bool {
	switch k {
	case KindPermission, KindQuestion, KindAuth:
		return true
	}
	return false
}

// Blocked reports state=blocked: the program waits on something other than
// itself. kind says on what; KindNone or an unknown Kind sends no kind key.
func (r *Reporter) Blocked(kind Kind, msg string) {
	extra := ""
	if kind.valid() {
		extra = ":kind=" + string(kind)
	}
	r.send("blocked", msg, extra)
}

// Clear removes forgectl's own record, and only that one; for a root reporter
// it clears the terminal's root record, which forgectl owns while it runs.
func (r *Reporter) Clear() { r.send("clear", "", "") }

func (r *Reporter) send(state, msg, extra string) {
	if !r.Enabled() {
		return
	}
	var b strings.Builder
	b.WriteString("state=" + state)
	if !r.root {
		b.WriteString(":id=" + programID)
	}
	b.WriteString(":app=" + programID)
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
