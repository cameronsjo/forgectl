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
