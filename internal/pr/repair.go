package pr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Repair modes. Exactly one applies per `--apply`; the CLI refuses zero or
// two, naming the whole set so the operator never has to guess what the third
// option was.
const (
	RepairModeAdoptWindow    = "--adopt-window"
	RepairModeRollback       = "--rollback"
	RepairModeForgetIfAbsent = "--forget-if-absent"
)

// RepairOpts drives one `pr repair` invocation.
type RepairOpts struct {
	// Record is the breadcrumb to act on. Required with Apply; ignored (and
	// meaningless) for an inspect.
	Record string
	// Apply turns the inspect into a mutation. Without it Repair reads only.
	Apply bool
	// Exactly one of these three is set when Apply is.
	AdoptWindow    bool
	Rollback       bool
	ForgetIfAbsent bool
	// DryRun prints what would happen and touches nothing.
	DryRun bool
	// Yes is the off-a-TTY confirmation a destructive mode requires.
	Yes bool
}

// RepairItem is one row of a repair report — what the record SAID, what was
// OBSERVED beside it, and what (if anything) was done.
type RepairItem struct {
	Ref             string `json:"ref"`
	RecordPath      string `json:"record_path"`
	FromPhase       string `json:"from_phase"`
	ToPhase         string `json:"to_phase,omitempty"`
	Reason          string `json:"reason,omitempty"`
	WindowLive      bool   `json:"window_live"`
	WorkspaceExists bool   `json:"workspace_exists"`
	Outcome         string `json:"outcome"`
	Error           string `json:"error,omitempty"`
}

// RepairReport is what `pr repair` returns and `--json` encodes.
type RepairReport struct {
	Items []RepairItem `json:"items"`
}

// repairPhases are the phases a record can be stuck in. `queued` has reserved
// nothing and `active` is the healthy end state, so neither is a candidate.
func isRepairPhase(p Phase) bool {
	switch p {
	case PhasePreparing, PhasePrepared, PhaseLaunching, PhaseNeedsRepair:
		return true
	}
	return false
}

// Repair inspects or settles unfinished review sessions.
//
// It is a COMPOSITE verb: it takes the lifecycle lock exactly once and calls
// the unlocked cores (listLocked, teardownLocked, transitionLocked). The lock
// is non-reentrant, so re-entering List or Teardown here would deadlock into a
// bounded-wait timeout rather than failing fast.
//
// Without Apply it reads only — no record is written, no tmux command that
// mutates anything is issued, and the report is the whole output.
func (c *Client) Repair(ctx context.Context, opts RepairOpts) (RepairReport, error) {
	if err := validateRepairOpts(opts); err != nil {
		return RepairReport{}, err
	}
	var report RepairReport
	err := c.withLifecycleLock(ctx, "repair", func() error {
		if !opts.Apply {
			var ierr error
			report, ierr = c.repairInspectLocked(ctx)
			return ierr
		}
		item, aerr := c.repairApplyLocked(ctx, opts)
		report.Items = []RepairItem{item}
		return aerr
	})
	if report.Items == nil {
		report.Items = []RepairItem{}
	}
	return report, err
}

// validateRepairOpts enforces the argument grammar before anything is read, so
// a malformed invocation never reaches the filesystem or tmux.
func validateRepairOpts(opts RepairOpts) error {
	if !opts.Apply {
		return nil
	}
	if opts.Record == "" {
		return fmt.Errorf("--apply needs the breadcrumb to act on; " +
			"list the records that need one with 'forgectl pr repair'")
	}
	var given []string
	if opts.AdoptWindow {
		given = append(given, RepairModeAdoptWindow)
	}
	if opts.Rollback {
		given = append(given, RepairModeRollback)
	}
	if opts.ForgetIfAbsent {
		given = append(given, RepairModeForgetIfAbsent)
	}
	if len(given) == 1 {
		return nil
	}
	return fmt.Errorf("--apply takes exactly one of %s, %s, %s (got %s)",
		RepairModeAdoptWindow, RepairModeRollback, RepairModeForgetIfAbsent, describeGiven(given))
}

// describeGiven renders what the operator actually passed, so the refusal is
// about their command rather than about the grammar in the abstract.
func describeGiven(given []string) string {
	switch len(given) {
	case 0:
		return "none"
	case 1:
		return given[0]
	case 2:
		return given[0] + " and " + given[1]
	default:
		return given[0] + ", " + given[1] + " and " + given[2]
	}
}

