// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Executables the herdr-app client runs. HCBinary supplies only the working
// folder of each terminal: `herdr-app ls` does not report one.
const (
	AppBinary = "herdr-app"
	HCBinary  = "hc"
)

// ErrUnsupportedMove means the host cannot make the requested move. herdr-app
// has `tab.move` (a position inside the tab's own space) and `space.move`, but
// no command that takes a tab to another space or creates a space for it.
var ErrUnsupportedMove = errors.New("herdr-app cannot move a tab to another space; it only reorders tabs within a space")

// AppClient reads and changes the herdr-app window the process runs in, by
// shelling to `herdr-app` (and, for terminal folders, `hc ls`) through an
// [exec.Runner]. It never calls herdr-app from the worker launch path; only
// `forgectl herdr organize` builds one.
//
// # Mapping onto the herdr 0.9 model
//
// The organize planner speaks the 0.9 vocabulary, so the client translates:
//
//   - space -> [Workspace]: WorkspaceID is the space id, Label its name (the
//     folder's base name when the space is unnamed), Number its 1-based
//     position, Focused its active flag, ActiveTabID its active tab.
//   - tab -> [Tab]: TabID is the tab id, Label its name ("tab N" when
//     unnamed, which is what herdr-app calls it), Number its 1-based
//     position, Focused its active flag.
//   - terminal -> [Pane]: PaneID, TerminalID and TabID come from the ls
//     row; TerminalTitleStripped is the tab's label (herdr-app reports no
//     terminal title, so the tab name is the title a rule's glob sees); CWD is
//     the terminal's folder from `hc ls`, matched by terminal id. Focused is
//     set on the first terminal of the active tab of the active space: ls does
//     not name a focused pane, and tab grain is what focus restore uses.
//   - positions: herdr-app counts from 1, the 0.9 model and organize steps
//     from 0. Every index goes out as position+1.
//
// herdr-app ids are not renumbered by a move (unverified against a live
// window: no mutation was run while this was written), but callers still
// re-read before acting, as they do on 0.9.
//
// A tab cannot change space, so [AppClient.MoveTab] accepts only an index
// target. The organize command tells the planner (organize.Config.StayInSpace)
// to report every cross-space move as blocked instead of attempting it.
type AppClient struct {
	runner exec.Runner
	// bin is the herdr-app executable: AppBinary, found on PATH, unless
	// NewAppAt named one.
	bin string
}

// NewApp returns an AppClient over r. It performs no host check; see
// [DetectHost] and [CheckApp].
func NewApp(r exec.Runner) *AppClient { return &AppClient{runner: r, bin: AppBinary} }

// NewAppAt returns an AppClient that runs herdr-app from bin, an absolute
// path: for a process started without the operator's PATH, such as a
// notification click.
func NewAppAt(r exec.Runner, bin string) *AppClient { return &AppClient{runner: r, bin: bin} }

// The shapes below are the subset of `herdr-app ls` and `hc ls` the client
// reads (recorded under testdata). Unknown fields are ignored.
type appLS struct {
	Spaces []appSpace `json:"spaces"`
}

type appSpace struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Folder string   `json:"folder"`
	Active bool     `json:"active"`
	Tabs   []appTab `json:"tabs"`
}

type appTab struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Active    bool          `json:"active"`
	Terminals []appTerminal `json:"terminals"`
}

type appTerminal struct {
	Terminal string `json:"terminal"`
	Pane     string `json:"pane"`
	App      string `json:"app"`
	Name     string `json:"name"`
}

type hcLS struct {
	Terminals []struct {
		Address string `json:"address"`
		CWD     string `json:"cwd"`
	} `json:"terminals"`
}

