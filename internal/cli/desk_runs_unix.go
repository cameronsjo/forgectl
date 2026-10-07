// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// loadedRun is one run read in full for `desk runs` and `desk show`.
type loadedRun struct {
	ref    runview.RunRef
	spec   *runview.Spec
	delta  runview.Delta
	folder *runview.Folder
	state  runview.RunState
	live   runview.LiveState
	at     int // events folded into state
}

// loadRun reads ref from the start and folds every event. One Load from a
// zero cursor reads everything the caps allow.
func loadRun(src runview.Source, ref runview.RunRef) (*loadedRun, error) {
	d, err := src.Load(ref, &runview.Cursor{})
	if err != nil {
		return nil, err
	}
	spec := runview.SpecOf(src, ref)
	r := &loadedRun{ref: ref, spec: spec, delta: d, folder: runview.NewFolder(spec, d.Defs)}
	r.folder.Append(d.Events...)
	r.foldTo(r.folder.Len(), false)
	return r, nil
}

// foldTo folds the run to its first i events. The run's live state is its
// source's word (the desk's own state for a desk run) unless this is a
// replay, which shows the fold alone: the desk's word is the run's state
// now, not at event i.
func (r *loadedRun) foldTo(i int, replay bool) {
	r.state = r.folder.At(i)
	r.at = min(max(i, 0), r.folder.Len())
	r.live = r.state.Live
	// A log has no step model, so the fold cannot know it ended: it stays
	// unknown in a replay too.
	if r.delta.Live != "" && (!replay || r.delta.Live == runview.LiveUnknown) {
		r.live = r.delta.Live
	}
}

func (r *loadedRun) isLive() bool {
	return r.live == runview.LiveLive || r.live == runview.LiveRunning
}

// activity is when the run last did anything: its last event, else the time
// its source last saw it change.
func (r *loadedRun) activity() time.Time {
	if !r.state.LastEvent.IsZero() {
		return r.state.LastEvent
	}
	return r.ref.Updated
}

// progress counts the run's steps: total, done and failed.
func (r *loadedRun) progress() (total, done, failed int) {
	for _, st := range r.state.Steps {
		switch st.Status {
		case runview.StepClosed:
			done++
		case runview.StepFailed:
			failed++
		}
	}
	return len(r.state.Steps), done, failed
}

// deskRunSources opens the sources `desk runs` and `desk show` read: the
// desk unless skipDesk, and the log when one is named. A source that cannot
// be opened is returned as a failure beside the ones that could.
func deskRunSources(cmd *cobra.Command, deps module.Deps, dirFlag string, log deskLogOpts, skipDesk bool) (srcs []runview.Source, closeAll func(), failed []error) {
	closeAll = func() {}
	if !skipDesk {
		d, err := openDeskDirFor(cmd, deps, dirFlag)
		if err != nil {
			failed = append(failed, err)
		} else {
			srcs = append(srcs, runview.NewDeskSource(d))
			closeAll = func() { _ = d.Close() }
		}
	}
	if log.path != "" {
		src, err := openLogSource(log)
		if err != nil {
			failed = append(failed, err)
		} else {
			srcs = append(srcs, src)
		}
	}
	return srcs, closeAll, failed
}

func openLogSource(log deskLogOpts) (runview.Source, error) {
	abs, err := filepath.Abs(log.path)
	if err != nil {
		return nil, fmt.Errorf("desk: --log: %w", err)
	}
	if log.lens != "" {
		lens, err := loadLens(log.lens)
		if err != nil {
			return nil, err
		}
		return runview.NewLensSource(abs, lens)
	}
	return runview.NewLogSource(abs, log.keys())
}

// loadLens reads the lens --lens names: a path when it has a slash or ends in
// .toml, else NAME.toml in the lenses directory, else the built-in events
// lens for "events".
func loadLens(name string) (*runview.Lens, error) {
	path, err := lensPath(name)
	if err != nil {
		return nil, err
	}
	lens, err := runview.LoadLens(path)
	if errors.Is(err, fs.ErrNotExist) && name == runview.EventsLensName {
		return runview.EventsLens(), nil // built in; a file of that name wins
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, WithExitCode(fmt.Errorf("desk: no lens %s at %s; write one there, or see forgectl desk lens --help", safeLabel(name), safeText(path)), deskExitUsage)
	}
	if err != nil {
		return nil, WithExitCode(fmt.Errorf("desk: %w", err), deskExitUsage)
	}
	return lens, nil
}