// repairInspectLocked builds one row per unsettled record. It reads tmux once
// for the whole set rather than per record.
func (c *Client) repairInspectLocked(ctx context.Context) (RepairReport, error) {
	summaries, unreadable, err := c.listLocked()
	if err != nil {
		return RepairReport{}, err
	}
	if unreadable > 0 {
		// Not a refusal: inspect is the verb an operator reaches for BECAUSE
		// something is wrong, so it reports what it could not read and shows
		// the rest. Only the arms that COUNT records refuse on this.
		slog.Warn("Some session records could not be read while inspecting for repair.", "unreadable", unreadable)
	}
	var candidates []SessionSummary
	refs := make([]Ref, 0, len(summaries))
	for _, s := range summaries {
		if !isRepairPhase(s.Phase()) {
			continue
		}
		candidates = append(candidates, s)
		refs = append(refs, s.Ref())
	}
	report := RepairReport{Items: make([]RepairItem, 0, len(candidates))}
	if len(candidates) == 0 {
		return report, nil
	}
	windowLive, tmuxOK := c.WindowsLive(ctx, refs)
	for _, s := range candidates {
		bc, _, lerr := loadBreadcrumbRecord(s.Path(), c.sessionsDir)
		item := RepairItem{
			Ref:             s.Ref().String(),
			RecordPath:      s.Path(),
			FromPhase:       string(s.Phase()),
			WindowLive:      tmuxOK && windowLive[s.Ref()],
			WorkspaceExists: s.IsWorkspaceLive(),
			Outcome:         "inspect",
		}
		if lerr == nil {
			item.Reason = bc.RepairReason
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

// repairApplyLocked performs one mutation. Every path through it refuses
// before touching anything when its precondition does not hold, and every
// refusal leaves the record, the workspace, and tmux exactly as they were.
func (c *Client) repairApplyLocked(ctx context.Context, opts RepairOpts) (RepairItem, error) {
	bc, _, err := loadBreadcrumbRecord(opts.Record, c.sessionsDir)
	if err != nil {
		return RepairItem{RecordPath: opts.Record, Outcome: "refused"}, err
	}
	ref, err := refFromRecord(bc)
	if err != nil {
		return RepairItem{RecordPath: opts.Record, Outcome: "refused"}, err
	}
	avail, _ := classifyWorkspace(bc.Workspace)
	item := RepairItem{
		Ref:             ref.String(),
		RecordPath:      opts.Record,
		FromPhase:       string(bc.Phase),
		Reason:          bc.RepairReason,
		WorkspaceExists: avail == workspaceAvailabilityLive,
	}
	switch {
	case opts.AdoptWindow:
		return c.repairAdoptLocked(ctx, opts, bc, ref, item, avail)
	case opts.Rollback:
		return c.repairRollbackLocked(ctx, opts, bc, ref, item)
	default:
		return c.repairForgetLocked(ctx, opts, bc, ref, item, avail)
	}
}

// repairAdoptLocked promotes a record to `active` against a window that really
// exists.
//
// It takes NO operand for the window. An operator-supplied tmux target would
// be exactly the unqualified authority forgectl#218 removed, and a `-t`
// grammar injection site besides — so the window is re-derived from the ref
// the same way every other verb derives it, and the record is only promoted if
// that derivation finds one under this client's session.
func (c *Client) repairAdoptLocked(ctx context.Context, opts RepairOpts, bc Breadcrumb, ref Ref, item RepairItem, avail workspaceAvailability) (RepairItem, error) {
	// The workspace check comes FIRST, before tmux is even consulted: adopting
	// promotes the record to something `pr teardown` will RemoveAll and kill,
	// so a record whose workspace is not a live sandbox must never reach that
	// state — and refusing before the tmux read keeps the refusal cheap and its
	// reason singular.
	if avail != workspaceAvailabilityLive {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to adopt %s: its workspace %s is not a live clean room, "+
			"and adopting would promote the record to one 'pr teardown' removes — "+
			"use 'forgectl pr repair %s --apply %s' instead",
			ref.String(), termsafe.QuotePath(bc.Workspace), opts.Record, RepairModeRollback)
	}
	window, err := c.resolveReviewWindow(ctx, ref)
	if err != nil {
		item.Outcome = "refused"
		name, nameErr := ReviewWindowName(ref)
		if nameErr != nil {
			return item, nameErr
		}
		return item, fmt.Errorf("refusing to adopt %s: no window named %q exists in the %q session "+
			"(a window with that name under another session is not this review's): %w",
			ref.String(), name, c.tmuxSession, err)
	}
	adopted := newDispatch(ref, window)
	if !validWindowID(adopted.WindowID) {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to adopt %s: the resolved window identity %s is not generation-qualified",
			ref.String(), termsafe.QuotePath(adopted.WindowID))
	}
	item.WindowLive = true
	item.ToPhase = string(PhaseActive)
	if opts.DryRun {
		item.Outcome = "would-adopt"
		return item, nil
	}
	rowID, err := c.beginRepairRow(bc, ref, RepairModeAdoptWindow, adopted.WindowID, opts.Record)
	if err != nil {
		item.Outcome = "refused"
		return item, err
	}
	if err := c.writeAdoptedRecord(opts.Record, bc, adopted.WindowID); err != nil {
		item.Outcome = "failed"
		item.Error = err.Error()
		c.completeRepairRow(rowID, bc, ref, RepairModeAdoptWindow, adopted.WindowID, opts.Record, err)
		return item, err
	}
	c.completeRepairRow(rowID, bc, ref, RepairModeAdoptWindow, adopted.WindowID, opts.Record, nil)
	item.Outcome = "adopted"
	slog.Info("Successfully adopted a live review window into its session record.",
		"ref", ref.String(), "path", opts.Record, "window", nativeWindowID(adopted.WindowID))
	return item, nil
}

// writeAdoptedRecord lands the `active` record. A v2 record goes through the
// ordinary compare-and-write transition; a LEGACY record has no revision to
// compare, so it is written with the legacy expectation — the one conversion
// that exists, and the only transition a legacy record accepts.
func (c *Client) writeAdoptedRecord(path string, bc Breadcrumb, windowID string) error {
	if bc.Version == breadcrumbVersion {
		return c.transitionLocked(path, anyPhase, PhaseActive, func(rec *Breadcrumb) error {
			rec.WindowID = windowID
			rec.RepairReason = ""
			return nil
		})
	}
	next := bc
	next.Version = breadcrumbVersion
	next.Phase = PhaseActive
	next.Revision = 1
	next.WindowID = windowID
	if err := validateBreadcrumbRecord(next); err != nil {
		return fmt.Errorf("refusing to write the converted record: %w", err)
	}
	data, err := encodeBreadcrumb(next)
	if err != nil {
		return err
	}
	return writeRecordAtomic(c.fs, c.sessionsDir, baseName(path), data, expectLegacyRecord)
}

// repairRollbackLocked discards a session that never made it: it refuses while
// a window is live, refuses when the window list is UNREADABLE (unreadable is
// not absent), writes a durable intent row, tears down, and only then completes
// the row.
//
// The intent row is what makes a mid-way death recoverable: once the record is
// gone, the log line is the only pointer left to the clean room on disk.
func (c *Client) repairRollbackLocked(ctx context.Context, opts RepairOpts, bc Breadcrumb, ref Ref, item RepairItem) (RepairItem, error) {
	live, ok := c.WindowLive(ctx, ref)
	if !ok {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to roll back %s: the tmux window list could not be read, "+
			"and an unreadable list is not an absent window — check `tmux list-windows -a`, then retry", ref.String())
	}
	item.WindowLive = live
	if live {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to roll back %s: its review window is still live — "+
			"adopt it with 'forgectl pr repair %s --apply %s', or close the window first",
			ref.String(), opts.Record, RepairModeAdoptWindow)
	}
	if !opts.Yes && !c.isTTY() {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to roll back %s without confirmation: this removes its clean room %s "+
			"and its record, and there is no terminal to confirm on — pass --yes to proceed",
			ref.String(), termsafe.QuotePath(bc.Workspace))
	}
	item.ToPhase = "removed"
	if opts.DryRun {
		item.Outcome = "would-rollback"
		return item, nil
	}
	if c.isTTY() && !opts.Yes {
		approved, err := c.approve(rollbackPrompt(ref, bc))
		if err != nil {
			item.Outcome = "refused"
			return item, fmt.Errorf("rollback confirmation: %w", err)
		}
		if !approved {
			item.Outcome = "declined"
			return item, nil
		}
	}
	rowID, err := c.beginRepairRow(bc, ref, RepairModeRollback, "", opts.Record)
	if err != nil {
		item.Outcome = "refused"
		return item, err
	}
	if err := c.teardownLocked(ctx, opts.Record); err != nil {
		item.Outcome = "failed"
		item.Error = err.Error()
		c.completeRepairRow(rowID, bc, ref, RepairModeRollback, "", opts.Record, err)
		slog.Error("A repair rollback failed partway; the clean room is recoverable from the repair audit log.",
			"ref", ref.String(), "workspace", bc.Workspace, "log", c.repairLogPath(), "error", err)
		return item, fmt.Errorf("roll back %s: %w — its clean room %s is named in %s",
			ref.String(), err, termsafe.QuotePath(bc.Workspace), termsafe.QuotePath(c.repairLogPath()))
	}
	c.completeRepairRow(rowID, bc, ref, RepairModeRollback, "", opts.Record, nil)
	item.Outcome = "rolled-back"
	slog.Info("Successfully rolled back an unfinished review session.", "ref", ref.String(), "path", opts.Record)
	return item, nil
}

