package herdr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/wire"
)

// The pane verbs below build a layout: split a pane, label it, and type a
// command into it. They run through the sensitive seam, each under its own
// [exec.CommandKind], because a split carries a cwd and pane run carries an
// operator-built command line, neither of which belongs in a log line.
//
// Pane ids renumber (see the package doc): resolve a pane by terminal_id
// with [PaneByTerminal] immediately before each call that names one.

// paneStreamCap bounds herdr's reply to a pane verb: one pane_info object at
// most.
const paneStreamCap = 64 << 10

// MaxRunCommandBytes caps the command line [PaneRun] types.
const MaxRunCommandBytes = 4096

// SplitDirection is where a split puts the new pane.
type SplitDirection string

const (
	SplitRight SplitDirection = "right"
	SplitDown  SplitDirection = "down"
)

// Split describes one `herdr pane split`.
type Split struct {
	// Pane is the pane to split; "" splits the caller's own pane (--current,
	// which herdr resolves from HERDR_PANE_ID).
	Pane      string
	Direction SplitDirection
	// Ratio is the share the ORIGINAL pane keeps (measured: 0.3 left 53 of
	// 175 columns), in (0, 1).
	Ratio float64
	// CWD is the new pane's working directory; "" leaves herdr's default.
	// It must be absolute.
	CWD string
}

// PaneSplit runs `herdr pane split` without moving focus and returns the new
// pane, including its terminal_id.
func PaneSplit(ctx context.Context, run exec.SensitiveRunner, herdrPath string, s Split) (Pane, error) {
	args := []exec.Arg{exec.MustFixed("pane"), exec.MustFixed("split")}
	if s.Pane == "" {
		args = append(args, exec.MustFixed("--current"))
	} else {
		if err := checkID("pane id", s.Pane); err != nil {
			return Pane{}, err
		}
		args = append(args, exec.MustFixed("--pane"), exec.Opaque(s.Pane))
	}
	switch s.Direction {
	case SplitRight:
		args = append(args, exec.MustFixed("--direction"), exec.MustFixed("right"))
	case SplitDown:
		args = append(args, exec.MustFixed("--direction"), exec.MustFixed("down"))
	default:
		return Pane{}, fmt.Errorf("herdr: unknown split direction %q", s.Direction)
	}
	if !(s.Ratio > 0 && s.Ratio < 1) {
		return Pane{}, fmt.Errorf("herdr: split ratio %v is not between 0 and 1", s.Ratio)
	}
	args = append(args, exec.MustFixed("--ratio"), exec.Opaque(strconv.FormatFloat(s.Ratio, 'f', 2, 64)))
	if s.CWD != "" {
		if !filepath.IsAbs(s.CWD) {
			return Pane{}, errors.New("herdr: split cwd must be an absolute path")
		}
		args = append(args, exec.MustFixed("--cwd"), exec.Opaque(s.CWD))
	}
	args = append(args, exec.MustFixed("--no-focus"))
	out, err := runVerb(ctx, run, herdrPath, exec.KindHerdrPaneSplit, "pane split", args)
	if err != nil {
		return Pane{}, err
	}
	r, err := wire.DecodeResult[struct {
		Pane *Pane `json:"pane"`
	}](out)
	if err != nil {
		return Pane{}, fmt.Errorf("herdr pane split: %w", err)
	}
	if r.Pane == nil || r.Pane.PaneID == "" || r.Pane.TerminalID == "" {
		return Pane{}, errors.New("herdr pane split: the reply names no pane_id and terminal_id")
	}
	return *r.Pane, nil
}

// PaneRename runs `herdr pane rename PANE LABEL`. label must be one safe
// operand (see [wire.CheckOperand]); herdr's "--clear" is not reachable here.
func PaneRename(ctx context.Context, run exec.SensitiveRunner, herdrPath, paneID, label string) error {
	if err := checkID("pane id", paneID); err != nil {
		return err
	}
	if err := checkID("pane label", label); err != nil {
		return err
	}
	_, err := runVerb(ctx, run, herdrPath, exec.KindHerdrPaneRename, "pane rename",
		[]exec.Arg{exec.MustFixed("pane"), exec.MustFixed("rename"), exec.Opaque(paneID), exec.Opaque(label)})
	return err
}

// PaneRun runs `herdr pane run PANE COMMAND`, which types command into the
// pane and presses Enter. The pane's shell interprets it, so command is
// shell text the caller built and quoted; here it is passed as one argv
// element and never through a shell of forgectl's own. It must be one line
// with no control characters (a newline would submit early) and must not
// start with "-": `pane run` types a "--" rather than parsing it (measured in
// internal/surface/herdradapter), so a dash-leading command has no safe form.
func PaneRun(ctx context.Context, run exec.SensitiveRunner, herdrPath, paneID, command string) error {
	if err := checkID("pane id", paneID); err != nil {
		return err
	}
	if err := CheckRunCommand(command); err != nil {
		return err
	}
	_, err := runVerb(ctx, run, herdrPath, exec.KindHerdrPaneRun, "pane run",
		[]exec.Arg{exec.MustFixed("pane"), exec.MustFixed("run"), exec.Opaque(paneID), exec.Opaque(command)})
	return err
}