func lensPath(name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) || strings.HasSuffix(name, ".toml") {
		return filepath.Abs(name)
	}
	if name == "." || name == ".." {
		return "", deskUsage("desk: --lens %s: give a lens name or a .toml file", safeLabel(name))
	}
	dir, err := config.LensesDir()
	if err != nil {
		return "", fmt.Errorf("desk: lenses directory: %w", err)
	}
	return filepath.Join(dir, name+".toml"), nil
}

// reportRunFailures writes one stderr line per failure and returns the error
// a verb exits 1 with once its result is on stdout: a partial result is not
// success (ADR-0008).
func reportRunFailures(w io.Writer, failed []error) error {
	if len(failed) == 0 {
		return nil
	}
	for _, err := range failed {
		_, _ = fmt.Fprintln(w, safeText(err.Error()))
	}
	return WithExitCode(fmt.Errorf("desk: %s could not be read", plural(len(failed), "source", "sources")), 1)
}

func runDeskRuns(cmd *cobra.Command, deps module.Deps, dirFlag string, log deskLogOpts, asJSON bool) error {
	srcs, closeAll, failed := deskRunSources(cmd, deps, dirFlag, log, false)
	defer closeAll()
	var runs []*loadedRun
	for _, src := range srcs {
		refs, err := src.List()
		if err != nil {
			failed = append(failed, fmt.Errorf("desk: cannot list %s: %w", src.Name(), err))
			continue
		}
		for _, ref := range refs {
			r, err := loadRun(src, ref)
			if errors.Is(err, desk.ErrNotFound) {
				continue // gone between the list and the load
			}
			if err != nil {
				failed = append(failed, fmt.Errorf("desk: cannot load %s/%s: %w", src.Name(), ref.Name, err))
				continue
			}
			if err := r.readInPart(); err != nil {
				failed = append(failed, fmt.Errorf("desk: %s/%s was read in part: %w", src.Name(), ref.Name, err))
			}
			runs = append(runs, r)
		}
	}
	slices.SortStableFunc(runs, func(a, b *loadedRun) int {
		return runview.CompareRuns(
			runview.RunOrder{Live: a.isLive(), Activity: a.activity(), Name: a.ref.Name, Source: a.ref.Source},
			runview.RunOrder{Live: b.isLive(), Activity: b.activity(), Name: b.ref.Name, Source: b.ref.Source})
	})
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, runsJSON(runs)); err != nil {
			return err
		}
	} else {
		noIcons, _ := cmd.Flags().GetBool("no-icons")
		if err := writeRunsText(out, runs, deskNow().UTC(), noIcons); err != nil {
			return err
		}
	}
	return jsonVerdict(reportRunFailures(cmd.ErrOrStderr(), failed), asJSON)
}

// deskRunJSON is one `desk runs --json` row. Additive changes only (ADR-0008).
type deskRunJSON struct {
	Source  string     `json:"source"`
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Live    string     `json:"live"`
	Exit    *int       `json:"exit"`
	Steps   int        `json:"steps"`
	Done    int        `json:"done"`
	Failed  int        `json:"failed"`
	Events  int        `json:"events"`
	Updated *time.Time `json:"updated"`
	Partial bool       `json:"partial"`
}

func runsJSON(runs []*loadedRun) []deskRunJSON {
	rows := make([]deskRunJSON, 0, len(runs))
	for _, r := range runs {
		total, done, failed := r.progress()
		rows = append(rows, deskRunJSON{
			Source: r.ref.Source, Name: r.ref.Name, Kind: string(r.ref.Kind), Live: string(r.live),
			Exit: r.state.Exit, Steps: total, Done: done, Failed: failed, Events: r.folder.Len(),
			Updated: utcPtr(r.activity()), Partial: r.delta.Partial,
		})
	}
	return rows
}

