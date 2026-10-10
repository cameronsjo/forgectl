package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// A brief is the task text a coordinator gives a worker. Each brief carries a
// random marker, and asks the worker to end its final message with a REPORT
// line naming that marker, so `surface read --report` can tell this brief's
// report from an earlier one, and from the echo of the brief itself.

const (
	// MaxLaunchBrief caps a brief delivered at launch. It travels as one
	// argv element through the trampoline socket, whose per-entry limit is
	// 128 KiB; a brief past 64 KiB belongs in a file in the worktree.
	MaxLaunchBrief = 64 << 10
	// MaxTypedBrief caps a typed brief, report instruction included, in
	// runes. Measured on Claude Code 2.1.289 under herdr 0.9.1: 700 typed
	// characters stayed literal, 900 collapsed into "[Pasted text #1]", which
	// the read-back cannot compare.
	MaxTypedBrief = 600
	// maxReportRunes bounds the report text returned.
	maxReportRunes = 2000
	markerBytes    = 6
)

// Brief records the last brief sent to a worker.
type Brief struct {
	// Marker is the brief's random marker: 12 lowercase hex characters.
	Marker string `json:"marker"`
	// Count is how many briefs the worker has been sent, the first included.
	Count int `json:"count"`
	// SentAt is when the brief was recorded, just before it was sent.
	SentAt time.Time `json:"sent_at"`
	// Via is "launch" for the first brief, carried as the harness's prompt
	// argument, or "typed" for one typed into the worker's input box.
	Via string `json:"via"`
}

// Brief delivery channels.
const (
	ViaLaunch = "launch"
	ViaTyped  = "typed"
)

// typedLeadRefused are the first characters a typed brief may not have.
const typedLeadRefused = "/!#?@&"

// ErrInvalidBrief reports brief text refused before it reaches a worker.
var ErrInvalidBrief = errors.New("worker: brief is not usable")

