// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// deskModule declares the desk: the dashboard (`desk`), its two agent verbs
// (`desk runs`, `desk show`), and the hidden supervisor the dashboard starts
// for a background item (`desk _supervise`).
var deskModule = module.Manifest{
	Name: "desk",
	Tier: module.TierExtension,
	New:  newDeskCmd,
}

// deskNoTTY is the dashboard's refusal off a terminal (ADR-0008 rule 1).
const deskNoTTY = `desk needs a terminal on stdin and stdout; use "desk runs --json" or "desk show <run> --json"`

// deskRuntime holds the desk's seams. Tests replace them; production uses
// deskDefaults.
type deskRuntime struct {
	stdinIsTerminal  func(io.Reader) bool
	stdoutIsTerminal func(io.Writer) bool
	getenv           func(string) string
	now              func() time.Time
	// runDesk is tui.RunDesk; foreground runs a TTY item in this terminal
	// and waits for a key after it (runForeground); waitKey waits for a key
	// after a foreground run that could not start.
	runDesk    func(context.Context, tui.DeskOptions) (tui.DeskResult, error)
	foreground func(d *desk.Desk, name, sha string) error
	waitKey    func() error
}

func deskDefaults() deskRuntime {
	return deskRuntime{
		stdinIsTerminal:  docsReadInputIsTerminal,
		stdoutIsTerminal: docsReadOutputIsTerminal,
		getenv:           os.Getenv,
		now:              time.Now,
		runDesk:          tui.RunDesk,
		foreground: func(d *desk.Desk, name, sha string) error {
			_, err := runForeground(d, name, sha, IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, TTY: int(os.Stdin.Fd())}) //nolint:gosec // G115: a descriptor fits in int
			return err
		},
		waitKey: func() error { return waitKey(int(os.Stdin.Fd())) }, //nolint:gosec // G115: a descriptor fits in int
	}
}

func newDeskCmd(deps module.Deps) *cobra.Command { return newDeskCmdWith(deps, deskDefaults()) }