func (a *AppClient) run(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := a.runner.Run(ctx, bin, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return out, nil
}

func (a *AppClient) ls(ctx context.Context) (appLS, error) {
	out, err := a.run(ctx, a.bin, "ls")
	if err != nil {
		return appLS{}, err
	}
	var d struct {
		Spaces *[]appSpace `json:"spaces"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return appLS{}, fmt.Errorf("%s ls: not JSON: %w", AppBinary, err)
	}
	// A renamed or missing key must not read as an empty window.
	if d.Spaces == nil {
		return appLS{}, fmt.Errorf(`%s ls: response has no "spaces"`, AppBinary)
	}
	return appLS{Spaces: *d.Spaces}, nil
}

// spaceLabels names every space. A named space keeps its name. An unnamed one
// is labeled by its folder's base name, or "space N" (N its 1-based position)
// when it has no usable folder; two unnamed spaces that would share a label
// each get " N" appended, so no two unnamed spaces collide.
func spaceLabels(spaces []appSpace) []string {
	base := func(s appSpace) string {
		if s.Name != "" {
			return ""
		}
		b := filepath.Base(s.Folder)
		if s.Folder == "" || b == "." || b == string(filepath.Separator) {
			return ""
		}
		return b
	}
	uses := map[string]int{}
	for _, s := range spaces {
		if b := base(s); b != "" {
			uses[b]++
		}
	}
	out := make([]string, len(spaces))
	for i, s := range spaces {
		switch b := base(s); {
		case s.Name != "":
			out[i] = s.Name
		case b == "":
			out[i] = "space " + strconv.Itoa(i+1)
		case uses[b] > 1:
			out[i] = b + " " + strconv.Itoa(i+1)
		default:
			out[i] = b
		}
	}
	return out
}

func tabLabel(t appTab, pos int) string {
	if t.Name != "" {
		return t.Name
	}
	return "tab " + strconv.Itoa(pos)
}

// Workspaces lists the window's spaces.
func (a *AppClient) Workspaces(ctx context.Context) ([]Workspace, error) {
	d, err := a.ls(ctx)
	if err != nil {
		return nil, err
	}
	labels := spaceLabels(d.Spaces)
	out := make([]Workspace, 0, len(d.Spaces))
	for i, s := range d.Spaces {
		w := Workspace{
			WorkspaceID: s.ID, Label: labels[i], Number: i + 1, Focused: s.Active, TabCount: len(s.Tabs),
		}
		for _, t := range s.Tabs {
			w.PaneCount += len(t.Terminals)
			if t.Active {
				w.ActiveTabID = t.ID
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// Tabs lists the tabs of one space, in the order herdr-app shows them.
func (a *AppClient) Tabs(ctx context.Context, workspaceID string) ([]Tab, error) {
	if err := checkID("space id", workspaceID); err != nil {
		return nil, err
	}
	d, err := a.ls(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range d.Spaces {
		if s.ID != workspaceID {
			continue
		}
		out := make([]Tab, 0, len(s.Tabs))
		for i, t := range s.Tabs {
			out = append(out, Tab{
				TabID: t.ID, WorkspaceID: s.ID, Label: tabLabel(t, i+1), Number: i + 1,
				Focused: t.Active, PaneCount: len(t.Terminals),
			})
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s ls: no space %q", AppBinary, workspaceID)
}

// Panes lists every terminal that sits in a tab. The folder of each comes
// from `hc ls`; a terminal hc does not list has an empty CWD.
func (a *AppClient) Panes(ctx context.Context) ([]Pane, error) {
	d, err := a.ls(ctx)
	if err != nil {
		return nil, err
	}
	cwd, err := a.folders(ctx)
	if err != nil {
		return nil, err
	}
	var out []Pane
	for _, s := range d.Spaces {
		for i, t := range s.Tabs {
			label := tabLabel(t, i+1)
			for j, term := range t.Terminals {
				out = append(out, Pane{
					PaneID: term.Pane, TabID: t.ID, WorkspaceID: s.ID, TerminalID: term.Terminal,
					Agent: term.App, Name: term.Name, CWD: cwd[term.Terminal],
					TerminalTitleStripped: label,
					Focused:               s.Active && t.Active && j == 0,
				})
			}
		}
	}
	return out, nil
}

func (a *AppClient) folders(ctx context.Context) (map[string]string, error) {
	out, err := a.run(ctx, HCBinary, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var d hcLS
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return nil, fmt.Errorf("%s ls: not JSON: %w", HCBinary, err)
	}
	m := make(map[string]string, len(d.Terminals))
	for _, t := range d.Terminals {
		m[t.Address] = t.CWD
	}
	return m, nil
}

// LocateTerminal finds the space and tab a terminal sits in, from
// `herdr-app ls` alone (no `hc ls`, which Panes also runs). A terminal no tab
// holds is an error naming it.
func (a *AppClient) LocateTerminal(ctx context.Context, terminal string) (spaceID, tabID string, err error) {
	if err := checkID("terminal id", terminal); err != nil {
		return "", "", err
	}
	d, err := a.ls(ctx)
	if err != nil {
		return "", "", err
	}
	for _, s := range d.Spaces {
		for _, t := range s.Tabs {
			for _, term := range t.Terminals {
				if term.Terminal == terminal {
					return s.ID, t.ID, nil
				}
			}
		}
	}
	return "", "", fmt.Errorf("%s ls: no tab holds terminal %q", AppBinary, terminal)
}

// FocusNeedsSpace reports that selecting a tab is not known to switch the
// space it is in, so a focus restore must finish by selecting the space.
func (a *AppClient) FocusNeedsSpace() bool { return true }

// MoveTab reorders a tab inside its space. Only an index target is supported:
// herdr-app cannot move a tab to another space, and a target that asks for
// that returns [ErrUnsupportedMove] before anything runs. The result carries
// the tab id and no tab list (the reply is not decoded), so a caller lists the
// tabs again.
func (a *AppClient) MoveTab(ctx context.Context, tabID string, to MoveTarget) (MoveResult, error) {
	if err := checkID("tab id", tabID); err != nil {
		return MoveResult{}, err
	}
	if to.mode != modeIndex {
		return MoveResult{}, ErrUnsupportedMove
	}
	if to.index < 0 {
		return MoveResult{}, fmt.Errorf("herdr-app: negative tab index %d", to.index)
	}
	if err := a.do(ctx, "tab.move", map[string]any{"id": tabID, "index": to.index + 1}); err != nil {
		return MoveResult{}, err
	}
	return MoveResult{TabID: tabID}, nil
}

// MoveWorkspace reorders a space to a 0-based index.
func (a *AppClient) MoveWorkspace(ctx context.Context, workspaceID string, index int) error {
	if err := checkID("space id", workspaceID); err != nil {
		return err
	}
	if index < 0 {
		return fmt.Errorf("herdr-app: negative space index %d", index)
	}
	return a.do(ctx, "space.move", map[string]any{"id": workspaceID, "index": index + 1})
}

// FocusWorkspace shows a space.
func (a *AppClient) FocusWorkspace(ctx context.Context, workspaceID string) error {
	if err := checkID("space id", workspaceID); err != nil {
		return err
	}
	return a.do(ctx, "space.select", map[string]any{"id": workspaceID})
}

// FocusTab shows a tab.
func (a *AppClient) FocusTab(ctx context.Context, tabID string) error {
	if err := checkID("tab id", tabID); err != nil {
		return err
	}
	return a.do(ctx, "tab.select", map[string]any{"id": tabID})
}

// do runs one herdr-app command with its arguments as a single JSON object. A
// reply that is a JSON object with an "error" member is a refusal even when
// the exit status was 0. The success reply's shape is not known (no mutation
// was run against a live window), so nothing else is read from it.
func (a *AppClient) do(ctx context.Context, command string, args map[string]any) error {
	// termsafe:allow-raw-json builds an argv operand for herdr-app, never terminal output
	payload, err := json.Marshal(args)
	if err != nil {
		return err
	}
	out, err := a.run(ctx, a.bin, "do", command, string(payload))
	if err != nil {
		return err
	}
	var reply struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(out), &reply) == nil && len(reply.Error) > 0 && string(reply.Error) != "null" {
		return fmt.Errorf("%s do %s: refused: %s", AppBinary, command, printableMax(string(reply.Error)))
	}
	return nil
}

// appNeeded are the commands organize --apply drives.
var appNeeded = []string{"tab.move", "space.move", "tab.select", "space.select"}

// lookPath finds a binary on PATH; tests replace it.
var lookPath = osexec.LookPath

// CheckApp reports whether `hc` is on PATH and herdr-app lists the commands
// `organize --apply` needs. `herdr-app commands` prints the built-in list without an open window,
// so this checks the binary, not the window; a window that is closed surfaces
// at the first read.
func CheckApp(ctx context.Context, r exec.Runner) error {
	// hc supplies each terminal's folder; refuse here, with the other gate
	// errors, rather than at the first read.
	if _, err := lookPath(HCBinary); err != nil {
		return fmt.Errorf("%s is not on PATH, and organize reads each terminal's folder from `%s ls`: %w", HCBinary, HCBinary, err)
	}
	out, err := r.Run(ctx, AppBinary, "commands")
	if err != nil {
		return fmt.Errorf("cannot run %s commands: %w", AppBinary, err)
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return fmt.Errorf("%s commands: not a JSON list: %w", AppBinary, err)
	}
	var have []string
	for _, c := range list {
		have = append(have, c.Name)
	}
	var missing []string
	for _, n := range appNeeded {
		if !slices.Contains(have, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s has no %s, which organize --apply needs", AppBinary, strings.Join(missing, ", "))
	}
	return nil
}
