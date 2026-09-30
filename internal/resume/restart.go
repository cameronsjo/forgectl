package resume

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
)

// This file is the decision half of `forgectl resume restart --outdated`:
// planning, the safe-to-restart predicate, and reading Claude Code's input box
// off a screen. Everything here is pure. The I/O half — signals, herdr calls,
// polling — is restart_run.go, behind the RestartEnv seam.
//
// The predicate comes from forgectl#725's measurements
// (docs/research/2026-09-29-idle-session-restart-safety.md in the planning
// worktree). A session is safe to stop only when all four hold, re-checked
// immediately before the signal:
//
//  1. its pid is alive and is still the claude process that wrote the registry
//     file (same session id, same procStart, a claude exec path, and a kernel
//     start time that matches procStart);
//  2. its status is exactly "idle";
//  3. herdr confirms the pane: the pane reports the same session id, and the
//     pid is that pane's foreground process;
//  4. the pane's input line is empty — `idle` says nothing about an unsent
//     draft, and every stop method measured drops one.
//
// Failing 2 or 4 is temporary, so the session waits. Failing 1 or 3 means the
// thing this run would signal is not the thing it selected, so it is refused
// and reported with the command to run by hand.

// ErrPaneGone reports that herdr has no pane by the id the session claims
// (herdr's pane_not_found). It is the one pane error that refuses a session;
// every other herdr failure waits.
var ErrPaneGone = errors.New("herdr pane not found")

// ErrHerdrTimeout reports a herdr call killed at its bound rather than one
// herdr answered with a failure. The call may have done its work before it
// was killed: a `pane run` may already have typed the relaunch line.
var ErrHerdrTimeout = errors.New("herdr call timed out")

// ProcIdentity is what the kernel says about a pid, as opposed to what a
// registry file says about it.
type ProcIdentity struct {
	ExecPath string
	Start    time.Time
}

// PaneState is what herdr reports about one pane: its agent label and its
// foreground process group.
type PaneState struct {
	// Agent and AgentSession are agent_session.agent and agent_session.value
	// from `herdr pane get`: which harness herdr believes runs in the pane and
	// its session id. Both are reported by the harness's own hooks, so a nested
	// session can relabel its parent's pane — hence check 3 also wants the pid.
	Agent        string
	AgentSession string
	// ScrolledBack is scroll.offset_from_bottom > 0: the operator is looking
	// at older output, so the visible screen need not show the live input
	// line — and a stale box frame left in the scrollback could read as empty.
	ScrolledBack bool
	// ForegroundPGID and ShellPID come from `herdr pane process-info`. The shell
	// owning the foreground again is what says a stopped session's pane is
	// ready for the next command.
	ForegroundPGID int
	ShellPID       int
	ForegroundPIDs []int
}

// ShellForeground reports whether the pane's own shell holds the foreground —
// the state in which text sent to the pane reaches a shell prompt rather than
// a dying TUI.
func (s PaneState) ShellForeground() bool {
	return s.ShellPID > 0 && s.ForegroundPGID == s.ShellPID
}

// RestartAction is what a restart run intends for one selected session.
type RestartAction int

const (
	// ActionRestart: the session has a pane and is stopped and resumed once the
	// predicate holds.
	ActionRestart RestartAction = iota
	// ActionManual: no pane is known, so there is nowhere to relaunch it. It is
	// reported with the command to run by hand and never touched.
	ActionManual
	// ActionSkip: the session is left alone for a stated reason.
	ActionSkip
)

// RestartPlanItem is one selected session and what the run intends for it.
type RestartPlanItem struct {
	Session OutdatedSession
	// SessionID is set even when Session is empty: a --session id that matched
	// no outdated live session is still reported.
	SessionID string
	Action    RestartAction
	Reason    string
}

// ManualResume is the by-hand command for a session this run did not resume.
func ManualResume(sessionID string) string {
	return "forgectl resume " + sessionID
}

// PlanRestart turns the outdated set into a plan. only, when non-empty,
// restricts the plan to those session ids; an id in only that is not in list
// is planned as a skip, so a typo or an already-current session is reported
// rather than silently ignored. Plan order is list order, then the unmatched
// ids in the order given.
func PlanRestart(list []OutdatedSession, only []string) []RestartPlanItem {
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	var plan []RestartPlanItem
	seen := map[string]bool{}
	for _, s := range list {
		if len(only) > 0 && !want[s.SessionID] {
			continue
		}
		seen[s.SessionID] = true
		plan = append(plan, planOne(s))
	}
	for _, id := range only {
		if seen[id] {
			continue
		}
		seen[id] = true
		plan = append(plan, RestartPlanItem{
			SessionID: id, Action: ActionSkip,
			Reason: "not a live session running an older version than the one installed",
		})
	}
	return plan
}

