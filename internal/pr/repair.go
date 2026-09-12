package pr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"charm.land/huh/v2"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// Repair modes. Exactly one applies per `--apply`; the CLI refuses zero or
// two, naming the whole set so the operator never has to guess what the third
// option was.
const (
	RepairModeAdoptWindow    = "--adopt-window"
	RepairModeRollback       = "--rollback"
	RepairModeForgetIfAbsent = "--forget-if-absent"
)

// Report outcomes. `unreadable` is the one that is not an action: it names a
// file this build could not decode, which blocks every launch until it is
// settled and must therefore appear in the report rather than only in a log.
const (
	repairOutcomeRefused    = "refused"
	repairOutcomeUnreadable = "unreadable"
	// repairPhaseUnreadable stands in the FromPhase column for a record whose
	// phase could not be read. It is deliberately not one of the six real
	// phases: nothing may treat it as a lifecycle state.
	repairPhaseUnreadable = "unreadable"
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
	var candidates []SessionSummary
	refs := make([]Ref, 0, len(summaries))
	for _, s := range summaries {
		if !isRepairPhase(s.Phase()) {
			continue
		}
		candidates = append(candidates, s)
		refs = append(refs, s.Ref())
	}
	report := RepairReport{Items: make([]RepairItem, 0, len(candidates)+len(unreadable))}
	// An unreadable record is the MOST urgent row, not an aside. Every arm that
	// counts records refuses while one exists, so leaving it out of the report
	// made `pr repair` print "no records need repair" and exit 0 while `pr pick`
	// was refused — a survey verb answering the opposite of the truth. Its row
	// carries the path and the decode error, because a count names no way out.
	for _, u := range unreadable {
		slog.Warn("A session record could not be read; it blocks every launch until it is settled.",
			"path", u.path, "error", u.err)
		report.Items = append(report.Items, RepairItem{
			RecordPath: u.path,
			FromPhase:  repairPhaseUnreadable,
			Outcome:    repairOutcomeUnreadable,
			Error:      termsafe.SafeLine(u.err.Error()),
		})
	}
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
	// THE OPERAND IS NOT THE RECORD. resolveBreadcrumbMember returns the
	// authoritative enumerated entry, and every step below acts on member.path
	// — never on what the caller typed. The adopt arm is why this matters most:
	// it writes through the atomic writer, which addresses a record as
	// sessionsDir + basename, so acting on an operand that merely resolves into
	// the directory would read one file and overwrite another sharing its name.
	member, err := c.resolveBreadcrumbEntry(opts.Record)
	if err != nil {
		return RepairItem{RecordPath: opts.Record, Outcome: repairOutcomeRefused}, err
	}
	bc, decodeErr := decodeBreadcrumbRecord(member.bytes, member.path)
	if decodeErr != nil {
		return c.repairUndecodableLocked(opts, member, decodeErr)
	}
	member.breadcrumb = bc
	ref, err := refFromRecord(bc)
	if err != nil {
		return RepairItem{RecordPath: member.path, Outcome: repairOutcomeRefused}, err
	}
	avail, _ := classifyWorkspace(bc.Workspace)
	item := RepairItem{
		Ref:             ref.String(),
		RecordPath:      member.path,
		FromPhase:       string(bc.Phase),
		Reason:          bc.RepairReason,
		WorkspaceExists: avail == workspaceAvailabilityLive,
	}
	switch {
	case opts.AdoptWindow:
		return c.repairAdoptLocked(ctx, member, ref, item, avail, opts.DryRun)
	case opts.Rollback:
		return c.repairRollbackLocked(ctx, opts, member, ref, item, avail)
	default:
		return c.repairForgetLocked(ctx, opts, member, ref, item, avail)
	}
}