// rollbackPrompt is what the interactive gate shows before a rollback. It
// names both things that go away, because a confirmation that does not say
// what is being removed is not one.
func rollbackPrompt(ref Ref, bc Breadcrumb) string {
	workspace := bc.Workspace
	if workspace == "" {
		workspace = "(no clean room was ever created)"
	}
	return fmt.Sprintf("Roll back the unfinished review of %s?\n  clean room: %s\n  record:     %s",
		ref.String(), workspace, bc.Ref)
}

// repairForgetLocked removes ONLY the record, after proving that neither a
// window nor a workspace is left behind. It is the narrow case: a record whose
// subject is already gone.
func (c *Client) repairForgetLocked(ctx context.Context, opts RepairOpts, bc Breadcrumb, ref Ref, item RepairItem, avail workspaceAvailability) (RepairItem, error) {
	live, ok := c.WindowLive(ctx, ref)
	if !ok {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to forget %s: the tmux window list could not be read, "+
			"and an unreadable list is not an absent window — check `tmux list-windows -a`, then retry", ref.String())
	}
	item.WindowLive = live
	if live {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to forget %s: its review window still exists — "+
			"adopt it with 'forgectl pr repair %s --apply %s'", ref.String(), opts.Record, RepairModeAdoptWindow)
	}
	if avail == workspaceAvailabilityLive {
		item.Outcome = "refused"
		return item, fmt.Errorf("refusing to forget %s: its clean room %s still exists, and forgetting the record "+
			"would leave it with nothing pointing at it — use 'forgectl pr repair %s --apply %s'",
			ref.String(), termsafe.QuotePath(bc.Workspace), opts.Record, RepairModeRollback)
	}
	item.ToPhase = "removed"
	if opts.DryRun {
		item.Outcome = "would-forget"
		return item, nil
	}
	rowID, err := c.beginRepairRow(bc, ref, RepairModeForgetIfAbsent, "", opts.Record)
	if err != nil {
		item.Outcome = "refused"
		return item, err
	}
	if err := c.teardownLocked(ctx, opts.Record); err != nil {
		item.Outcome = "failed"
		item.Error = err.Error()
		c.completeRepairRow(rowID, bc, ref, RepairModeForgetIfAbsent, "", opts.Record, err)
		return item, fmt.Errorf("forget %s: %w", ref.String(), err)
	}
	c.completeRepairRow(rowID, bc, ref, RepairModeForgetIfAbsent, "", opts.Record, nil)
	item.Outcome = "forgotten"
	slog.Info("Successfully discarded a session record whose window and clean room were both gone.",
		"ref", ref.String(), "path", opts.Record)
	return item, nil
}