func planOne(s OutdatedSession) RestartPlanItem {
	item := RestartPlanItem{Session: s, SessionID: s.SessionID}
	switch {
	case s.VersionUnparseable:
		// "MUST NOT restart a session whose version is current": an
		// unparseable version could be current, so it is not ours to stop.
		item.Action = ActionSkip
		item.Reason = "its recorded version cannot be compared, so nothing proves it outdated"
	case s.Pane == "":
		item.Action = ActionManual
		item.Reason = "no herdr pane in its environment, so there is nowhere to relaunch it; quit it, then run " + ManualResume(s.SessionID)
	case s.Busy:
		item.Action = ActionRestart
		item.Reason = fmt.Sprintf("waits: status is %q, restarts once it is idle", s.Status)
	default:
		item.Action = ActionRestart
		item.Reason = "idle; restarts once the pane checks pass"
	}
	return item
}

// Readiness is the verdict of one predicate evaluation.
type Readiness int

const (
	// Ready: every check passed; signal now.
	Ready Readiness = iota
	// NotYet: a temporary condition (busy, a draft, an unreadable screen);
	// check again later.
	NotYet
	// Refused: the process or pane is not the one selected; never signal,
	// report with the by-hand command.
	Refused
)

// Check is a verdict and the reason for it.
type Check struct {
	Readiness Readiness
	Reason    string
}

// Observation is everything one predicate evaluation reads, gathered by the
// I/O layer immediately before the decision.
type Observation struct {
	// Entry is the pid's registry file re-read now; EntryFound is false when it
	// is gone or unreadable.
	Entry      RegistryEntry
	EntryFound bool
	// Alive is a fresh liveness probe of the pid.
	Alive bool
	// Proc is the kernel's view of the pid.
	Proc    ProcIdentity
	ProcErr error
	// Pane is herdr's view of the pane; PaneErr is set when herdr could not
	// show it (the pane was closed, or herdr is not answering).
	Pane    PaneState
	PaneErr error
	// Screen is the pane's visible text; ScreenErr is set when it could not be
	// read.
	Screen    string
	ScreenErr error
}

// procStartTolerance absorbs procStart's whole-second resolution against the
// kernel's microsecond start time. A reused pid starts seconds to days later.
const procStartTolerance = 2 * time.Second

// Evaluate applies the four checks in order. The order is the refusal order:
// an identity failure is reported as that even when the session is also busy,
// because it is the one that means "do not touch this".
func Evaluate(want OutdatedSession, obs Observation) Check {
	if c, ok := checkIdentity(want, obs); !ok {
		return c
	}
	if IsBusy(obs.Entry.Status) {
		return Check{NotYet, fmt.Sprintf("status is %q, waiting for idle", obs.Entry.Status)}
	}
	if c, ok := checkPane(want, obs); !ok {
		return c
	}
	if obs.Pane.ScrolledBack {
		return Check{NotYet, "the pane is scrolled back, so its live input line is not on screen"}
	}
	if obs.ScreenErr != nil {
		return Check{NotYet, "could not read the pane's screen"}
	}
	if empty, reason := InputLineEmpty(obs.Screen); !empty {
		return Check{NotYet, reason}
	}
	return Check{Ready, "idle, pane confirmed, input line empty"}
}

// checkIdentity is check 1: the pid is alive and is the claude process that
// wrote the registry file this run selected.
func checkIdentity(want OutdatedSession, obs Observation) (Check, bool) {
	refuse := func(format string, a ...any) (Check, bool) {
		return Check{Refused, fmt.Sprintf(format, a...)}, false
	}
	switch {
	case !obs.Alive:
		return refuse("its process (pid %d) has exited", want.Pid)
	case !obs.EntryFound:
		return refuse("its registry file (%d.json) is gone", want.Pid)
	case obs.Entry.SessionID != want.SessionID:
		return refuse("pid %d's registry file now names another session (pid reused?)", want.Pid)
	case want.ProcStart == "":
		return refuse("its registry file records no procStart, so pid reuse cannot be ruled out")
	case obs.Entry.ProcStart != want.ProcStart:
		return refuse("pid %d's recorded start time changed (pid reused?)", want.Pid)
	case obs.ProcErr != nil:
		return refuse("could not read pid %d's executable and start time", want.Pid)
	case !IsClaudeExec(obs.Proc.ExecPath):
		return refuse("pid %d is not running a claude binary", want.Pid)
	}
	recorded, err := ParseProcStart(want.ProcStart)
	if err != nil {
		return refuse("its recorded procStart does not parse, so pid reuse cannot be ruled out")
	}
	if d := obs.Proc.Start.Sub(recorded); d > procStartTolerance || d < -procStartTolerance {
		return refuse("pid %d started at a different time than the registry records (pid reused?)", want.Pid)
	}
	return Check{}, true
}

