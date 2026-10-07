// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// ErrRunWatchNeedsTerminal is RunWatch's refusal without a terminal on stdin
// and stdout.
var ErrRunWatchNeedsTerminal = errors.New("desk show --live: the live view needs a terminal on stdin and stdout; drop --live for one printed view, or add --json")

// ErrRunWatchNoRun is RunWatch's result when the run named is not there.
var ErrRunWatchNoRun = errors.New("no such run")

// RunWatchOptions configures the live run view on its own.
type RunWatchOptions struct {
	Theme theme.Theme
	// Name is the run to open; "" opens the first the source lists.
	Name string
	// Poll is how often the run is re-read; 0 means deskPollEvery.
	Poll  time.Duration
	ASCII bool
}

// RunWatch opens the run view full screen on one run of src, without the
// dashboard around it: the same flow, timeline, replay and keys as the
// dashboard's r. It follows the run live until q.
func RunWatch(ctx context.Context, src runview.Source, opts RunWatchOptions) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return ErrRunWatchNeedsTerminal
	}
	final, err := tea.NewProgram(newRunWatchModel(ctx, src, opts), keymap.ProgramOptions(ctx)...).Run()
	if err != nil {
		return err
	}
	if w, ok := final.(runWatchModel); ok && w.missing {
		return fmt.Errorf("%w: %s", ErrRunWatchNoRun, safeMessage(opts.Name))
	}
	return nil
}

// runWatchModel hosts the dashboard's run view alone. It holds a deskModel
// with no desk: the run view reads only its runs, theme and size, so the
// keys, loads and replay are the dashboard's own code, not a copy.
type runWatchModel struct {
	m deskModel
	// missing is set when the run named was not there to open.
	missing bool
}

func newRunWatchModel(ctx context.Context, src runview.Source, opts RunWatchOptions) runWatchModel {
	poll := opts.Poll
	if poll <= 0 {
		poll = deskPollEvery
	}
	m := deskModel{ctx: ctx, theme: opts.Theme, poll: poll, runs: src, opts: DeskOptions{ASCII: opts.ASCII}}
	m.runGen++
	m.rv = &deskRunView{gen: m.runGen, follow: true, loading: true, ascii: opts.ASCII, want: opts.Name}
	return runWatchModel{m: m}
}

func (w runWatchModel) Init() tea.Cmd {
	return tea.Batch(loadRunCmd(w.m.runs, w.m.rv.gen, w.m.rv.want, runview.RunRef{}, nil), w.m.tick())
}

func (w runWatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var next tea.Model
	var cmd tea.Cmd
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		w.m.width, w.m.height = t.Width, t.Height
		return w, nil
	case tea.BackgroundColorMsg:
		if w.m.theme.Mode() == theme.ModeAuto {
			w.m.theme = w.m.theme.WithDark(t.IsDark())
		}
		return w, nil
	case deskTickMsg:
		return w, tea.Batch(w.m.tick(), w.m.pollRun())
	case deskRunLoadMsg:
		next, cmd = w.m.applyRunLoad(t)
	case deskRunPlayMsg:
		next, cmd = w.m.playStep(t)
	case tea.KeyPressMsg:
		switch t.String() {
		case "ctrl+c":
			return w, tea.Quit
		case "esc", "r":
			return w, nil // the dashboard's close keys; here only q quits
		}
		next, cmd = w.m.runViewKey(t.String())
	default:
		return w, nil
	}
	w.m = next.(deskModel) //nolint:forcetypeassert // the run view's methods return a deskModel
	if w.m.rv == nil {
		_, load := msg.(deskRunLoadMsg)
		w.missing = load
		// q closed the view, or the run it was opened on is gone: there is
		// no dashboard to fall back to.
		return w, tea.Quit
	}
	return w, cmd
}

func (w runWatchModel) View() tea.View {
	v := tea.NewView("")
	if w.m.rv != nil {
		v = tea.NewView(asciiFrame(w.m.rv.render(w.m.styles(), w.m.screenWidth(), w.m.screenHeight()), w.m.opts.ASCII))
	}
	v.AltScreen = true
	v.WindowTitle = "forgectl run"
	if w.m.rv != nil && w.m.rv.ref().Name != "" {
		v.WindowTitle = asciiFrame("run ● "+safeMessage(w.m.rv.ref().Name), w.m.opts.ASCII)
	}
	return v
}