func newDeskCmdWith(deps module.Deps, rt deskRuntime) *cobra.Command {
	var specs []string
	cmd := &cobra.Command{
		Use:   "desk",
		Short: "Approval queue and run viewer: approve staged items, watch multi-step runs",
		Long: `desk is a full-screen dashboard with two jobs: the approval queue (read,
approve, skip, or run the scripts and batches staged in the desk directory)
and the run viewer (watch a multi-step run move through its steps, replay a
finished one). Runs come from the desk itself and from every --spec passed.

  forgectl desk                          the dashboard (needs a terminal)
  forgectl desk --spec ~/pipeline.toml   add the runs a run spec describes
  forgectl desk runs --json              every run, as JSON (no terminal needed)
  forgectl desk show <run> --json        one run's state, as JSON

Without a terminal on stdin and stdout, desk exits 1 and names the two verbs.
Reference: docs/commands/desk.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Before anything is opened: listing the desk scans it, and a scan
			// changes the queue.
			// The line is printed as is, not through the styled error frame,
			// which would reword it.
			if !rt.stdinIsTerminal(cmd.InOrStdin()) || !rt.stdoutIsTerminal(cmd.OutOrStdout()) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), deskNoTTY)
				return newSilentCodedError(1)
			}
			noIcons, _ := cmd.Flags().GetBool("no-icons") // persistent root flag
			srcs := openDeskSources(rt.getenv, specs)
			defer srcs.close()
			return rt.dashboard(cmd.Context(), deps, srcs, noIcons)
		},
	}
	cmd.PersistentFlags().StringArrayVar(&specs, "spec", nil, "a run spec file whose runs to add (repeatable; experimental format)")
	cmd.AddCommand(newDeskRunsCmd(rt, &specs), newDeskShowCmd(rt, &specs), newDeskSuperviseCmd())
	return cmd
}

// dashboard runs the dashboard until the operator quits. A TTY item the
// operator approved closes it: the item runs in this terminal, and the
// dashboard opens again on that run with the view as it was.
func (rt deskRuntime) dashboard(ctx context.Context, deps module.Deps, srcs *deskSources, noIcons bool) error {
	var restore *tui.DeskViewState
	for {
		res, err := rt.runDesk(ctx, tui.DeskOptions{
			Sources: srcs.sources,
			Failed:  srcs.failed,
			Theme:   deps.Theme,
			NoIcons: noIcons,
			Restore: restore,
		})
		if errors.Is(err, tea.ErrInterrupted) {
			return nil
		}
		if err != nil {
			return err
		}
		run, ok := res.Action.(tui.DeskRun)
		if !ok || srcs.desk == nil {
			return nil
		}
		if err := rt.foreground(srcs.desk, run.Name, run.SHA); err != nil {
			if errors.Is(err, ErrPaneGone) {
				return nil
			}
			// The run never began, so nothing printed its line or waited.
			_, _ = fmt.Fprintf(os.Stderr, "\ncould not run %s: %s — press any key\n", safeLabel(run.Name), safeText(err.Error()))
			if errors.Is(rt.waitKey(), ErrPaneGone) {
				return nil
			}
		}
		state := res.State
		state.Source, state.Run = deskSourceName, run.Name
		restore = &state
	}
}

// deskSourceName is the desk source's name (runview's deskSource.Name).
const deskSourceName = "desk"

// deskSources are the run sources the desk reads: the desk first, then each
// spec in the order passed. A source that could not be built is in failed;
// the rest still load.
type deskSources struct {
	desk    *desk.Desk
	sources []runview.Source
	failed  []tui.DeskSourceFailure
}

func (s *deskSources) close() {
	if s.desk != nil {
		_ = s.desk.Close()
	}
}

// openDeskSources opens the desk and loads each spec. A spec whose name is
// already a source's fails on its own, since --from could not tell the two
// apart.
func openDeskSources(getenv func(string) string, specs []string) *deskSources {
	s := &deskSources{}
	home, homeErr := os.UserHomeDir()
	dir, err := desk.ResolveDir(getenv, home)
	if err != nil {
		err = errors.Join(err, homeErr)
	} else {
		s.desk, err = desk.Open(dir)
	}
	if err != nil {
		s.failed = append(s.failed, tui.DeskSourceFailure{Name: deskSourceName, Err: err})
	} else {
		s.sources = append(s.sources, runview.NewDeskSource(s.desk))
	}
	names := map[string]bool{deskSourceName: true}
	for _, p := range specs {
		src, err := openSpecSource(p, names)
		if err != nil {
			s.failed = append(s.failed, tui.DeskSourceFailure{Name: p, Err: err})
			continue
		}
		names[src.Name()] = true
		s.sources = append(s.sources, src)
	}
	return s
}

func openSpecSource(p string, names map[string]bool) (runview.Source, error) {
	spec, err := runview.LoadSpec(p)
	if err != nil {
		return nil, err
	}
	src, err := runview.NewSpecSource(spec)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", runview.ErrSpec, termsafe.QuotePath(p), err)
	}
	if names[src.Name()] {
		return nil, fmt.Errorf("%w %s: name: %s is already the name of another source", runview.ErrSpec, termsafe.QuotePath(p), termsafe.QuoteTextMax(src.Name(), labelMaxRunes))
	}
	return src, nil
}

// deskFailure is a source, or one run of a source, that could not be read.
type deskFailure struct{ err error }

// String is the failure as one line of text.
func (f deskFailure) String() string {
	return "desk: " + safeText(f.err.Error())
}

// deskRun is one run loaded in full for the agent verbs.
type deskRun struct {
	src    runview.Source
	ref    runview.RunRef
	spec   *runview.Spec
	delta  runview.Delta
	folder *runview.Folder
	state  runview.RunState
	live   runview.LiveState
}

// activity is when the run last did anything: its last event, else the time
// its source last saw it change.
func (r *deskRun) activity() time.Time {
	if !r.state.LastEvent.IsZero() {
		return r.state.LastEvent
	}
	return r.ref.Updated
}

func (r *deskRun) isLive() bool {
	return r.live == runview.LiveLive || r.live == runview.LiveRunning
}

// deskSpecFor is the spec a run folds with, as the dashboard picks it: the
// desk's for a desk run, else the source's own, else an empty spec.
func deskSpecFor(src runview.Source, ref runview.RunRef) *runview.Spec {
	if ref.Kind == runview.KindDesk {
		return runview.DeskSpec()
	}
	if sp, ok := src.(runview.SpecProvider); ok && sp.Spec() != nil {
		return sp.Spec()
	}
	return &runview.Spec{}
}

// loadDeskRun reads one run from the start and folds it as of now. One Load
// from a zero cursor reads everything the caps allow.
func loadDeskRun(src runview.Source, ref runview.RunRef, now time.Time) (*deskRun, error) {
	r := &deskRun{src: src, ref: ref, spec: deskSpecFor(src, ref)}
	d, err := src.Load(ref, &runview.Cursor{})
	if err != nil {
		return nil, err
	}
	r.delta = d
	r.folder = runview.NewFolder(r.spec, d.Defs)
	r.folder.Append(d.Events...)
	r.at(r.folder.Len(), now)
	return r, nil
}

// at folds the run to its first i events. A desk run's state is the desk's
// own word; any other run's is the fold's, which judges staleness at now.
func (r *deskRun) at(i int, now time.Time) {
	r.state = r.folder.At(i, now)
	r.live = r.state.Live
	if r.delta.Live != "" {
		r.live = r.delta.Live
	}
}

// listDeskRuns lists and loads every run of every source, in the dashboard's
// order: live runs first, then by last activity, newest first. A run that
// vanished between the list and its load is left out; any other failure is
// returned beside the runs that loaded.
func listDeskRuns(sources []runview.Source, now time.Time, only func(runview.RunRef) bool) ([]*deskRun, []deskFailure) {
	var runs []*deskRun
	var failed []deskFailure
	for _, src := range sources {
		refs, err := src.List()
		if err != nil {
			failed = append(failed, deskFailure{fmt.Errorf("cannot list %s: %w", src.Name(), err)})
			continue
		}
		for _, ref := range refs {
			if only != nil && !only(ref) {
				continue
			}
			r, err := loadDeskRun(src, ref, now)
			if errors.Is(err, desk.ErrNotFound) {
				continue
			}
			if err != nil {
				failed = append(failed, deskFailure{fmt.Errorf("cannot load %s/%s: %w", src.Name(), ref.Name, err)})
				continue
			}
			runs = append(runs, r)
		}
	}
	slices.SortStableFunc(runs, func(a, b *deskRun) int {
		if la, lb := a.isLive(), b.isLive(); la != lb {
			if la {
				return -1
			}
			return 1
		}
		if ta, tb := a.activity(), b.activity(); !ta.Equal(tb) {
			if ta.After(tb) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ref.Name+"\x00"+a.ref.Source, b.ref.Name+"\x00"+b.ref.Source)
	})
	return runs, failed
}

// sourceFailures turns the sources that could not be built into failures.
func (s *deskSources) failures() []deskFailure {
	out := make([]deskFailure, 0, len(s.failed))
	for _, f := range s.failed {
		out = append(out, deskFailure{f.Err})
	}
	return out
}

// reportFailures writes one stderr line per failure and returns the error a
// verb exits 1 with once its result is on stdout (ADR-0008 rule 3: a
// partial result is not success).
func reportFailures(w io.Writer, failed []deskFailure) error {
	if len(failed) == 0 {
		return nil
	}
	for _, f := range failed {
		_, _ = fmt.Fprintln(w, f.String())
	}
	n := len(failed)
	return WithExitCode(fmt.Errorf("%s could not be read", plural(n, "source", "sources")), 1)
}

// joinFailures is err with every failure joined in, for a verb that fails
// before it writes its result, so stderr carries one error.
func joinFailures(err error, failed []deskFailure) error {
	if len(failed) == 0 {
		return err
	}
	errs := []error{err}
	for _, f := range failed {
		errs = append(errs, f.err)
	}
	return WithExitCode(errors.Join(errs...), ExitCode(err))
}

// --- desk runs ---

func newDeskRunsCmd(rt deskRuntime, specs *[]string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List every run from every source (no terminal needed)",
		Long: `runs lists every run from the desk and from every --spec, one row per run in
the dashboard's order: live runs first, then by last activity.

Listing the desk scans it, and a scan changes the queue: an item dropped into
pending/ by hand gets its hash fixed, and an item whose bytes changed moves
to skipped/ with reason "changed".

A source that cannot be read is named on stderr; the other sources are still
listed, and the exit is 1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			srcs := openDeskSources(rt.getenv, *specs)
			defer srcs.close()
			now := rt.now()
			runs, failed := listDeskRuns(srcs.sources, now, nil)
			failed = append(srcs.failures(), failed...)
			out := cmd.OutOrStdout()
			if asJSON {
				if err := writeDeskRunsJSON(out, runs); err != nil {
					return err
				}
			} else {
				noIcons, _ := cmd.Flags().GetBool("no-icons")
				writeDeskRunsText(out, runs, now, noIcons)
			}
			return jsonVerdict(reportFailures(cmd.ErrOrStderr(), failed), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit [{"source","name","kind","live","updated","exit","partial"}] to stdout`)
	return cmd
}

// deskRunJSON is one `desk runs --json` row. Additive changes only (ADR-0008).
type deskRunJSON struct {
	Source  string  `json:"source"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Live    string  `json:"live"`
	Updated *string `json:"updated"`
	Exit    *int    `json:"exit"`
	Partial bool    `json:"partial"`
}

func writeDeskRunsJSON(w io.Writer, runs []*deskRun) error {
	rows := make([]deskRunJSON, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, deskRunJSON{
			Source:  r.ref.Source,
			Name:    r.ref.Name,
			Kind:    string(r.ref.Kind),
			Live:    string(r.live),
			Updated: jsonTime(r.activity()),
			Exit:    r.state.Exit,
			Partial: r.delta.Partial,
		})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// jsonTime is t in RFC 3339, UTC, or nil when t is unknown.
func jsonTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// deskGlyphs is the run and step legend of docs/commands/desk.md, with its
// ASCII fallback for --no-icons.
type deskGlyphs struct{ pending, running, done, failed, stale, lost, skipped string }

func pickDeskGlyphs(noIcons bool) deskGlyphs {
	if noIcons {
		return deskGlyphs{".", "*", "+", "x", "!", "?", "-"}
	}
	return deskGlyphs{"·", "◐", "✓", "✗", "!", "?", "–"}
}

// look is a run's mark and word, as the dashboard's run row shows them.
func (g deskGlyphs) look(r *deskRun) (string, string) {
	switch r.live {
	case runview.LiveLive:
		return g.running, "live"
	case runview.LiveRunning:
		return g.running, "running"
	case runview.LiveEnded:
		switch {
		case r.state.Exit == nil:
			return g.pending, "ended"
		case *r.state.Exit == 0:
			return g.done, "ok"
		}
		return g.failed, strconv.Itoa(*r.state.Exit)
	case runview.LiveStale:
		if errors.Is(r.delta.Err, os.ErrNotExist) && len(r.delta.Events) == 0 {
			return g.pending, "new"
		}
		return g.stale, "stale"
	case runview.LiveLost:
		return g.lost, "lost"
	case runview.LiveWaiting:
		return g.pending, "waiting"
	case runview.LiveSkipped:
		return g.skipped, "skipped"
	}
	return g.pending, "pending"
}

// writeDeskRunsText prints one run row per line: mark, word, name and the
// time since the last event. A name two sources share carries @source.
func writeDeskRunsText(w io.Writer, runs []*deskRun, now time.Time, noIcons bool) {
	g := pickDeskGlyphs(noIcons)
	count := map[string]int{}
	for _, r := range runs {
		count[r.ref.Name]++
	}
	names := make([]string, len(runs))
	width := 0
	for i, r := range runs {
		names[i] = safeLabel(r.ref.Name)
		if count[r.ref.Name] > 1 {
			names[i] += "@" + safeLabel(r.ref.Source)
		}
		width = max(width, len([]rune(names[i])))
	}
	for i, r := range runs {
		mark, word := g.look(r)
		age := ""
		if a := r.activity(); !a.IsZero() {
			age = deskAge(now.Sub(a))
		}
		line := fmt.Sprintf("%s %-7s %-*s %s", mark, word, width, names[i], age)
		_, _ = fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

// deskAge is a short duration: 12s, 4m, 1h, 3d.
func deskAge(d time.Duration) string {
	switch d = max(d, 0); {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// --- desk show ---

func newDeskShowCmd(rt deskRuntime, specs *[]string) *cobra.Command {
	var asJSON bool
	var from string
	var at int
	cmd := &cobra.Command{
		Use:   "show <run>",
		Short: "Print one run's flow, or its state as JSON (no terminal needed)",
		Long: `show prints one run: its steps as plain text, or with --json its folded state
and counts. --at N replays: the state after the run's first N events.

When two sources hold a run of that name, --from names the source (a spec's
name, or "desk"); without it, show exits 2. A run no source holds exits 1.

Reading the desk scans it, and a scan changes the queue (see desk runs --help).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if cmd.Flags().Changed("at") && at < 0 {
				return WithExitCode(fmt.Errorf("--at must be 0 or more, not %d", at), 2)
			}
			srcs := openDeskSources(rt.getenv, *specs)
			defer srcs.close()
			sources := srcs.sources
			if from != "" {
				i := slices.IndexFunc(sources, func(s runview.Source) bool { return s.Name() == from })
				if i < 0 {
					return joinFailures(WithExitCode(fmt.Errorf("no source named %s", termsafe.QuoteArgMax(from, 0)), 2), srcs.failures())
				}
				sources = sources[i : i+1]
			}
			now := rt.now()
			runs, failed := listDeskRuns(sources, now, func(ref runview.RunRef) bool { return ref.Name == name })
			failed = append(srcs.failures(), failed...)
			switch len(runs) {
			case 0:
				return joinFailures(WithExitCode(fmt.Errorf("no run named %s", termsafe.QuoteArgMax(name, 0)), 1), failed)
			case 1:
			default:
				// In the order the sources were given: the desk, then each spec.
				var in []string
				for _, src := range sources {
					if slices.ContainsFunc(runs, func(r *deskRun) bool { return r.src == src }) {
						in = append(in, safeLabel(src.Name()))
					}
				}
				return joinFailures(WithExitCode(fmt.Errorf("run %s is in sources %s; pass --from", termsafe.QuoteArgMax(name, 0), joinAnd(in)), 2), failed)
			}
			r := runs[0]
			events := r.folder.Len()
			if cmd.Flags().Changed("at") {
				events = min(at, r.folder.Len())
				r.at(events, now)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if err := writeDeskShowJSON(out, r, events); err != nil {
					return err
				}
			} else {
				noIcons, _ := cmd.Flags().GetBool("no-icons")
				writeDeskShowText(out, r, events, cmd.Flags().Changed("at"), noIcons)
			}
			return jsonVerdict(reportFailures(cmd.ErrOrStderr(), failed), asJSON)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", `the source that holds the run: a spec's name, or "desk"`)
	cmd.Flags().IntVar(&at, "at", 0, "replay: show the state after the run's first N events")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"source","name","live","exit","exit_meaning","steps","edges","checks","restarts","last_event","counts","partial"} to stdout`)
	return cmd
}

// joinAnd is "a", "a and b", or "a, b and c".
func joinAnd(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// deskShowJSON is the `desk show --json` shape. Additive changes only
// (ADR-0008).
type deskShowJSON struct {
	Source      string             `json:"source"`
	Name        string             `json:"name"`
	Live        string             `json:"live"`
	Exit        *int               `json:"exit"`
	ExitMeaning string             `json:"exit_meaning"`
	Steps       []deskStepJSON     `json:"steps"`
	Edges       [][2]string        `json:"edges"`
	Checks      []deskCheckJSON    `json:"checks"`
	Restarts    []*string          `json:"restarts"`
	LastEvent   *string            `json:"last_event"`
	Counts      deskShowCountsJSON `json:"counts"`
	Partial     bool               `json:"partial"`
}

type deskStepJSON struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"`
	Attempt int     `json:"attempt"`
	Rejects int     `json:"rejects"`
	Resumed bool    `json:"resumed"`
	Start   *string `json:"start"`
	End     *string `json:"end"`
}

type deskCheckJSON struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Seen   bool   `json:"seen"`
	Passed bool   `json:"passed"`
}

type deskShowCountsJSON struct {
	Events       int `json:"events"`
	Dropped      int `json:"dropped"`
	UnknownSteps int `json:"unknown_steps"`
	BadExits     int `json:"bad_exits"`
}

func writeDeskShowJSON(w io.Writer, r *deskRun, events int) error {
	s := r.state
	out := deskShowJSON{
		Source:      r.ref.Source,
		Name:        r.ref.Name,
		Live:        string(r.live),
		Exit:        s.Exit,
		ExitMeaning: s.ExitMeaning,
		Steps:       make([]deskStepJSON, 0, len(s.Steps)),
		Edges:       make([][2]string, 0, len(s.Edges)),
		Checks:      make([]deskCheckJSON, 0, len(s.Checks)),
		Restarts:    make([]*string, 0, len(s.Restarts)),
		LastEvent:   jsonTime(s.LastEvent),
		Counts: deskShowCountsJSON{
			Events:       events,
			Dropped:      r.delta.Dropped,
			UnknownSteps: s.UnknownSteps,
			BadExits:     s.BadExits,
		},
		Partial: r.delta.Partial,
	}
	for _, st := range s.Steps {
		out.Steps = append(out.Steps, deskStepJSON{
			ID: st.ID, Status: string(st.Status), Attempt: st.Attempt, Rejects: st.Rejects,
			Resumed: st.Resumed, Start: jsonTime(st.Start), End: jsonTime(st.End),
		})
	}
	out.Edges = append(out.Edges, s.Edges...)
	for _, c := range s.Checks {
		out.Checks = append(out.Checks, deskCheckJSON{ID: c.ID, Label: c.Label, Seen: c.Seen, Passed: c.Passed})
	}
	for _, t := range s.Restarts {
		out.Restarts = append(out.Restarts, jsonTime(t))
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// writeDeskShowText prints the compact flow: a header line, one line per
// step, the checks, and what was left out.
func writeDeskShowText(w io.Writer, r *deskRun, events int, replay bool, noIcons bool) {
	g := pickDeskGlyphs(noIcons)
	s := r.state
	_, word := g.look(r)
	if r.live == runview.LiveEnded {
		word = "ended"
	}
	head := safeLabel(r.ref.Source) + "/" + safeLabel(r.ref.Name) + " · " + word
	if s.Exit != nil {
		head += " · exit " + strconv.Itoa(*s.Exit)
	}
	if s.ExitMeaning != "" {
		head += " (" + safeText(s.ExitMeaning) + ")"
	}
	if replay {
		head += fmt.Sprintf(" · replay %d/%d", events, r.folder.Len())
	}
	_, _ = fmt.Fprintln(w, head)

	idW := 0
	for _, st := range s.Steps {
		idW = max(idW, len([]rune(safeLabel(st.ID))))
	}
	after := stepPredecessors(s)
	for i, st := range s.Steps {
		parts := []string{string(st.Status)}
		if st.Attempt > 0 {
			parts = append(parts, "attempt "+strconv.Itoa(st.Attempt))
		}
		if m := r.spec.Gate.MaxRejects; st.Rejects > 0 && m > 0 {
			parts = append(parts, fmt.Sprintf("rej %d/%d", st.Rejects, m))
		} else if st.Rejects > 0 {
			parts = append(parts, fmt.Sprintf("rej %d", st.Rejects))
		}
		if st.Resumed {
			parts = append(parts, "resumed")
		}
		if !st.Start.IsZero() && !st.End.IsZero() {
			parts = append(parts, st.End.Sub(st.Start).Round(time.Second).String())
		}
		if p := after[st.ID]; !isChainLink(s, i, p) {
			safe := make([]string, len(p))
			for k, id := range p {
				safe[k] = safeLabel(id)
			}
			parts = append(parts, "after "+strings.Join(safe, ", "))
		}
		_, _ = fmt.Fprintf(w, "  %s %-*s  %s\n", g.stepMark(st.Status), idW, safeLabel(st.ID), strings.Join(parts, " · "))
	}
	for _, c := range s.Checks {
		label := c.Label
		if label == "" {
			label = c.ID
		}
		verdict := "not seen"
		if c.Seen {
			verdict = "failed"
			if c.Passed {
				verdict = "passed"
			}
		}
		_, _ = fmt.Fprintf(w, "  check %s: %s\n", safeTitle(label), verdict)
	}
	foot := []string{plural(events, "event", "events")}
	for _, c := range []struct {
		n         int
		one, many string
	}{
		{r.delta.Dropped, "line dropped", "lines dropped"},
		{r.delta.DroppedFields, "field dropped", "fields dropped"},
		{s.UnknownSteps, "unknown step", "unknown steps"},
		{s.BadExits, "bad exit", "bad exits"},
	} {
		if c.n > 0 {
			foot = append(foot, plural(c.n, c.one, c.many))
		}
	}
	if r.delta.Partial {
		foot = append(foot, "partial: over 32 MiB cap")
	}
	if r.delta.Err != nil {
		foot = append(foot, "note: "+safeText(r.delta.Err.Error()))
	}
	_, _ = fmt.Fprintln(w, strings.Join(foot, " · "))
}

func (g deskGlyphs) stepMark(st runview.StepStatus) string {
	switch st {
	case runview.StepRunning:
		return g.running
	case runview.StepClosed:
		return g.done
	case runview.StepFailed:
		return g.failed
	}
	return g.pending
}

// stepPredecessors maps each step to the steps it follows.
func stepPredecessors(s runview.RunState) map[string][]string {
	out := map[string][]string{}
	for _, e := range s.Edges {
		out[e[1]] = append(out[e[1]], e[0])
	}
	return out
}

// isChainLink reports whether the steps before step i are exactly the one
// listed before it (or none, for the first), so "after" adds nothing.
func isChainLink(s runview.RunState, i int, before []string) bool {
	if i == 0 {
		return len(before) == 0
	}
	return len(before) == 1 && before[0] == s.Steps[i-1].ID
}