// repairUndecodableLocked is the one escape for a record this build cannot
// read. Such a record refuses every counting arm — so it blocks every launch —
// while no verb can decode it, and before this the only way out was a manual
// `rm` that no message ever named.
//
// Only --forget-if-absent may settle it, and what it can honestly claim is
// narrow: the pinned-handle protocol proves WHICH file is being unlinked from
// dev+ino and byte equality, neither of which needs a decode. It cannot prove
// the record named no clean room, because it cannot read the record — so that
// is stated in the refusal path, logged, and written into the audit row rather
// than quietly assumed.
func (c *Client) repairUndecodableLocked(opts RepairOpts, member breadcrumbMember, decodeErr error) (RepairItem, error) {
	item := RepairItem{
		RecordPath: member.path,
		FromPhase:  repairPhaseUnreadable,
		Outcome:    repairOutcomeRefused,
		Error:      termsafe.SafeLine(decodeErr.Error()),
	}
	if !opts.ForgetIfAbsent {
		return item, fmt.Errorf("this build cannot read session record %s, so it cannot adopt or roll it back: %w — "+
			"settle it with 'forgectl pr repair %s --apply %s', which removes the record without reading it",
			member.displayPath, decodeErr, opts.Record, RepairModeForgetIfAbsent)
	}
	item.ToPhase = "removed"
	if opts.DryRun {
		item.Outcome = "would-forget"
		return item, nil
	}
	slog.Warn("Removing a session record this build cannot read; whether it named a clean room cannot be checked.",
		"path", member.path, "error", decodeErr)
	rowID, err := c.beginRepairRow(Breadcrumb{Phase: repairPhaseUnreadable}, Ref{}, RepairModeForgetIfAbsent, "", member.path)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.discardUndecodableRecord(member); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, Breadcrumb{Phase: repairPhaseUnreadable}, Ref{}, RepairModeForgetIfAbsent, "", member.path, err)
		return item, err
	}
	c.completeRepairRow(rowID, Breadcrumb{Phase: repairPhaseUnreadable}, Ref{}, RepairModeForgetIfAbsent, "", member.path, nil)
	item.Outcome = "forgotten"
	slog.Info("Successfully discarded a session record this build could not read.", "path", member.path)
	return item, nil
}