// utcPtr is t in UTC, or nil when t is unknown.
func utcPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// writeRunsText prints one row per run: mark, word, name, steps done of
// total, failed steps, and the time since the run last did anything.
func writeRunsText(out io.Writer, runs []*loadedRun, now time.Time, noIcons bool) error {
	w := &stickyWriter{w: out}
	if len(runs) == 0 {
		w.printf("no runs\n")
		return w.err
	}
	g := runview.PickGlyphs(noIcons)
	names := make([]string, len(runs))
	width := 0
	for i, r := range runs {
		names[i] = safeLabel(r.ref.Name)
		if r.ref.Kind == runview.KindLog {
			names[i] = "log:" + names[i]
		}
		width = max(width, len([]rune(names[i])))
	}
	for i, r := range runs {
		mk := runview.RunMark(g, r.live, r.state.Exit)
		parts := []string{fmt.Sprintf("%s %-8s %-*s", mk.Glyph, mk.Word, width, names[i])}
		if total, done, failed := r.progress(); total > 0 {
			p := fmt.Sprintf("%d/%d steps", done, total)
			if failed > 0 {
				p += fmt.Sprintf(", %d failed", failed)
			}
			parts = append(parts, p)
		} else {
			parts = append(parts, plural(r.folder.Len(), "event", "events"))
		}
		if a := r.activity(); !a.IsZero() {
			parts = append(parts, shortAge(int64(now.Sub(a)/time.Second)))
		}
		w.printf("%s\n", strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	return w.err
}

func runDeskShow(cmd *cobra.Command, deps module.Deps, dirFlag, name string, log deskLogOpts, o deskShowOpts) error {
	if name != "" {
		if err := checkDeskName(name); err != nil {
			return err
		}
	}
	if o.live && !deskHasTerminal() {
		return WithExitCode(tui.ErrRunWatchNeedsTerminal, deskExitUsage)
	}
	srcs, closeAll, failed := deskRunSources(cmd, deps, dirFlag, log, name == "")
	defer closeAll()
	if len(failed) > 0 {
		return errors.Join(failed...)
	}
	src := srcs[0]
	ref := runview.RunRef{Source: src.Name(), Name: name, Kind: runview.KindDesk}
	if name == "" {
		refs, err := src.List()
		if err != nil {
			return err
		}
		ref = refs[0]
	}
	if name != "" {
		// A name taken from a JSON path carries the file's extension.
		ref.Name = resolveName(name, func(n string) bool {
			probe := ref
			probe.Name = n
			_, err := loadRun(src, probe)
			return !errors.Is(err, desk.ErrNotFound)
		})
	}
	r, err := loadRun(src, ref)
	if errors.Is(err, desk.ErrNotFound) {
		return deskNotFound("desk show", "run", name, "runs", knownRunNames(dirFlag))
	}
	if err != nil {
		return err
	}
	if o.live {
		err := tui.RunWatch(cmd.Context(), src, tui.RunWatchOptions{Theme: deps.Theme, Name: r.ref.Name, ASCII: deskNoIcons(cmd, deps)})
		if errors.Is(err, tui.ErrRunWatchNoRun) {
			return WithExitCode(err, 1)
		}
		return err
	}
	if o.at >= 0 {
		r.foldTo(o.at, true)
	}
	out := cmd.OutOrStdout()
	if o.asJSON {
		if err := writeJSON(out, showJSON(r, o.at >= 0)); err != nil {
			return err
		}
	} else {
		noIcons, _ := cmd.Flags().GetBool("no-icons")
		if err := writeShowText(out, r, o, noIcons); err != nil {
			return err
		}
	}
	// A run read in part is shown, then exits 1: a partial result is not
	// success (ADR-0008). The note is in the output already.
	if err := r.readInPart(); err != nil {
		return jsonVerdict(WithExitCode(fmt.Errorf("desk show: %s was read in part: %w", safeLabel(r.ref.Name), err), 1), o.asJSON)
	}
	return nil
}

// deskShowJSON is the `desk show --json` shape. Additive changes only
// (ADR-0008). At is the replay point, null without --at; Events holds the
// events folded into the state shown.
type deskShowJSON struct {
	Source      string           `json:"source"`
	Name        string           `json:"name"`
	Kind        string           `json:"kind"`
	Live        string           `json:"live"`
	Exit        *int             `json:"exit"`
	At          *int             `json:"at"`
	EventsTotal int              `json:"events_total"`
	Steps       []runStepJSON    `json:"steps"`
	Edges       [][2]string      `json:"edges"`
	Events      []runEventJSON   `json:"events"`
	Counts      runShowCountJSON `json:"counts"`
	Partial     bool             `json:"partial"`
	Held        bool             `json:"held"`
	Note        string           `json:"note,omitempty"`
}

type runStepJSON struct {
	ID              string     `json:"id"`
	Status          string     `json:"status"`
	RunnerState     string     `json:"runner_state,omitempty"`
	After           []string   `json:"after"`
	Start           *time.Time `json:"start"`
	End             *time.Time `json:"end"`
	DurationSeconds *float64   `json:"duration_seconds"`
}

type runEventJSON struct {
	Seq    int               `json:"seq"`
	Time   *time.Time        `json:"time"`
	Name   string            `json:"name"`
	Step   string            `json:"step,omitempty"`
	Fields map[string]string `json:"fields"`
}

type runShowCountJSON struct {
	Dropped       int `json:"dropped"`
	Ignored       int `json:"ignored"`
	DroppedFields int `json:"dropped_fields"`
	UnknownSteps  int `json:"unknown_steps"`
	BadExits      int `json:"bad_exits"`
}

func showJSON(r *loadedRun, replay bool) deskShowJSON {
	s := r.state
	out := deskShowJSON{
		Source: r.ref.Source, Name: r.ref.Name, Kind: string(r.ref.Kind), Live: string(r.live), Exit: s.Exit,
		EventsTotal: r.folder.Len(),
		Steps:       make([]runStepJSON, 0, len(s.Steps)),
		Edges:       append(make([][2]string, 0, len(s.Edges)), s.Edges...),
		Events:      make([]runEventJSON, 0, r.at),
		Counts: runShowCountJSON{
			Dropped: r.delta.Dropped, Ignored: r.delta.Ignored, DroppedFields: r.delta.DroppedFields,
			UnknownSteps: s.UnknownSteps, BadExits: s.BadExits,
		},
		Partial: r.delta.Partial,
		Held:    r.delta.Held,
	}
	if replay {
		at := r.at
		out.At = &at
	}
	if r.delta.Err != nil {
		out.Note = r.delta.Err.Error()
	}
	after := predecessors(s)
	for _, st := range s.Steps {
		js := runStepJSON{
			ID: st.ID, Status: string(st.Status), RunnerState: runview.RunnerState(r.delta.Timing, st.ID),
			After: append([]string{}, after[st.ID]...), Start: utcPtr(st.Start), End: utcPtr(st.End),
		}
		if replay {
			js.RunnerState = "" // the runner's word is the step's state now, not at event i
		}
		if d := runview.StepDur(r.delta.Timing, st); d > 0 && !replay {
			secs := d.Seconds()
			js.DurationSeconds = &secs
		}
		out.Steps = append(out.Steps, js)
	}
	for _, e := range r.folder.Events()[:r.at] {
		fields := make(map[string]string, len(e.Fields))
		for _, f := range e.Fields {
			if _, dup := fields[f.Key]; !dup {
				fields[f.Key] = f.Value
			}
		}
		out.Events = append(out.Events, runEventJSON{Seq: e.Seq, Time: utcPtr(e.Time), Name: e.Name, Step: e.Step, Fields: fields})
	}
	return out
}

// writeShowText prints the run's flow: a header line, one line per step, the
// events with --events, a count line, and where the item's record is.
func writeShowText(out io.Writer, r *loadedRun, o deskShowOpts, noIcons bool) error {
	w := &stickyWriter{w: out}
	g := runview.PickGlyphs(noIcons)
	s := r.state
	replay := o.at >= 0
	mk := runview.RunMark(g, r.live, s.Exit)
	head := safeLabel(r.ref.Source) + "/" + safeLabel(r.ref.Name) + " · " + mk.Word
	if replay {
		head += fmt.Sprintf(" · replay %d/%d", r.at, r.folder.Len())
	}
	w.printf("%s\n", head)
	now := deskNow()
	if replay {
		now = time.Time{}
	}
	if g := runview.Gist(s, r.live, r.folder.Events()[:r.at], now); g != "" {
		w.printf("%s\n", safeText(g))
	}

	idW := 0
	for _, st := range s.Steps {
		idW = max(idW, len([]rune(safeLabel(st.ID))))
	}
	after := predecessors(s)
	for i, st := range s.Steps {
		runner := ""
		if !replay {
			runner = runview.RunnerState(r.delta.Timing, st.ID)
		}
		smk := runview.StepMark(g, st.Status, runner)
		parts := []string{smk.Word}
		if d := runview.StepDur(r.delta.Timing, st); d > 0 && !replay {
			parts = append(parts, d.Round(100*time.Millisecond).String())
		}
		if p := after[st.ID]; len(p) > 0 && !isChainLink(s, i, p) {
			safe := make([]string, len(p))
			for k, id := range p {
				safe[k] = safeLabel(id)
			}
			parts = append(parts, "after "+strings.Join(safe, ", "))
		}
		w.printf("  %s %-*s  %s\n", smk.Glyph, idW, safeLabel(st.ID), strings.Join(parts, " · "))
	}
	if len(s.Steps) == 0 && r.ref.Kind == runview.KindLog {
		if r.spec.ActionField != "" {
			w.printf("  (no step has started: --events shows the timeline, and forgectl desk lens check shows which rules matched)\n")
		} else {
			w.printf("  (a log has no step model: --events shows its timeline, and a --lens gives it one)\n")
		}
	}

	if o.events {
		for _, e := range r.folder.Events()[:r.at] {
			w.printf("%s\n", eventLine(e))
		}
	}

	foot := []string{plural(r.at, "event", "events")}
	for _, c := range []struct {
		n         int
		one, many string
	}{
		{r.delta.Dropped, "line dropped", "lines dropped"},
		{r.delta.Ignored, "line ignored", "lines ignored"},
		{r.delta.DroppedFields, "field dropped", "fields dropped"},
		{s.UnknownSteps, "unknown step", "unknown steps"},
		{s.BadExits, "bad exit", "bad exits"},
	} {
		if c.n > 0 {
			foot = append(foot, plural(c.n, c.one, c.many))
		}
	}
	if r.delta.Partial {
		foot = append(foot, "partial: past the 32 MiB cap")
	}
	if r.delta.Held {
		foot = append(foot, "last line has no newline yet: not read")
	}
	if r.delta.Err != nil {
		foot = append(foot, "note: "+safeText(r.delta.Err.Error()))
	}
	w.printf("%s\n", strings.Join(foot, " · "))
	if r.ref.Kind == runview.KindDesk {
		w.printf("record: forgectl desk status %s\n", safeLabel(r.ref.Name))
	}
	return w.err
}

// eventLine renders one event as a timeline row: its number, time when it
// has one, name, step, then its other fields. Every value was cleaned at
// ingest; safeText caps the line.
func eventLine(e runview.Event) string {
	parts := []string{fmt.Sprintf("  #%-4d", e.Seq)}
	if !e.Time.IsZero() {
		parts = append(parts, e.Time.UTC().Format(time.RFC3339))
	}
	parts = append(parts, e.Name)
	if e.Step != "" {
		parts = append(parts, "step="+e.Step)
	}
	for _, f := range e.Fields {
		if f.Key == "id" && e.Step != "" {
			continue
		}
		parts = append(parts, f.Key+"="+strconv.Quote(f.Value))
	}
	return safeText(strings.Join(parts, " "))
}

// predecessors maps each step to the steps it follows.
func predecessors(s runview.RunState) map[string][]string {
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

// errPastCap is a log larger than the read cap: the part past it was not read.
var errPastCap = errors.New("past the 32 MiB cap")

// readInPart is why the run was read only in part, or nil: a read error, or a
// log past the cap. Either is a partial result, which exits 1 (ADR-0008).
func (r *loadedRun) readInPart() error {
	switch {
	case r.delta.Err != nil:
		return r.delta.Err
	case r.delta.Partial:
		return errPastCap
	}
	return nil
}

// knownRunNames lists the runs in the desk, to decorate a not-found error. A
// desk that cannot be opened yields none: the error is already being reported.
func knownRunNames(dirFlag string) []string {
	d, err := openDeskDir(dirFlag)
	if err != nil {
		return nil
	}
	defer d.Close() //nolint:errcheck // read side
	return runNames(d)
}