// DeskAgentSource is the --source and --agent label the desk reports under.
// herdr keeps lifecycle authority per source, so the desk's blocked state
// never overwrites the session's own, and release-agent hands back only what
// this source took.
const DeskAgentSource = "forgectl-desk"

// The pane id comes before the flags in both verbs below. herdr 0.9.1 help
// shows it last, but `pane release-agent --source S --agent S PANE` exits 2
// with "unknown option: S"; with the pane first it works (measured).

// PaneReportBlocked runs `herdr pane report-agent --source forgectl-desk
// --agent forgectl-desk --state blocked --message MESSAGE PANE`, which puts
// the pane in herdr's needs-you state. message is untrusted text (an item
// name) and is rendered inert and capped first.
func PaneReportBlocked(ctx context.Context, run exec.SensitiveRunner, herdrPath, paneID, message string) error {
	if err := checkID("pane id", paneID); err != nil {
		return err
	}
	args := []exec.Arg{
		exec.MustFixed("pane"), exec.MustFixed("report-agent"), exec.Opaque(paneID),
		exec.MustFixed("--source"), exec.MustFixed(DeskAgentSource),
		exec.MustFixed("--agent"), exec.MustFixed(DeskAgentSource),
		exec.MustFixed("--state"), exec.MustFixed("blocked"),
	}
	if text := notificationText(message); text != "" {
		args = append(args, exec.MustFixed("--message"), exec.Opaque(text))
	}
	_, err := runVerb(ctx, run, herdrPath, exec.KindHerdrPaneAgent, "pane report-agent", args)
	return err
}

// PaneReleaseDesk runs `herdr pane release-agent --source forgectl-desk
// --agent forgectl-desk PANE`, clearing what [PaneReportBlocked] set.
func PaneReleaseDesk(ctx context.Context, run exec.SensitiveRunner, herdrPath, paneID string) error {
	if err := checkID("pane id", paneID); err != nil {
		return err
	}
	_, err := runVerb(ctx, run, herdrPath, exec.KindHerdrPaneAgent, "pane release-agent", []exec.Arg{
		exec.MustFixed("pane"), exec.MustFixed("release-agent"), exec.Opaque(paneID),
		exec.MustFixed("--source"), exec.MustFixed(DeskAgentSource),
		exec.MustFixed("--agent"), exec.MustFixed(DeskAgentSource),
	})
	return err
}

// PaneMove describes one `herdr pane move`. The pane keeps its terminal, so
// whatever runs in it keeps running; only its place in the layout changes.
type PaneMove struct {
	// Pane is the pane to move.
	Pane string
	// NewTab moves the pane into a new tab of its workspace. Otherwise Tab
	// names the destination tab.
	NewTab bool
	Tab    string
	// Target is the pane in Tab to split; "" splits the tab's focused pane.
	// Direction and Ratio apply only with Tab (the first pane of a new tab
	// fills it).
	Target    string
	Direction SplitDirection
	// Ratio is the share Target keeps, in (0, 1).
	Ratio float64
}

// MovedPane is what `herdr pane move` reports: the pane as it is after the
// move, and the new tab when the move made one.
type MovedPane struct {
	Pane   Pane
	NewTab string
}

// PanePlace runs `herdr pane move` without moving focus. A move herdr
// declines (changed=false) returns an error, so a caller never carries on as
// if the pane had moved.
func PanePlace(ctx context.Context, run exec.SensitiveRunner, herdrPath string, m PaneMove) (MovedPane, error) {
	if err := checkID("pane id", m.Pane); err != nil {
		return MovedPane{}, err
	}
	args := []exec.Arg{exec.MustFixed("pane"), exec.MustFixed("move"), exec.Opaque(m.Pane)}
	if m.NewTab {
		args = append(args, exec.MustFixed("--new-tab"))
	} else {
		if err := checkID("tab id", m.Tab); err != nil {
			return MovedPane{}, err
		}
		args = append(args, exec.MustFixed("--tab"), exec.Opaque(m.Tab))
		if m.Target != "" {
			if err := checkID("pane id", m.Target); err != nil {
				return MovedPane{}, err
			}
			args = append(args, exec.MustFixed("--target-pane"), exec.Opaque(m.Target))
		}
		switch m.Direction {
		case SplitRight:
			args = append(args, exec.MustFixed("--split"), exec.MustFixed("right"))
		case SplitDown:
			args = append(args, exec.MustFixed("--split"), exec.MustFixed("down"))
		default:
			return MovedPane{}, fmt.Errorf("herdr: unknown split direction %q", m.Direction)
		}
		if !(m.Ratio > 0 && m.Ratio < 1) {
			return MovedPane{}, fmt.Errorf("herdr: move ratio %v is not between 0 and 1", m.Ratio)
		}
		args = append(args, exec.MustFixed("--ratio"), exec.Opaque(strconv.FormatFloat(m.Ratio, 'f', 2, 64)))
	}
	args = append(args, exec.MustFixed("--no-focus"))
	out, err := runVerb(ctx, run, herdrPath, exec.KindHerdrPaneMove, "pane move", args)
	if err != nil {
		return MovedPane{}, err
	}
	r, err := wire.DecodeResult[struct {
		MoveResult *struct {
			Changed    *bool `json:"changed"`
			Pane       *Pane `json:"pane"`
			CreatedTab *struct {
				TabID string `json:"tab_id"`
			} `json:"created_tab"`
		} `json:"move_result"`
	}](out)
	if err != nil {
		return MovedPane{}, fmt.Errorf("herdr pane move: %w", err)
	}
	mr := r.MoveResult
	switch {
	case mr == nil || mr.Changed == nil:
		return MovedPane{}, errors.New("herdr pane move: the reply has no move_result.changed")
	case !*mr.Changed:
		return MovedPane{}, fmt.Errorf("herdr pane move: herdr declined to move pane %s", printableMax(m.Pane))
	case mr.Pane == nil || mr.Pane.PaneID == "" || mr.Pane.TerminalID == "":
		return MovedPane{}, errors.New("herdr pane move: the reply names no pane_id and terminal_id")
	}
	out2 := MovedPane{Pane: *mr.Pane}
	if mr.CreatedTab != nil {
		out2.NewTab = mr.CreatedTab.TabID
	}
	if m.NewTab && out2.NewTab == "" {
		out2.NewTab = mr.Pane.TabID
	}
	return out2, nil
}

