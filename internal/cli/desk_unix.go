// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// Unix-only seams: whether the process has a terminal on stdin and stdout
// (for the dashboard), and on stdin alone (for `add -`).
var (
	deskHasTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	}
	deskStdinIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	deskStdin           io.Reader = os.Stdin
)

const (
	// deskFrameWidth and deskFrameHeight are --frame's size when $COLUMNS or
	// $LINES is unset or not a positive number.
	deskFrameWidth  = 80
	deskFrameHeight = 40
	// deskStatusDone is how many done items the text status lists.
	deskStatusDone = 20
	// deskShortSHA is how much of a hash the text status shows. Hashes are
	// never compared short; this is display only.
	deskShortSHA = 12
	// deskMaxItemBytes mirrors the desk's own 1 MiB item cap for stdin.
	deskMaxItemBytes = 1 << 20
	// deskWhatCols caps the WHAT text on a status line.
	deskWhatCols = 72
)

// openDeskDir resolves and opens the desk.
func openDeskDir(flag string) (*desk.Desk, error) {
	dir, err := resolveDeskDir(flag)
	if err != nil {
		return nil, err
	}
	return desk.Open(dir)
}

// openDeskDirFor opens the desk and attaches the operator-signal clearing, so
// every verb that can take an item out of pending/ (a run, a skip, a Scan that
// skips a changed item) keeps the pane state in step.
func openDeskDirFor(cmd *cobra.Command, deps module.Deps, flag string) (*desk.Desk, error) {
	d, err := openDeskDir(flag)
	if err != nil {
		return nil, err
	}
	newDeskSignal(deps).attach(d)
	return d, nil
}

func runDeskDashboard(cmd *cobra.Command, deps module.Deps, dirFlag string, frame bool) error {
	if !frame && !deskHasTerminal() {
		return WithExitCode(tui.ErrDeskNeedsTerminal, deskExitUsage)
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return err
	}
	defer d.Close()           //nolint:errcheck // read side; nothing to flush
	home, _ := deskUserHome() // display only: no home shows the full path
	ascii := deskNoIcons(cmd, deps)
	if frame {
		return printDeskFrame(deps.Theme.Writer(cmd.OutOrStdout(), os.Environ()), d, deps, home, ascii)
	}
	return tui.RunDesk(cmd.Context(), d, tui.DeskOptions{Version: meta.Version, Home: home, Theme: deps.Theme, ASCII: ascii})
}

// deskNoIcons reports whether the desk draws in ASCII: the root's
// --no-icons flag, or no_icons in the config, as the hub reads them.
func deskNoIcons(cmd *cobra.Command, deps module.Deps) bool {
	flag, err := cmd.Flags().GetBool("no-icons")
	return deps.Cfg.NoIcons || (err == nil && flag)
}

// printDeskFrame draws one frame the way the dashboard would, sized by
// $COLUMNS and $LINES. out is the theme's writer, so NO_COLOR and a pipe
// both drop the colour.
func printDeskFrame(out io.Writer, d *desk.Desk, deps module.Deps, home string, ascii bool) error {
	snap, err := d.Scan()
	if err != nil {
		return err
	}
	opts := tui.DeskFrameOptions{
		Version: meta.Version,
		Dir:     tui.TildePath(d.Path(), home),
		Steps:   map[string][]desk.StepStatus{},
		Records: map[string][]byte{},
		ASCII:   ascii,
	}
	if h, err := os.Hostname(); err == nil {
		opts.Host = h
	}
	th := deps.Theme
	opts.Theme = &th
	for _, it := range snap.Running {
		if data, _, err := d.Record(it.Name); err == nil {
			opts.Records[it.Name] = data
		}
		if it.Kind == desk.KindBatch {
			if s, err := d.BatchStatus(it.Name); err == nil {
				opts.Steps[it.Name] = s
			}
		}
	}
	width := envSize("COLUMNS", deskFrameWidth)
	height := envSize("LINES", deskFrameHeight)
	frame := tui.RenderDeskFrame(snap, width, height, deskNow(), opts)
	_, err = io.WriteString(out, frame+"\n")
	return err
}

// envSize reads a positive int from key, else def.
func envSize(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(deskGetenv(key))); err == nil && n > 0 && n <= 1000 {
		return n
	}
	return def
}

// deskAddJSON is `desk add --json`.
type deskAddJSON struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	SHA256   string   `json:"sha256"`
	Path     string   `json:"path"`
	Warnings []string `json:"warnings"`
	// Duplicate is true when an identical item was already waiting: nothing
	// was queued, and the other fields describe that item.
	Duplicate bool `json:"duplicate"`
}