// checkPane is check 3: herdr agrees the pane holds this session, and the pid
// is the pane's foreground process. Both halves are needed because either
// source alone can be wrong in the same way: a claude started inside another
// session inherits its parent's HERDR_PANE_ID, and its hooks relabel the
// parent's pane.
func checkPane(want OutdatedSession, obs Observation) (Check, bool) {
	switch {
	case errors.Is(obs.PaneErr, ErrPaneGone):
		return Check{Refused, fmt.Sprintf("pane %s no longer exists", want.Pane)}, false
	case obs.PaneErr != nil:
		// Any other failure (herdr not answering, a timeout) is not evidence
		// about the pane, so it waits rather than refusing for good.
		return Check{NotYet, fmt.Sprintf("herdr could not show pane %s; retrying", want.Pane)}, false
	case obs.Pane.Agent != "claude" || obs.Pane.AgentSession != want.SessionID:
		return Check{Refused, fmt.Sprintf("pane %s's herdr label names a different session (nested session?)", want.Pane)}, false
	case !slices.Contains(obs.Pane.ForegroundPIDs, want.Pid):
		return Check{Refused, fmt.Sprintf("pid %d is not pane %s's foreground process (nested session?)", want.Pid, want.Pane)}, false
	}
	return Check{}, true
}

// IsClaudeExec reports whether an exec path names a Claude Code binary: a file
// named claude (the native installer's ~/.local/bin/claude link, as execve saw
// it), or a versions/<X> target the link points at. Anything else — node
// running an npm install, a reused pid running something unrelated — is not
// recognized, which refuses rather than guesses.
func IsClaudeExec(path string) bool {
	if path == "" {
		return false
	}
	base := filepath.Base(path)
	if base == "claude" {
		return true
	}
	if filepath.Base(filepath.Dir(path)) != "versions" {
		return false
	}
	_, err := ParseVersion(base)
	return err == nil
}

// procStartLayout is ps(1)'s lstart format, which Claude Code writes in UTC.
// Runs of spaces are collapsed before parsing, so a space-padded day of the
// month ("Sep  3") reads the same as "Sep 3".
const procStartLayout = "Mon Jan 2 15:04:05 2006"

// ParseProcStart parses a registry procStart value as UTC.
func ParseProcStart(s string) (time.Time, error) {
	t, err := time.ParseInLocation(procStartLayout, strings.Join(strings.Fields(s), " "), time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("procStart %q: %w", s, err)
	}
	return t, nil
}

// Input-box reasons, shared with the tests so a wording change is one edit.
const (
	reasonBoxUnrecognized = "the pane's input box is not recognized, so an unsent draft cannot be ruled out"
	reasonDraft           = "the pane's input line holds an unsent draft"
)

// minRuleRunes is the shortest run of box-drawing dashes read as Claude Code's
// input-box border. Anything shorter is a separator in ordinary output.
const minRuleRunes = 20

// InputLineEmpty reports whether Claude Code's input line on a screen is
// empty. Claude Code draws the input as a "❯" line between two horizontal
// rules of "─", above its status lines; an empty draft renders as "❯" with
// nothing after it.
//
// It fails closed. A screen whose last two rules do not enclose a line
// beginning with "❯" is not recognized, and an unrecognized screen is treated
// as holding a draft: a false "empty" loses typed text for good, a false
// "draft" only delays the restart. The shell prompt this operator uses also
// starts with "❯", which is why the rules, not the glyph alone, locate the box.
//
// The screen is untrusted text from another program; this reads it and never
// echoes it.
func InputLineEmpty(screen string) (bool, string) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	bottom, top := -1, -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !isRule(lines[i]) {
			continue
		}
		if bottom < 0 {
			bottom = i
			continue
		}
		top = i
		break
	}
	if top < 0 || bottom-top < 2 {
		return false, reasonBoxUnrecognized
	}
	box := lines[top+1 : bottom]
	first := strings.TrimLeftFunc(box[0], unicode.IsSpace)
	rest, ok := strings.CutPrefix(first, "❯")
	if !ok {
		return false, reasonBoxUnrecognized
	}
	if strings.TrimFunc(rest, unicode.IsSpace) != "" {
		return false, reasonDraft
	}
	for _, l := range box[1:] {
		if strings.TrimFunc(l, unicode.IsSpace) != "" {
			return false, reasonDraft
		}
	}
	return true, ""
}

// isRule reports whether a line is a horizontal rule: at least minRuleRunes
// "─", making up at least four fifths of the line's non-space runes (so a rule
// carrying a short embedded label still counts).
func isRule(line string) bool {
	dashes, other := 0, 0
	for _, r := range line {
		switch {
		case r == '─':
			dashes++
		case unicode.IsSpace(r):
		default:
			other++
		}
	}
	return dashes >= minRuleRunes && dashes*5 >= (dashes+other)*4
}

// ValidSessionID reports whether id has the shape of a Claude Code session id:
// hex and dashes, bounded, never flag-shaped. It gates every id that reaches a
// path or an argv.
func ValidSessionID(id string) bool { return validSessionID(id) }