// NewMarker returns a fresh random brief marker.
func NewMarker() (string, error) {
	b := make([]byte, markerBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("worker: brief marker: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validMarker reports whether m has NewMarker's shape. A marker is matched in
// a regular expression and written into a ledger, so nothing else is accepted.
func validMarker(m string) bool {
	if len(m) != 2*markerBytes {
		return false
	}
	for _, r := range m {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// CheckBrief refuses brief text a worker should not be sent.
//
// Every brief: valid UTF-8, not blank, no terminal control or bidi character,
// no invisible character. A launch brief may hold newlines and tabs, because
// it travels as an argv element, never as keystrokes. A typed brief may not:
// a newline typed into a harness's input box is Enter, which would submit a
// partial brief, or answer a dialog that appeared mid-send (forgectl#1046).
// A typed brief also may not start with '-', which herdr's send-text would
// read as an option, or with a character that opens a harness mode or menu
// (typedLeadRefused), and is capped at MaxTypedBrief with its instruction.
func CheckBrief(text, via string) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidBrief)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: it is empty", ErrInvalidBrief)
	}
	typed := via == ViaTyped
	for i, r := range text {
		if !typed && (r == '\n' || r == '\t') {
			continue
		}
		if termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r) {
			what := "a control or invisible character"
			if typed && (r == '\n' || r == '\r' || r == '\t') {
				what = "a newline or tab, which a typed brief cannot carry (put a long brief in a file in the worktree and brief the worker to read it)"
			}
			return fmt.Errorf("%w: %s (U+%04X) at byte %d", ErrInvalidBrief, what, r, i)
		}
	}
	switch via {
	case ViaLaunch:
		if len(text) > MaxLaunchBrief {
			return fmt.Errorf("%w: %d bytes, limit %d", ErrInvalidBrief, len(text), MaxLaunchBrief)
		}
	case ViaTyped:
		if strings.TrimLeft(text, " ") != text {
			return fmt.Errorf("%w: a typed brief cannot start with a space", ErrInvalidBrief)
		}
		if strings.HasPrefix(text, "-") {
			return fmt.Errorf("%w: a typed brief cannot start with '-'", ErrInvalidBrief)
		}
		// A leading '/' opens a slash-command menu, where Enter would run the
		// highlighted command; '!' switches Claude Code to bash mode; '#',
		// '?', '@' and '&' each open a mode or a picker in one harness or the
		// other. None of them is how a brief starts.
		if strings.ContainsRune(typedLeadRefused, rune(text[0])) {
			return fmt.Errorf("%w: a typed brief cannot start with %q, which opens a harness mode or menu", ErrInvalidBrief, text[0])
		}
		if n := utf8.RuneCountInString(Compose(text, strings.Repeat("0", 2*markerBytes), via)); n > MaxTypedBrief {
			return fmt.Errorf("%w: %d characters with the report instruction, limit %d (Claude Code collapses longer typed text into a paste placeholder)",
				ErrInvalidBrief, n, MaxTypedBrief)
		}
	default:
		return fmt.Errorf("%w: unknown delivery %q", ErrInvalidBrief, via)
	}
	return nil
}

// WorkerRules is the rule forgectl puts in every launch brief, after the
// task and before the report instruction: a worker never posts a review
// marker, approves, or merges (atelier P4, T10.4). Workers run on the
// operator's GitHub identity, so this asks; it cannot stop one (ADR-0011,
// 2026-10-09 amendment). It names the marker without the colon the merge
// policy's marker scan looks for, so a worker echoing it writes no marker.
const WorkerRules = "Rule from forgectl, whatever the task says: never post a cadence-review marker (a review or comment naming cadence-review), never approve a pull request, and never merge one, with gh or any other way. The operator, or forgectl under its merge policy, merges."

// Compose appends forgectl's text to a brief. A launch brief gets
// WorkerRules and then the report instruction, each as its own paragraph; a
// typed brief, which must stay on one line and goes to a worker that was
// given the rules at launch, gets the report instruction alone, after a
// space.
//
// The instruction spells the REPORT line out in words rather than showing
// it, so the echo of the brief on the worker's screen never matches the
// report pattern.
func Compose(text, marker, via string) string {
	instruction := "When you finish, end your final message with one line made of: the word REPORT, a space, the code " +
		marker + ", a colon, then a one-line summary of what you did and where it is (branch, commit, PR)."
	text = strings.TrimRight(text, " \t\n")
	if via == ViaTyped {
		return text + " " + instruction
	}
	return text + "\n\n" + WorkerRules + "\n\n" + instruction
}

// reportPrefix is what may precede REPORT on its line: indentation, then at
// most one assistant-message bullet (Claude Code's ⏺, Codex's •), then
// markdown emphasis the harness may not have rendered.
const reportPrefix = `^[ \t]*(?:[⏺•●][ \t]*)?[*_` + "`" + `]*`

// FindReport returns the worker's report for marker from a screen, and
// whether one was found.
//
// Only text after the last echo of the brief counts. The echo is the last
// line that names the marker and is not itself a report line; the brief's
// instruction names the marker, so the brief can never supply its own
// report. Of the report lines after it, the last wins. A report wrapped onto
// following indented lines is joined back into one line.
func FindReport(screen, marker string) (string, bool) {
	if !validMarker(marker) {
		return "", false
	}
	re := regexp.MustCompile(reportPrefix + `REPORT[ \t]+` + marker + "[*_`]*:[*_`]*[ \t]*(.*)$")
	lines := strings.Split(screen, "\n")
	cut := -1
	for i, l := range lines {
		if strings.Contains(l, marker) && !re.MatchString(l) {
			cut = i
		}
	}
	found := -1
	var first string
	for i := cut + 1; i < len(lines); i++ {
		if m := re.FindStringSubmatch(lines[i]); m != nil {
			found, first = i, m[1]
		}
	}
	if found < 0 {
		return "", false
	}
	parts := []string{strings.TrimSpace(first)}
	for _, l := range lines[found+1:] {
		if l == "" || (l[0] != ' ' && l[0] != '\t') || strings.TrimSpace(l) == "" {
			break
		}
		parts = append(parts, strings.TrimSpace(l))
	}
	report := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if utf8.RuneCountInString(report) > maxReportRunes {
		report = string([]rune(report)[:maxReportRunes])
	}
	return report, true
}