// beginRepairRow writes the write-ahead intent. A failure here REFUSES the
// mutation rather than proceeding without a trail: the row is the recovery
// pointer, so a rollback with no row is the one shape that can lose a clean
// room outright.
func (c *Client) beginRepairRow(bc Breadcrumb, ref Ref, mode, windowID, record string) (string, error) {
	id, err := randomSuffix()
	if err != nil {
		return "", fmt.Errorf("derive repair audit row id: %w", err)
	}
	row := RepairRow{
		TS: time.Now().UTC(), ID: id, Actor: repairActor(), Ref: ref.String(),
		RecordPath: record, FromPhase: string(bc.Phase), Mode: mode,
		WindowID: windowID, Workspace: bc.Workspace, Outcome: repairOutcomeIntent,
	}
	if err := c.appendRepairRowLocked(row); err != nil {
		return "", fmt.Errorf("record the repair intent before acting: %w — nothing was changed", err)
	}
	return id, nil
}

// completeRepairRow closes out an intent. Its own failure cannot undo the
// mutation that already happened, so it is logged rather than returned — the
// dangling intent row is itself the honest record of that.
func (c *Client) completeRepairRow(id string, bc Breadcrumb, ref Ref, mode, windowID, record string, cause error) {
	row := RepairRow{
		TS: time.Now().UTC(), ID: id, Actor: repairActor(), Ref: ref.String(),
		RecordPath: record, FromPhase: string(bc.Phase), Mode: mode,
		WindowID: windowID, Workspace: bc.Workspace, Outcome: repairOutcomeApplied,
	}
	if cause != nil {
		row.Outcome = repairOutcomeFailed
		row.Error = termsafe.SafeLine(cause.Error())
	}
	if err := c.appendRepairRowLocked(row); err != nil {
		slog.Error("Failed to complete a repair audit row; the intent row is left dangling, which is the honest record.",
			"id", id, "ref", ref.String(), "error", err)
	}
}

// baseName is filepath.Base with the package's own name, kept beside its one
// caller so a future change cannot silently hand writeRecordAtomic a path
// rather than a basename (which it refuses, loudly, by design).
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if os.IsPathSeparator(path[i]) {
			return path[i+1:]
		}
	}
	return path
}