func runDeskAdd(cmd *cobra.Command, deps module.Deps, dirFlag, file string, o deskAddOpts) error {
	if err := checkAddFlags(file, o); err != nil {
		return err
	}
	src := file
	if file == "-" {
		if deskStdinIsTerminal() {
			return deskUsage("desk add: stdin is a terminal; pipe the item in, or pass a file")
		}
		tmp, cleanup, err := stageStdin(o.name)
		if err != nil {
			return err
		}
		defer cleanup()
		src = tmp
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return err
	}
	defer d.Close()            //nolint:errcheck // Add has already synced what it wrote
	sig := newDeskSignal(deps) // d already clears signals: openDeskDirFor attached them
	if pane, ok := sig.pane(); ok {
		d.SetSignalPane(pane)
	}
	what, why := strings.TrimSpace(o.what), strings.TrimSpace(o.why)
	var (
		a         desk.Added
		duplicate bool
	)
	if o.allowDuplicate {
		a, err = d.Add(src, what, why, o.tty)
	} else {
		a, duplicate, err = d.AddUnique(src, what, why, o.tty)
	}
	if err != nil {
		return err
	}
	// The item is queued; a signal that fails is a warning, not a failed add.
	// A duplicate queued nothing, so it signals only when the first attempt
	// never finished signalling (it died after queueing, or a signal failed):
	// the item would otherwise wait with nobody told.
	signalNow := !duplicate || !d.Signalled(a.Name)
	var signalFailures []string
	if signalNow {
		signalFailures = sig.queued(cmd.Context(), d, a, what)
	}
	warnings := a.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	for _, f := range signalFailures {
		warnings = append(warnings, "operator signal failed: "+f)
	}
	if signalNow && len(signalFailures) == 0 {
		// Recorded only when every enabled signal went out, so a failed one
		// is retried by the next add of the same file.
		if err := d.MarkSignalled(a.Name); err != nil {
			warnings = append(warnings, "could not record that the operator was signalled: "+termsafe.SafeLineMax(err.Error(), deskWhatCols))
		}
	}
	out := cmd.OutOrStdout()
	if o.asJSON {
		return writeJSON(out, deskAddJSON{Name: a.Name, Kind: string(a.Kind), SHA256: a.SHA256, Path: a.Path, Warnings: warnings, Duplicate: duplicate})
	}
	ew := &stickyWriter{w: cmd.ErrOrStderr()}
	for _, w := range warnings {
		ew.printf("warning: %s\n", safeText(w))
	}
	w := &stickyWriter{w: out}
	w.printf("name=%s\nkind=%s\nsha256=%s\nduplicate=%t\n", a.Name, a.Kind, a.SHA256, duplicate)
	if duplicate {
		switch {
		case !signalNow:
			ew.printf("note: %s is already waiting with this sha256; nothing was queued (--allow-duplicate queues another)\n", safeText(a.Name))
		case len(signalFailures) == 0 && !sig.enabled():
			ew.printf("note: %s is already waiting with this sha256; nothing was queued, and no operator signal is enabled, so none was sent (--allow-duplicate queues another)\n", safeText(a.Name))
		case len(signalFailures) == 0:
			ew.printf("note: %s is already waiting with this sha256; nothing was queued, and the operator signal had not gone out, so it was sent now (--allow-duplicate queues another)\n", safeText(a.Name))
		default:
			// The failure itself is the warning line above, once; the note
			// only says what it means for this retry.
			ew.printf("note: %s already queued; signal not sent (the warning above says why; --allow-duplicate queues another)\n", safeText(a.Name))
		}
	}
	return errors.Join(w.err, ew.err)
}