// repairAdoptLocked promotes a record to `active` against a window that really
// exists.
//
// It takes NO operand for the window. An operator-supplied tmux target would
// be exactly the unqualified authority forgectl#218 removed, and a `-t`
// grammar injection site besides — so the window is re-derived from the ref
// the same way every other verb derives it, and the record is only promoted if
// that derivation finds one under this client's session.
func (c *Client) repairAdoptLocked(ctx context.Context, member breadcrumbMember, ref Ref, item RepairItem, avail workspaceAvailability, dryRun bool) (RepairItem, error) {
	bc := member.breadcrumb
	// The workspace check comes FIRST, before tmux is even consulted: adopting
	// promotes the record to something `pr teardown` will RemoveAll and kill,
	// so a record whose workspace is not a live sandbox must never reach that
	// state — and refusing before the tmux read keeps the refusal cheap and its
	// reason singular.
	if avail != workspaceAvailabilityLive {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to adopt %s: its workspace %s is not a live clean room, "+
			"and adopting would promote the record to one 'pr teardown' removes — "+
			"use 'forgectl pr repair %s --apply %s' instead",
			ref.String(), termsafe.QuotePath(bc.Workspace), member.path, RepairModeRollback)
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
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to adopt %s: the resolved window identity %s is not generation-qualified",
			ref.String(), termsafe.QuotePath(adopted.WindowID))
	}
	item.WindowLive = true
	item.ToPhase = string(PhaseActive)
	if dryRun {
		item.Outcome = "would-adopt"
		return item, nil
	}
	rowID, err := c.beginRepairRow(bc, ref, RepairModeAdoptWindow, adopted.WindowID, member.path)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.writeAdoptedRecord(member.path, bc, adopted.WindowID); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, bc, ref, RepairModeAdoptWindow, adopted.WindowID, member.path, err)
		return item, err
	}
	c.completeRepairRow(rowID, bc, ref, RepairModeAdoptWindow, adopted.WindowID, member.path, nil)
	item.Outcome = "adopted"
	slog.Info("Successfully adopted a live review window into its session record.",
		"ref", ref.String(), "path", member.path, "window", nativeWindowID(adopted.WindowID))
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
// not absent), refuses a workspace teardown cannot act on, writes a durable
// intent row, tears down, and only then completes the row.
//
// EVERY REFUSAL PRECEDES THE INTENT ROW, on purpose. A dangling intent with no
// completion is the signal that a rollback died mid-delete — the one shape that
// says "a clean room may be orphaned, go look" — so a refusal that wrote one
// would forge that signal and send an operator hunting a directory nothing ever
// touched.
//
// The intent row is what makes a real mid-way death recoverable: once the
// record is gone, the log line is the only pointer left to the clean room.
func (c *Client) repairRollbackLocked(ctx context.Context, opts RepairOpts, member breadcrumbMember, ref Ref, item RepairItem, avail workspaceAvailability) (RepairItem, error) {
	bc := member.breadcrumb
	live, ok := c.WindowLive(ctx, ref)
	if !ok {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to roll back %s: the tmux window list could not be read, "+
			"and an unreadable list is not an absent window — check `tmux list-windows -a`, then retry", ref.String())
	}
	item.WindowLive = live
	if live {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to roll back %s: its review window is still live — "+
			"adopt it with 'forgectl pr repair %s --apply %s', or close the window first",
			ref.String(), member.path, RepairModeAdoptWindow)
	}
	// Teardown refuses a workspace that is neither a live sandbox nor cleanly
	// absent, so establish that here rather than discovering it after the intent
	// row is already on disk.
	if bc.Workspace != "" && avail == workspaceAvailabilityInvalid {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to roll back %s: its recorded workspace %s is neither a live clean room "+
			"nor cleanly absent, so teardown cannot act on it — inspect that path by hand before settling this record",
			ref.String(), termsafe.QuotePath(bc.Workspace))
	}
	item.ToPhase = "removed"
	// The preview comes BEFORE the confirmation gate: --dry-run mutates nothing,
	// so gating a read-only preview on a destructive confirmation (or on --yes
	// off a terminal) refuses the one invocation that was always safe.
	if opts.DryRun {
		item.Outcome = "would-rollback"
		return item, nil
	}
	if !opts.Yes {
		if !c.isTTY() {
			item.Outcome = repairOutcomeRefused
			return item, fmt.Errorf("refusing to roll back %s without confirmation: this removes its clean room %s "+
				"and its record, and there is no terminal to confirm on — pass --yes to proceed",
				ref.String(), termsafe.QuotePath(bc.Workspace))
		}
		approved, err := c.confirmRemoval(rollbackPrompt(ref, bc))
		if err != nil {
			item.Outcome = repairOutcomeRefused
			return item, fmt.Errorf("rollback confirmation: %w", err)
		}
		if !approved {
			item.Outcome = "declined"
			return item, nil
		}
	}
	rowID, err := c.beginRepairRow(bc, ref, RepairModeRollback, "", member.path)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.teardownLocked(ctx, member.path); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, bc, ref, RepairModeRollback, "", member.path, err)
		slog.Error("A repair rollback failed partway; the clean room is recoverable from the repair audit log.",
			"ref", ref.String(), "workspace", bc.Workspace, "log", c.repairLogPath(), "error", err)
		return item, fmt.Errorf("roll back %s: %w — its clean room %s is named in %s",
			ref.String(), err, termsafe.QuotePath(bc.Workspace), termsafe.QuotePath(c.repairLogPath()))
	}
	c.completeRepairRow(rowID, bc, ref, RepairModeRollback, "", member.path, nil)
	item.Outcome = "rolled-back"
	slog.Info("Successfully rolled back an unfinished review session.", "ref", ref.String(), "path", member.path)
	return item, nil
}