// CheckRunCommand reports why command cannot be typed by [PaneRun], or nil.
func CheckRunCommand(command string) error {
	switch {
	case command == "":
		return errors.New("herdr: pane run needs a command")
	case len(command) > MaxRunCommandBytes:
		return fmt.Errorf("herdr: pane run command is longer than %d bytes", MaxRunCommandBytes)
	case !utf8.ValidString(command):
		return errors.New("herdr: pane run command is not valid UTF-8")
	case command[0] == '-':
		return errors.New("herdr: pane run command must not start with '-'")
	}
	for _, r := range command {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return errors.New("herdr: pane run command must be one line with no control characters")
		}
	}
	return nil
}

// PaneByTerminal returns the pane whose terminal_id is terminalID, from a
// fresh `pane list`. Call it right before acting on the pane.
func (c *Client) PaneByTerminal(ctx context.Context, terminalID string) (Pane, error) {
	if terminalID == "" {
		return Pane{}, errors.New("herdr: empty terminal id")
	}
	panes, err := c.Panes(ctx)
	if err != nil {
		return Pane{}, err
	}
	for _, p := range panes {
		if p.TerminalID == terminalID {
			return p, nil
		}
	}
	return Pane{}, fmt.Errorf("herdr: no pane holds terminal %q any more", printableMax(terminalID))
}

// Rect is a layout rectangle in cells.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Layout is the part of `herdr pane layout` a caller sizes splits from.
type Layout struct {
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	FocusedPaneID string `json:"focused_pane_id"`
	Area          Rect   `json:"area"`
}

// CurrentLayout reads `herdr pane layout --current`: the layout of the tab
// holding the caller's own pane.
func (c *Client) CurrentLayout(ctx context.Context) (Layout, error) {
	args := []string{"pane", "layout", "--current"}
	r, err := read[struct {
		Layout *Layout `json:"layout"`
	}](ctx, c, args...)
	if err != nil {
		return Layout{}, err
	}
	l, err := need(r.Layout, "layout", args)
	if err != nil {
		return Layout{}, err
	}
	if l.Area.Width <= 0 {
		return Layout{}, fmt.Errorf("herdr %s: the layout has no area width", argvText(args))
	}
	return l, nil
}

// runVerb runs one herdr verb through the sensitive seam and returns its
// complete stdout. herdr's refusal envelope is read from stderr on exit 1, or
// from stdout on exit 0 (#722); a truncated stream is never parsed.
func runVerb(ctx context.Context, run exec.SensitiveRunner, herdrPath string, kind exec.CommandKind, what string, args []exec.Arg) ([]byte, error) {
	if !filepath.IsAbs(herdrPath) {
		return nil, fmt.Errorf("herdr: %s needs an absolute herdr path", what)
	}
	res, err := run.RunSensitive(ctx, exec.SensitiveCommand{
		Kind:      kind,
		Path:      exec.Secret(herdrPath),
		Args:      args,
		StdoutCap: paneStreamCap,
		StderrCap: paneStreamCap,
	})
	if err != nil {
		var se *exec.SensitiveError
		if errors.As(err, &se) && se.Outcome == exec.OutcomeExit && res.ExitCode == exitFailure {
			if data, complete := res.Stderr.CopyBytesForParse(); complete {
				if e := refusal(data); e != nil {
					e.cause = err
					return nil, e
				}
			}
		}
		return nil, fmt.Errorf("herdr %s: %w", what, err)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return nil, fmt.Errorf("herdr %s: the reply was cut short", what)
	}
	if e := refusal(data); e != nil {
		return nil, e
	}
	return data, nil
}