// stageStdin copies stdin, capped, into a private temp dir under name, so
// Add reads it like any file.
func stageStdin(name string) (string, func(), error) {
	data, err := io.ReadAll(io.LimitReader(deskStdin, deskMaxItemBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("desk add: read stdin: %w", err)
	}
	if len(data) > deskMaxItemBytes {
		return "", nil, errors.New("desk add: stdin is larger than 1 MiB")
	}
	dir, err := os.MkdirTemp("", "forgectl-desk-add-")
	if err != nil {
		return "", nil, fmt.Errorf("desk add: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("desk add: %w", err)
	}
	return p, cleanup, nil
}

// deskPlanJSON is `desk plan --json`.
type deskPlanJSON struct {
	Name     string         `json:"name"`
	SHA256   string         `json:"sha256"`
	Steps    []deskPlanStep `json:"steps"`
	Waves    [][]string     `json:"waves"`
	Order    string         `json:"order"`
	Warnings []string       `json:"warnings"`
}

type deskPlanStep struct {
	ID      string   `json:"id"`
	After   []string `json:"after"`
	Timeout int      `json:"timeout"`
	Private bool     `json:"private"`
}

func runDeskPlan(cmd *cobra.Command, deps module.Deps, dirFlag, target string, asJSON bool) error {
	label, data, err := readPlanTarget(cmd, deps, dirFlag, target)
	if err != nil {
		return err
	}
	m, err := desk.LoadManifest(data, label)
	if err != nil {
		return err
	}
	waves := m.Waves()
	warnings := m.Lint()
	if warnings == nil {
		warnings = []string{}
	}
	out := cmd.OutOrStdout()
	if asJSON {
		res := deskPlanJSON{Name: label, SHA256: m.SHA256, Waves: waves, Order: desk.OrderLine(waves), Warnings: warnings, Steps: []deskPlanStep{}}
		for _, s := range m.Steps {
			after := s.After
			if after == nil {
				after = []string{}
			}
			res.Steps = append(res.Steps, deskPlanStep{ID: s.ID, After: after, Timeout: s.Timeout, Private: s.Private})
		}
		return writeJSON(out, res)
	}
	w := &stickyWriter{w: out}
	w.printf("name=%s\nsha256=%s\nsteps=%d\norder=%s\n", safeText(label), m.SHA256, len(m.Steps), desk.OrderLine(waves))
	for _, warning := range warnings {
		w.printf("warning: %s\n", safeText(warning))
	}
	return w.err
}

// readPlanTarget reads a manifest file, or a desk item by name.
func readPlanTarget(cmd *cobra.Command, deps module.Deps, dirFlag, target string) (label string, data []byte, err error) {
	if strings.HasSuffix(target, ".manifest") || strings.Contains(target, "/") {
		// The core's reader: capped, and a FIFO or device is refused, not
		// read (a blocking open would hang).
		data, err := desk.ReadSource(target)
		if err != nil {
			return "", nil, err
		}
		return filepath.Base(target), data, nil
	}
	if err := checkDeskName(target); err != nil {
		return "", nil, err
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return "", nil, err
	}
	defer d.Close() //nolint:errcheck // read side
	snap, err := d.Scan()
	if err != nil {
		return "", nil, err
	}
	for _, it := range snap.Pending {
		if it.Name == target {
			if it.Kind != desk.KindBatch {
				return "", nil, fmt.Errorf("desk plan: %s is a script, not a batch manifest", target)
			}
			if it.State == desk.StateRefused {
				return "", nil, fmt.Errorf("desk plan: %s is refused: %s", target, safeText(it.Refusal))
			}
			return target, it.Content, nil
		}
	}
	data, kind, err := d.Record(target)
	if errors.Is(err, desk.ErrNotFound) {
		return "", nil, fmt.Errorf("desk plan: no item named %s", target)
	}
	if err != nil {
		return "", nil, err
	}
	if kind != desk.KindBatch {
		return "", nil, fmt.Errorf("desk plan: %s is a script, not a batch manifest", target)
	}
	return target, data, nil
}

// deskItemJSON is one item in `desk status --json`.
type deskItemJSON struct {
	Name            string     `json:"name"`
	Number          *int       `json:"number"` // null for a legacy name with no NN- number
	Legacy          bool       `json:"legacy"`
	Kind            string     `json:"kind"`
	State           string     `json:"state"`
	What            string     `json:"what"`
	Why             string     `json:"why"`
	TTY             bool       `json:"tty"`
	SHA256          string     `json:"sha256"`
	AddedAt         *time.Time `json:"added_at"`
	StartedAt       *time.Time `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	AgeSeconds      *int64     `json:"age_seconds"`
	DurationSeconds *int64     `json:"duration_seconds"`
	Stale           bool       `json:"stale"`
	ExitCode        *int       `json:"exit_code"`
	SkipReason      string     `json:"skip_reason"`
	SkipNote        string     `json:"skip_note"`
	SkippedBy       string     `json:"skipped_by"`
	SkippedAt       *time.Time `json:"skipped_at"`
	Refusal         string     `json:"refusal"`
	PID             int        `json:"pid"`
}

// deskStatusJSON is `desk status --json`.
type deskStatusJSON struct {
	Dir     string         `json:"dir"`
	Taken   time.Time      `json:"taken"`
	Pending []deskItemJSON `json:"pending"`
	Running []deskItemJSON `json:"running"`
	Done    []deskItemJSON `json:"done"`
	Skipped []deskItemJSON `json:"skipped"`
}

// deskDetailJSON is `desk status NAME --json`.
type deskDetailJSON struct {
	Item    deskItemJSON      `json:"item"`
	Log     string            `json:"log"`
	Events  string            `json:"events"`
	Steps   []deskStepJSON    `json:"steps"`
	Summary *desk.Summary     `json:"summary"`
	Record  *deskRecordDigest `json:"record"`
}

// deskRecordDigest names the bytes a running or finished item ran.
type deskRecordDigest struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type deskStepJSON struct {
	ID    string     `json:"id"`
	State string     `json:"state"`
	RC    *int       `json:"rc"`
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
	Deps  []string   `json:"deps"`
}

// itemView is the facts status prints about one item, computed once for
// both the text and the JSON form.
func itemView(it desk.Item, now time.Time) deskItemJSON {
	v := deskItemJSON{
		Name: it.Name, Kind: string(it.Kind), State: string(it.State), Legacy: it.Legacy,
		What: it.What, Why: it.Why, TTY: it.TTY, SHA256: it.Meta.SHA256,
		AddedAt: it.Meta.AddedAt, Stale: it.Stale, ExitCode: it.ExitCode,
		SkipReason: it.Meta.SkipReason, SkipNote: it.Meta.SkipNote, SkippedBy: it.Meta.SkippedBy, SkippedAt: it.Meta.SkippedAt, Refusal: it.Refusal, PID: it.Meta.PID,
	}
	if it.Number >= 0 {
		n := it.Number
		v.Number = &n
	}
	if !it.Started.IsZero() {
		t := it.Started.UTC()
		v.StartedAt = &t
	}
	if !it.Ended.IsZero() {
		t := it.Ended.UTC()
		v.EndedAt = &t
	}
	var since *time.Time
	switch it.State {
	case desk.StateWaiting, desk.StateRefused:
		since = v.AddedAt
	case desk.StateRunning, desk.StateLost:
		since = v.StartedAt
	case desk.StateDone:
		since = v.EndedAt
	}
	if since != nil {
		age := int64(now.Sub(*since).Seconds())
		v.AgeSeconds = &age
	}
	if v.StartedAt != nil && v.EndedAt != nil {
		dur := int64(v.EndedAt.Sub(*v.StartedAt).Seconds())
		v.DurationSeconds = &dur
	}
	return v
}

func itemViews(items []desk.Item, now time.Time) []deskItemJSON {
	out := make([]deskItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, itemView(it, now))
	}
	return out
}

func runDeskStatus(cmd *cobra.Command, deps module.Deps, dirFlag, name string, asJSON bool) error {
	if name != "" {
		if err := checkDeskName(name); err != nil {
			return err
		}
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // Scan's own writes are synced
	snap, err := d.Scan()
	if err != nil {
		return err
	}
	now := deskNow().UTC()
	out := cmd.OutOrStdout()
	if name != "" {
		name = resolveName(name, func(n string) bool { return snapshotHasItem(snap, n) })
		return printDeskDetail(out, d, snap, name, now, asJSON)
	}
	if asJSON {
		return writeJSON(out, deskStatusJSON{
			Dir: snap.Dir, Taken: snap.Taken,
			Pending: itemViews(snap.Pending, now), Running: itemViews(snap.Running, now),
			Done: itemViews(snap.Done, now), Skipped: itemViews(snap.Skipped, now),
		})
	}
	w := &stickyWriter{w: out}
	w.printf("desk %s: %d waiting, %d running, %d done, %d skipped\n",
		safeText(snap.Dir), len(snap.Pending), len(snap.Running), len(snap.Done), len(snap.Skipped))
	for _, group := range [][]desk.Item{snap.Pending, snap.Running} {
		for _, it := range group {
			w.printf("%s\n", safeText(statusLine(itemView(it, now))))
		}
	}
	for i, it := range snap.Done {
		if i == deskStatusDone {
			w.printf("… %d older done items (--json lists all)\n", len(snap.Done)-deskStatusDone)
			break
		}
		w.printf("%s\n", safeText(statusLine(itemView(it, now))))
	}
	for _, it := range snap.Skipped {
		w.printf("%s\n", safeText(statusLine(itemView(it, now))))
	}
	return w.err
}

// statusLine renders one item as a grep-friendly line. Every untrusted field
// (WHAT, a skip note, a refusal) goes through termsafe.
func statusLine(v deskItemJSON) string {
	parts := []string{fmt.Sprintf("%-8s %s", v.State, v.Name)}
	if v.AgeSeconds != nil {
		parts = append(parts, "age="+shortAge(*v.AgeSeconds))
	}
	if v.Stale {
		parts = append(parts, "stale")
	}
	if v.Legacy {
		parts = append(parts, "legacy")
	}
	if v.TTY {
		parts = append(parts, "tty")
	}
	if v.SHA256 != "" && v.State != string(desk.StateDone) {
		parts = append(parts, "sha256="+v.SHA256[:min(deskShortSHA, len(v.SHA256))])
	}
	switch {
	case v.State == string(desk.StateDone) && v.ExitCode != nil:
		parts = append(parts, "exit="+strconv.Itoa(*v.ExitCode))
	case v.State == string(desk.StateDone):
		parts = append(parts, "no-exit-recorded")
	}
	if v.DurationSeconds != nil && v.State == string(desk.StateDone) {
		parts = append(parts, "took="+shortAge(*v.DurationSeconds))
	}
	if v.SkipReason != "" {
		parts = append(parts, "reason="+termsafe.SafeLineMax(v.SkipReason, deskQuoteMax))
	}
	if v.SkippedBy != "" {
		parts = append(parts, "by="+termsafe.SafeLineMax(v.SkippedBy, deskQuoteMax))
	}
	if v.SkipNote != "" {
		parts = append(parts, "note="+strconv.Quote(termsafe.SafeLineMax(v.SkipNote, deskQuoteMax)))
	}
	if v.Refusal != "" {
		parts = append(parts, "refused="+strconv.Quote(termsafe.SafeLineMax(v.Refusal, deskQuoteMax)))
	}
	if v.State == string(desk.StateLost) {
		parts = append(parts, "(clear it: forgectl desk skip "+v.Name+" --reason ...)")
	}
	if v.What != "" && (v.State == string(desk.StateWaiting) || v.State == string(desk.StateRunning)) {
		parts = append(parts, "what="+strconv.Quote(termsafe.SafeLineMax(v.What, deskWhatCols)))
	}
	return strings.Join(parts, "  ")
}

// shortAge renders seconds as 42s, 12m, 3h, or 2d.
func shortAge(s int64) string {
	switch {
	case s < 0:
		return "0s"
	case s < 60:
		return strconv.FormatInt(s, 10) + "s"
	case s < 3600:
		return strconv.FormatInt(s/60, 10) + "m"
	case s < 48*3600:
		return strconv.FormatInt(s/3600, 10) + "h"
	}
	return strconv.FormatInt(s/86400, 10) + "d"
}

func findItem(snap *desk.Snapshot, name string) (desk.Item, bool) {
	for _, group := range [][]desk.Item{snap.Pending, snap.Running, snap.Done, snap.Skipped} {
		for _, it := range group {
			if it.Name == name {
				return it, true
			}
		}
	}
	return desk.Item{}, false
}

func printDeskDetail(out io.Writer, d *desk.Desk, snap *desk.Snapshot, name string, now time.Time, asJSON bool) error {
	it, ok := findItem(snap, name)
	if !ok {
		return deskNotFound("desk status", "item", name, "waiting", waitingNames(d))
	}
	res := deskDetailJSON{Item: itemView(it, now), Log: d.LogPath(name), Events: d.EventsPath(name)}
	if it.State != desk.StateWaiting && it.State != desk.StateRefused {
		if data, _, err := d.Record(name); err == nil {
			res.Record = &deskRecordDigest{SHA256: desk.SHA256Hex(data), Bytes: len(data)}
		}
	}
	if it.Kind == desk.KindBatch {
		if sum, err := d.ReadSummary(name); err == nil {
			res.Summary = sum
		}
		if rows, err := d.BatchStatus(name); err == nil {
			for _, r := range rows {
				res.Steps = append(res.Steps, stepView(r))
			}
		}
	}
	if asJSON {
		if res.Steps == nil {
			res.Steps = []deskStepJSON{}
		}
		return writeJSON(out, res)
	}
	v := res.Item
	w := &stickyWriter{w: out}
	// Every value goes through safeText, even ones the desk validated:
	// this is a print site for meta read from disk.
	kv := func(k, val string) { w.printf("%s=%s\n", k, safeText(val)) }
	kv("name", v.Name)
	kv("state", v.State)
	kv("kind", v.Kind)
	kv("what", safeText(v.What))
	kv("why", safeText(v.Why))
	kv("tty", strconv.FormatBool(v.TTY))
	kv("sha256", v.SHA256)
	for _, t := range []struct {
		k string
		v *time.Time
	}{{"added", v.AddedAt}, {"started", v.StartedAt}, {"ended", v.EndedAt}, {"skipped_at", v.SkippedAt}} {
		if t.v != nil {
			kv(t.k, t.v.UTC().Format(time.RFC3339))
		}
	}
	if v.ExitCode != nil {
		kv("exit", strconv.Itoa(*v.ExitCode))
	} else if v.State == string(desk.StateDone) {
		kv("exit", "none recorded")
	}
	if v.Stale {
		kv("stale", "true")
	}
	for _, f := range []struct{ k, v string }{{"skip_reason", v.SkipReason}, {"skipped_by", v.SkippedBy}, {"skip_note", v.SkipNote}, {"refusal", v.Refusal}} {
		if f.v != "" {
			kv(f.k, safeText(f.v))
		}
	}
	if res.Record != nil {
		kv("record_sha256", res.Record.SHA256)
	}
	if v.State == string(desk.StateDone) {
		kv("log", safeText(res.Log))
	}
	kv("events", safeText(res.Events))
	if res.Summary != nil {
		s := res.Summary
		w.printf("summary rc=%d reason=%s ok=%d failed=%d skipped=%d\n", s.RC, safeText(s.Reason), s.OK, s.Failed, s.Skipped)
		for _, st := range s.Steps {
			line := fmt.Sprintf("step %s %s", st.ID, safeText(st.State))
			if st.RC != nil {
				line += " rc=" + strconv.Itoa(*st.RC)
			}
			if st.Duration != nil {
				line += fmt.Sprintf(" %.1fs", *st.Duration)
			}
			if st.Reason != "" {
				line += " reason=" + safeText(st.Reason)
			}
			w.printf("%s\n", safeText(line))
		}
		return w.err
	}
	for _, st := range res.Steps {
		line := fmt.Sprintf("step %s %s", st.ID, safeText(st.State))
		if st.RC != nil {
			line += " rc=" + strconv.Itoa(*st.RC)
		}
		w.printf("%s\n", safeText(line))
	}
	return w.err
}

func stepView(r desk.StepStatus) deskStepJSON {
	s := deskStepJSON{ID: r.ID, State: r.State, RC: r.RC, Deps: r.Deps}
	if !r.Start.IsZero() {
		t := r.Start
		s.Start = &t
	}
	if !r.End.IsZero() {
		t := r.End
		s.End = &t
	}
	if s.Deps == nil {
		s.Deps = []string{}
	}
	return s
}

func runDeskWatch(cmd *cobra.Command, deps module.Deps, dirFlag, name string, deadline, skip int) error {
	if err := checkDeskName(name); err != nil {
		return err
	}
	// The resume= line must name the same desk; a --dir that cannot be
	// written as one shell word is refused now, not dropped from it later.
	if dirFlag != "" {
		dir, err := resolveDeskDir(dirFlag)
		if err != nil {
			return err
		}
		if _, err := shellQuote(dir); err != nil {
			return deskUsage("desk watch: --dir %w, so a resume= line could not name it", err)
		}
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // read side
	name = resolveDeskName(d, name)
	// A write to a closed stdout must come back as EPIPE, not kill the
	// process with SIGPIPE before it can say where to resume.
	signal.Ignore(syscall.SIGPIPE)
	defer signal.Reset(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if deadline > 0 {
		var cancel context.CancelFunc
		watchCtx, cancel = context.WithTimeout(watchCtx, time.Duration(deadline)*time.Second)
		defer cancel()
	}
	w := &stickyWriter{w: cmd.OutOrStdout()}
	var rc *int
	printed := 0
	emit := func(line string) {
		if w.err != nil {
			return
		}
		w.printf("%s\n", safeText(line))
		if w.err != nil {
			// Nobody is reading: stop now rather than watch a run for no one.
			stopWatch()
			return
		}
		printed++
		if v, ok := runEndRC(line); ok {
			rc = &v
		}
	}
	state, seen, err := d.Watch(watchCtx, name, skip, deskWatchInterval, emit)
	switch {
	case w.err != nil:
		// stdout is gone, so the resume point goes in the error, on stderr.
		return WithExitCode(fmt.Errorf("desk watch: stdout closed (%w); resume with %s",
			w.err, watchResume(name, skip+printed, deadline, dirFlag)), deskExitBrokenPipe)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		w.printf("resume=%s\n", watchResume(name, seen, deadline, dirFlag))
		return WithExitCode(fmt.Errorf("desk watch: %s did not finish within %ds", name, deadline), deskExitTempFail)
	case err != nil && ctx.Err() != nil:
		w.printf("resume=%s\n", watchResume(name, seen, deadline, dirFlag))
		return WithExitCode(fmt.Errorf("desk watch: interrupted while watching %s", name), deskExitInterrupted)
	case errors.Is(err, desk.ErrNotFound):
		return deskNotFound("desk watch", "item", name, "waiting", waitingNames(d))
	case err != nil:
		return err
	}
	switch state {
	case desk.WatchLost:
		return fmt.Errorf("desk watch: %s is lost: its supervisor is gone with no RUN-END (clear it with forgectl desk skip %s --reason <why>)", name, name)
	case desk.WatchSkipped:
		return fmt.Errorf("desk watch: %s was skipped and will not run", name)
	case desk.WatchEnded:
	default:
		return fmt.Errorf("desk watch: %s stopped in state %s", name, state)
	}
	if rc == nil {
		// A legacy item with no events, or a resume point past RUN-END: the
		// queue's recorded exit is the outcome.
		snap, err := d.Scan()
		if err != nil {
			return err
		}
		if it, ok := findItem(snap, name); ok && it.ExitCode != nil {
			rc = it.ExitCode
		}
	}
	switch {
	case rc == nil:
		return fmt.Errorf("desk watch: %s finished with no exit recorded", name)
	case *rc != 0:
		return fmt.Errorf("desk watch: %s finished with exit %d", name, *rc)
	}
	return nil
}

// runEndRC reads rc= from a RUN-END line.
func runEndRC(line string) (int, bool) {
	rest, ok := strings.CutPrefix(line, desk.EventRunEnd+" ")
	if !ok {
		return 0, false
	}
	for _, f := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(f, "rc="); ok {
			n, err := strconv.Atoi(v)
			return n, err == nil
		}
	}
	return 0, false
}

// watchResume is the command that picks a watch up after seen lines.
func watchResume(name string, seen, deadline int, dirFlag string) string {
	s := fmt.Sprintf("forgectl desk watch %s --skip %d", name, seen)
	if deadline > 0 {
		s += " --deadline " + strconv.Itoa(deadline)
	}
	if dirFlag != "" {
		// runDeskWatch refused a --dir that cannot be quoted before it
		// started, so neither step fails here.
		dir, _ := resolveDeskDir(dirFlag)
		q, _ := shellQuote(dir)
		s += " --dir " + q
	}
	return s
}

// deskSkipJSON is `desk skip --json`.
type deskSkipJSON struct {
	Name string `json:"name"`
	// Reason is the category recorded with the skip: "operator", or "lost" for
	// a lost run.
	Reason string `json:"reason"`
	// Note is the one-line --reason text kept with the skip (empty for a skip
	// made without one).
	Note string `json:"note"`
	// Already is true when the item was already in skipped/: nothing changed,
	// and Reason and Note are what was recorded then.
	Already bool `json:"already"`
}

func runDeskSkip(cmd *cobra.Command, deps module.Deps, dirFlag, name, reason string, asJSON bool) error {
	if err := checkDeskName(name); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return deskUsage("desk skip: --reason is required and must not be empty")
	}
	if err := checkFlagText("--reason", reason); err != nil {
		return err
	}
	if len([]rune(reason)) > desk.SkipNoteMax {
		return deskUsage("desk skip: --reason must be at most %d characters", desk.SkipNoteMax)
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // Skip syncs its own writes
	name = resolveDeskName(d, name)
	recorded, err := d.SkipNoted(name, reason)
	switch {
	case errors.Is(err, desk.ErrNotFound):
		// A retry finds the item already skipped: report it, exit 0, as
		// `tasks done` does for a task already done. A name that never
		// existed is the error below, so a caller can tell them apart.
		if meta, ok := d.SkippedMeta(name); ok {
			return reportDeskSkip(cmd, deskSkipJSON{Name: name, Reason: skipReasonOf(meta), Note: meta.SkipNote, Already: true}, asJSON)
		}
		return deskSkipNotFound(d, name)
	case errors.Is(err, desk.ErrClaimed):
		return fmt.Errorf("desk skip: %s was claimed by a desk first", name)
	case err != nil:
		return err
	}
	return reportDeskSkip(cmd, deskSkipJSON{Name: name, Reason: recorded, Note: reason}, asJSON)
}

// skipReasonOf is the category recorded with a skip: operator when the meta
// names none (an older skip).
func skipReasonOf(meta desk.Meta) string {
	if meta.SkipReason == "" {
		return desk.SkipOperator
	}
	return meta.SkipReason
}

// reportDeskSkip prints a skip: key=value lines with the note quoted, or JSON.
// An already-skipped item adds already=true and a note on stderr.
func reportDeskSkip(cmd *cobra.Command, r deskSkipJSON, asJSON bool) error {
	if asJSON {
		r.Reason, r.Note = safeText(r.Reason), safeText(r.Note)
		return writeJSON(cmd.OutOrStdout(), r)
	}
	if r.Already {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "note: %s was already skipped; nothing changed\n", safeText(r.Name)); err != nil {
			return err
		}
	}
	line := fmt.Sprintf("skipped=%s reason=%s note=%s", r.Name, safeText(r.Reason), strconv.Quote(safeText(r.Note)))
	if r.Already {
		line += " already=true"
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), line)
	return err
}

// deskSkipNotFound is the error for a name that is in no state a skip can
// act on. It lists the waiting names, and any item with the same name under
// another number, which is the usual cause (the number changed when the item
// was queued again).
func deskSkipNotFound(d *desk.Desk, name string) error {
	err := deskNotFound("desk skip", "waiting item or lost run", name, "waiting", waitingNames(d))
	if similar := similarItems(d, name); len(similar) > 0 {
		return fmt.Errorf("%w; same name under another number: %s", err, strings.Join(similar, ", "))
	}
	return err
}

// deskPruneJSON is `desk prune --json`.
type deskPruneJSON struct {
	Removed int `json:"removed"`
	Days    int `json:"days"`
	// Found is false when the directory is not a desk: nothing was opened or
	// created, and Removed is 0.
	Found bool `json:"found"`
}

// deskPrunePlanJSON is `desk prune --dry-run --json`.
type deskPrunePlanJSON struct {
	DryRun bool `json:"dry_run"`
	// Found is false when the desk directory does not exist: nothing was
	// opened or created, and the plan is empty because there is no desk.
	Found       bool                `json:"found"`
	Days        int                 `json:"days"`
	WouldRemove int                 `json:"would_remove"`
	Items       []deskPrunePlanItem `json:"items"`
}

// deskPrunePlanItem is one item a prune would delete.
type deskPrunePlanItem struct {
	State  string    `json:"state"`
	Name   string    `json:"name"`
	Newest time.Time `json:"newest"`
}

func runDeskPrune(cmd *cobra.Command, deps module.Deps, dirFlag string, days int, asJSON, dryRun bool) error {
	dir, err := resolveDeskDir(dirFlag)
	if err != nil {
		return err
	}
	// A prune never creates the desk: every other verb makes a missing desk
	// directory by opening it, which for prune would turn a typo in --dir into
	// a new empty desk and a quiet pruned=0.
	if !desk.Exists(dir) {
		return reportNoDeskToPrune(cmd, dir, days, asJSON, dryRun)
	}
	if dryRun {
		plan, err := desk.PrunePlanAt(dir, days)
		if err != nil {
			return err
		}
		return printDeskPrunePlan(cmd.OutOrStdout(), plan, true, days, asJSON)
	}
	d, err := openDeskDirFor(cmd, deps, dirFlag)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // deletes are not buffered
	n, err := d.Prune(days)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), deskPruneJSON{Removed: n, Days: days, Found: true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "pruned=%d days=%d\n", n, days)
	return err
}

// reportNoDeskToPrune is prune, or its preview, on a directory that is not a
// desk: nothing is opened or created, a note names the path on stderr, and the
// result is the empty one with found=false. Exit 0, as an empty desk.
func reportNoDeskToPrune(cmd *cobra.Command, dir string, days int, asJSON, dryRun bool) error {
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "note: desk not found at %s; nothing to prune\n", safePath(dir)); err != nil {
		return err
	}
	if dryRun {
		return printDeskPrunePlan(cmd.OutOrStdout(), nil, false, days, asJSON)
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), deskPruneJSON{Days: days})
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "pruned=0 days=%d\n", days)
	return err
}

// printDeskPrunePlan is `desk prune --dry-run`'s output for plan.
func printDeskPrunePlan(out io.Writer, plan []desk.PrunedItem, found bool, days int, asJSON bool) error {
	if asJSON {
		items := make([]deskPrunePlanItem, 0, len(plan))
		for _, p := range plan {
			items = append(items, deskPrunePlanItem{State: p.State, Name: p.Name, Newest: p.Newest.UTC()})
		}
		return writeJSON(out, deskPrunePlanJSON{DryRun: true, Found: found, Days: days, WouldRemove: len(plan), Items: items})
	}
	w := &stickyWriter{w: out}
	w.printf("would_prune=%d days=%d found=%t\n", len(plan), days, found)
	for _, p := range plan {
		w.printf("%s/%s newest=%s\n", p.State, safeText(p.Name), p.Newest.UTC().Format(time.RFC3339))
	}
	return w.err
}

func runDeskSupervise(dirFlag, name, sha, kind string) error {
	dir, err := resolveDeskDir(dirFlag)
	if err != nil {
		return err
	}
	if rc := desk.RunSupervisor(dir, name, sha, kind); rc != 0 {
		return WithExitCode(fmt.Errorf("desk item finished with exit %d", rc), rc)
	}
	return nil
}

// deskSupported: the desk core runs here.
const deskSupported = true