// rollbackPrompt is what the interactive gate shows before a rollback. It names
// both things that go away, because a confirmation that does not say what is
// being removed is not one.
//
// Both interpolations are terminal-clamped. A workspace only has to be an
// absolute path to validate, so it can carry control or bidi bytes — and this
// is the single surface where a human is asked to approve a deletion, which is
// the exact place those bytes must not be able to hide what is being removed.
func rollbackPrompt(ref Ref, bc Breadcrumb) string {
	workspace := "(no clean room was ever created)"
	if bc.Workspace != "" {
		workspace = termsafe.QuotePath(bc.Workspace)
	}
	return fmt.Sprintf("Roll back the unfinished review of %s?\n  clean room: %s\n  record:     %s",
		termsafe.SafeLine(ref.String()), workspace, termsafe.SafeLine(bc.Ref))
}

// confirmRemoval is the default destructive-repair gate. Every line a human
// reads here names a REMOVAL — the note title, the confirm title, and the
// affirmative button — so that answering yes cannot mean something other than
// what was asked. confirmReview's "Post this review to the PR?" previously
// stood in for this, and its yes deleted a clean room.
func confirmRemoval(prompt string, th theme.Theme) (bool, error) {
	ok := false
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Remove this clean room? — this cannot be undone").
				Description(prompt),
			huh.NewConfirm().
				Title("Remove the clean room and its session record?").
				Affirmative("Remove").
				Negative("Cancel").
				Value(&ok),
		),
	).WithTheme(th.Huh()).Run()
	return ok, err
}

// repairForgetLocked removes ONLY the record, after proving that neither a
// window nor a workspace is left behind. It is the narrow case: a record whose
// subject is already gone. Like rollback, every refusal precedes the intent row.
func (c *Client) repairForgetLocked(ctx context.Context, opts RepairOpts, member breadcrumbMember, ref Ref, item RepairItem, avail workspaceAvailability) (RepairItem, error) {
	bc := member.breadcrumb
	live, ok := c.WindowLive(ctx, ref)
	if !ok {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to forget %s: the tmux window list could not be read, "+
			"and an unreadable list is not an absent window — check `tmux list-windows -a`, then retry", ref.String())
	}
	item.WindowLive = live
	if live {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to forget %s: its review window still exists — "+
			"adopt it with 'forgectl pr repair %s --apply %s'", ref.String(), member.path, RepairModeAdoptWindow)
	}
	// Forget's whole claim is that nothing is left behind. Live means a clean
	// room is; Invalid means SOMETHING is at that path and this build cannot say
	// what — neither is an absence, and both must refuse before the intent row.
	if bc.Workspace != "" && avail != workspaceAvailabilityMissing {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to forget %s: its recorded workspace %s is not cleanly absent, and forgetting "+
			"the record would leave it with nothing pointing at it — use 'forgectl pr repair %s --apply %s'",
			ref.String(), termsafe.QuotePath(bc.Workspace), member.path, RepairModeRollback)
	}
	item.ToPhase = "removed"
	if opts.DryRun {
		item.Outcome = "would-forget"
		return item, nil
	}
	rowID, err := c.beginRepairRow(bc, ref, RepairModeForgetIfAbsent, "", member.path)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.teardownLocked(ctx, member.path); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, bc, ref, RepairModeForgetIfAbsent, "", member.path, err)
		return item, fmt.Errorf("forget %s: %w", ref.String(), err)
	}
	c.completeRepairRow(rowID, bc, ref, RepairModeForgetIfAbsent, "", member.path, nil)
	item.Outcome = "forgotten"
	slog.Info("Successfully discarded a session record whose window and clean room were both gone.",
		"ref", ref.String(), "path", member.path)
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
