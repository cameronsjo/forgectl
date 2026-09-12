package pr

import (
	"context"
	"encoding/json"
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
	// RepairModePrune is the housekeeping sweep, and it is deliberately NOT one
	// of the three `--apply` modes: it takes no breadcrumb, acts on files that
	// are already outside every enumeration, and is the only repair mode that
	// UNLINKS rather than renames.
	RepairModePrune = "--prune"
)

// Report outcomes. `unreadable` is the one that is not an action: it names a
// file this build could not decode, which blocks every launch until it is
// settled and must therefore appear in the report rather than only in a log.
const (
	repairOutcomeRefused    = "refused"
	repairOutcomeUnreadable = "unreadable"
	// repairOutcomeSetAside is what settling an unreadable record does: the
	// file is RENAMED out of the enumerated set, never unlinked, because this
	// is the one arm that cannot say what it is acting on.
	repairOutcomeSetAside      = "set-aside"
	repairOutcomeWouldSetAside = "would-set-aside"
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
	Ref        string `json:"ref"`
	RecordPath string `json:"record_path"`
	FromPhase  string `json:"from_phase"`
	ToPhase    string `json:"to_phase,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// WindowLive and WorkspaceExists are POINTERS because nil means unknown,
	// which is a real state on the unreadable arm: a record this build cannot
	// decode may carry no readable ref to derive a window from and no readable
	// workspace to stat. A plain bool there asserted `false` about a clean room
	// that demonstrably existed — a report making a positive claim it could not
	// support, which is worse than admitting the gap.
	WindowLive      *bool  `json:"window_live,omitempty"`
	WorkspaceExists *bool  `json:"workspace_exists,omitempty"`
	Outcome         string `json:"outcome"`
	Error           string `json:"error,omitempty"`
}

// RepairReport is what `pr repair` returns and `--json` encodes.
type RepairReport struct {
	Items []RepairItem `json:"items"`
}

// boolPtr is the observation constructor: a KNOWN true or false, as opposed to
// the nil that means nobody could find out.
func boolPtr(v bool) *bool { return &v }

// observedWindow renders one liveness observation: nil when tmux itself could
// not be read, because an unreadable window list says nothing about any
// individual window and reporting `false` there would flag every healthy review
// as dead.
func observedWindow(tmuxOK, live bool) *bool {
	if !tmuxOK {
		return nil
	}
	return &live
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
			WindowLive:      observedWindow(tmuxOK, windowLive[s.Ref()]),
			WorkspaceExists: boolPtr(s.IsWorkspaceLive()),
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
		return c.repairUndecodableLocked(ctx, opts, member, decodeErr)
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
		WorkspaceExists: boolPtr(avail == workspaceAvailabilityLive),
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
// IT SETS THE RECORD ASIDE RATHER THAN REMOVING IT. "Cannot decode" is a wider
// class than a torn write: validateLifecycleFields refuses any record whose
// version this build does not know, so an INTACT record written by a newer
// forgectl — live clean room, live window, phase active — arrives here too.
// A rename out of the `.json` namespace clears the block just as well as an
// unlink and destroys nothing.
//
// Three things guard it, because this is the only arm that cannot prove what it
// is acting on:
//
//   - a BEST-EFFORT ref read from the raw bytes, used for one thing only —
//     refusing while that ref's window is live. It is never trusted for
//     anything else, and a ref that does not parse simply means the check
//     cannot run.
//   - the same confirmation gate as --rollback (--yes off a terminal), because
//     the ordinary forget earns its ungated pass by proving absence first and
//     this one proves nothing.
//   - an audit row carrying the path, the extracted ref, and the record's own
//     bytes, capped and clamped — for the one removal that cannot say what it
//     removed, an empty trail is the worst possible outcome.
func (c *Client) repairUndecodableLocked(ctx context.Context, opts RepairOpts, member breadcrumbMember, decodeErr error) (RepairItem, error) {
	item := RepairItem{
		RecordPath: member.path,
		FromPhase:  repairPhaseUnreadable,
		Outcome:    repairOutcomeRefused,
		Error:      termsafe.SafeLine(decodeErr.Error()),
	}
	if !opts.ForgetIfAbsent {
		return item, fmt.Errorf("this build cannot read session record %s, so it cannot adopt or roll it back: %w — "+
			"set it aside with 'forgectl pr repair %s --apply %s', which moves the record out of the way without reading it",
			member.displayPath, decodeErr, member.displayPath, RepairModeForgetIfAbsent)
	}

	// BEST EFFORT, and bounded to one question. A newer-version record is the
	// undecodable subclass that actually matters, and it carries a plain "ref"
	// string; refusing while that window is live closes the case where this arm
	// would otherwise hide a running session from the build that owns it.
	ref, refKnown := refFromRawRecord(member.bytes)
	if !refKnown {
		// The liveness refusal below is the only guard that reads the record at
		// all, so a record yielding no ref gets NO liveness check — not a
		// passing one. Saying so is the whole fix: item.WindowLive stays nil
		// (the report renders "?"), the log says why, and the confirmation
		// prompt carries the same sentence. Refusing instead would be worse: a
		// torn write is the canonical corrupt record, this is the only verb that
		// can clear it, and refusing would send the operator back to `rm`.
		slog.Warn("Setting aside a session record with no readable ref; whether its review window is live was not checked.",
			"path", member.path, "error", decodeErr)
	}
	if refKnown {
		item.Ref = ref.String()
		live, tmuxOK := c.WindowLive(ctx, ref)
		if !tmuxOK {
			return item, fmt.Errorf("refusing to set %s aside: the tmux window list could not be read, "+
				"and an unreadable list is not an absent window — check `tmux list-windows -a`, then retry",
				member.displayPath)
		}
		item.WindowLive = &live
		if live {
			return item, fmt.Errorf("refusing to set %s aside: it names %s, whose review window is still live — "+
				"this record was written by a build that reads a record format this one does not, so the session is "+
				"running and only that build can settle it", member.displayPath, ref.String())
		}
	}

	item.ToPhase = "set aside"
	// The preview precedes the gate: --dry-run mutates nothing.
	if opts.DryRun {
		item.Outcome = repairOutcomeWouldSetAside
		return item, nil
	}
	if !opts.Yes {
		if !c.isTTY() {
			return item, fmt.Errorf("refusing to set %s aside without confirmation: this build cannot read the record, "+
				"so it cannot say what the record described, and there is no terminal to confirm on — pass --yes to proceed",
				member.displayPath)
		}
		approved, err := c.confirmRemoval(setAsidePrompt(member, decodeErr, refKnown))
		if err != nil {
			return item, fmt.Errorf("set-aside confirmation: %w", err)
		}
		if !approved {
			item.Outcome = "declined"
			return item, nil
		}
	}

	slog.Warn("Setting aside a session record this build cannot read; whether it named a clean room cannot be checked.",
		"path", member.path, "error", decodeErr)
	row := RepairRow{
		Ref:         item.Ref,
		RecordPath:  member.path,
		FromPhase:   repairPhaseUnreadable,
		Mode:        RepairModeForgetIfAbsent,
		Record:      cappedRecordBytes(member.bytes),
		RecordBytes: len(member.bytes),
	}
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		return item, err
	}
	aside, err := c.setAsideUndecodableRecord(member)
	if err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, row, err)
		return item, err
	}
	c.completeRepairRow(rowID, row, nil)
	item.Outcome = repairOutcomeSetAside
	item.ToPhase = aside
	slog.Info("Successfully set aside a session record this build could not read.",
		"was", member.path, "now", aside)
	return item, nil
}

// maxAuditRecordBytes caps how much of an unreadable record's own bytes ride in
// the audit row. The row must stay inside the log's line limit, and the point is
// to preserve enough to identify what was set aside — not to mirror the file.
const maxAuditRecordBytes = 4 << 10

// cappedRecordBytes is the audit payload for a record nothing can decode: its
// raw bytes, capped and terminal-clamped. This is the whole reason the trail is
// not empty for the one case it was built for — no other field can name what
// was set aside, because nothing could read it.
//
// The cap is applied twice: once to the raw bytes, and again after clamping,
// because escaping a control-heavy record expands it. Truncating on a byte
// boundary can split a rune; the value is evidence for a human, never parsed,
// and the row must fit the log's line limit.
// Both cuts land on a rune boundary. The raw cut is where a multi-byte
// sequence could be split, and SafeLine would fold the orphan to U+FFFD; the
// clamped cut is where json.Marshal would do the same. Either is the one silent
// byte rewrite this payload exists to avoid, so the invariant is held in both
// places rather than in one.
func cappedRecordBytes(raw []byte) string {
	clamped := termsafe.SafeLine(truncateString(string(raw), maxAuditRecordBytes))
	return truncateString(clamped, maxAuditRecordBytes)
}

// refFromRawRecord reads just enough of an unparseable record to ask whether a
// window is live: the ref, and the local flag the window name derives from.
//
// It is DELIBERATELY the tolerant decoder — a plain Unmarshal, no
// duplicate-key rejection, no unknown-field refusal — which is the opposite of
// what decodeBreadcrumb does, and is safe for exactly one reason: the result is
// used only to REFUSE. It never reaches an argv, a path, or a write. A record
// that lies here can only cause a refusal to set itself aside, which is the
// direction that costs nothing.
//
// IT OFTEN FINDS NOTHING, and a ref-less record still proceeds. What it yields,
// measured against the literal body below:
//
//	torn write (the canonical corrupt record)   no ref — Unmarshal fails
//	intact record from a newer forgectl         a ref — the field is plain and present
//	a nested or renamed "ref" field             no ref — the shallow struct misses it
//	an empty or null "ref"                      no ref — ParseRef refuses
//	a ref that is not owner/repo#N              no ref — Complete() refuses
//
// So the case this check exists for — the newer build's live session — is the
// one case it reliably answers, and the torn write is the one it reliably
// cannot. Refusing on "no ref" would therefore refuse exactly the record class
// `--forget-if-absent` was built to clear, leaving `rm` as the only escape
// again. The caller proceeds and SAYS SO instead, in the log and in the
// confirmation prompt.
func refFromRawRecord(data []byte) (Ref, bool) {
	var shallow struct {
		Ref   string `json:"ref"`
		Local bool   `json:"local"`
	}
	if err := json.Unmarshal(data, &shallow); err != nil || shallow.Ref == "" {
		return Ref{}, false
	}
	ref, err := ParseRef(shallow.Ref)
	if err != nil || !ref.Complete() {
		return Ref{}, false
	}
	if shallow.Local {
		ref = ref.asLocal()
	}
	return ref, true
}

// setAsidePrompt is what the confirmation gate shows for a record nothing can
// read. It says plainly what is and is not known, because that uncertainty is
// the entire reason this arm has a gate at all.
// refKnown is what changes the prompt. When false, the one guard that reads the
// record could not run at all, and the human approving the move deserves that
// sentence rather than a prompt that reads identically to the checked case.
func setAsidePrompt(member breadcrumbMember, decodeErr error, refKnown bool) string {
	prompt := fmt.Sprintf("Set aside a session record this build cannot read?\n"+
		"  record: %s\n"+
		"  reason: %s\n"+
		"  the file is renamed, not deleted — but whether it named a clean room cannot be checked",
		member.displayPath, termsafe.SafeLine(decodeErr.Error()))
	if !refKnown {
		prompt += "\n  no ref could be read, so whether its review window is live was not checked"
	}
	return prompt
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
			ref.String(), termsafe.QuotePath(bc.Workspace), member.displayPath, RepairModeRollback)
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
	item.WindowLive = boolPtr(true)
	item.ToPhase = string(PhaseActive)
	if dryRun {
		item.Outcome = "would-adopt"
		return item, nil
	}
	row := repairRowFor(member, ref, RepairModeAdoptWindow, adopted.WindowID)
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.writeAdoptedRecord(member.path, bc, adopted.WindowID); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, row, err)
		return item, err
	}
	c.completeRepairRow(rowID, row, nil)
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
	// The legacy branch bypasses transitionOnce, so it does not inherit that
	// function's read-and-write-name-the-same-file guard. Its one caller passes
	// an already-resolved member path; this is the backstop a second caller
	// would otherwise be missing.
	if err := c.assertDirectSessionsDirEntry(path); err != nil {
		return err
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
	item.WindowLive = &live
	if live {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to roll back %s: its review window is still live — "+
			"adopt it with 'forgectl pr repair %s --apply %s', or close the window first",
			ref.String(), member.displayPath, RepairModeAdoptWindow)
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
	row := repairRowFor(member, ref, RepairModeRollback, "")
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.teardownLocked(ctx, member.path); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, row, err)
		slog.Error("A repair rollback failed partway; the clean room is recoverable from the repair audit log.",
			"ref", ref.String(), "workspace", bc.Workspace, "log", c.repairLogPath(), "error", err)
		return item, fmt.Errorf("roll back %s: %w — its clean room %s is named in %s",
			ref.String(), err, termsafe.QuotePath(bc.Workspace), termsafe.QuotePath(c.repairLogPath()))
	}
	c.completeRepairRow(rowID, row, nil)
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
	item.WindowLive = &live
	if live {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to forget %s: its review window still exists — "+
			"adopt it with 'forgectl pr repair %s --apply %s'", ref.String(), member.displayPath, RepairModeAdoptWindow)
	}
	// Forget's whole claim is that nothing is left behind. Live means a clean
	// room is; Invalid means SOMETHING is at that path and this build cannot say
	// what — neither is an absence, and both must refuse before the intent row.
	if bc.Workspace != "" && avail != workspaceAvailabilityMissing {
		item.Outcome = repairOutcomeRefused
		return item, fmt.Errorf("refusing to forget %s: its recorded workspace %s is not cleanly absent, and forgetting "+
			"the record would leave it with nothing pointing at it — use 'forgectl pr repair %s --apply %s'",
			ref.String(), termsafe.QuotePath(bc.Workspace), member.displayPath, RepairModeRollback)
	}
	item.ToPhase = "removed"
	if opts.DryRun {
		item.Outcome = "would-forget"
		return item, nil
	}
	row := repairRowFor(member, ref, RepairModeForgetIfAbsent, "")
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		item.Outcome = repairOutcomeRefused
		return item, err
	}
	if err := c.teardownLocked(ctx, member.path); err != nil {
		item.Outcome = repairOutcomeFailed
		item.Error = err.Error()
		c.completeRepairRow(rowID, row, err)
		return item, fmt.Errorf("forget %s: %w", ref.String(), err)
	}
	c.completeRepairRow(rowID, row, nil)
	item.Outcome = "forgotten"
	slog.Info("Successfully discarded a session record whose window and clean room were both gone.",
		"ref", ref.String(), "path", member.path)
	return item, nil
}

// beginRepairRow writes the write-ahead intent. The caller supplies the
// descriptive fields; this stamps the timestamp, the row id, the actor, and the
// outcome, so those four can never be spelled differently by two call sites.
//
// A failure here REFUSES the mutation rather than proceeding without a trail:
// the row is the recovery pointer, so a removal with no row is the one shape
// that can lose a clean room outright.
func (c *Client) beginRepairRow(row RepairRow) (string, error) {
	id, err := randomSuffix()
	if err != nil {
		return "", fmt.Errorf("derive repair audit row id: %w", err)
	}
	row.TS = time.Now().UTC()
	row.ID = id
	row.Actor = repairActor()
	row.Outcome = repairOutcomeIntent
	row.Error = ""
	if err := c.appendRepairRowLocked(row); err != nil {
		return "", fmt.Errorf("record the repair intent before acting: %w — nothing was changed", err)
	}
	return id, nil
}

// completeRepairRow closes out an intent. Its own failure cannot undo the
// mutation that already happened, so it is logged rather than returned — the
// dangling intent row is itself the honest record of that.
func (c *Client) completeRepairRow(id string, row RepairRow, cause error) {
	row.TS = time.Now().UTC()
	row.ID = id
	row.Actor = repairActor()
	row.Outcome = repairOutcomeApplied
	row.Error = ""
	if cause != nil {
		row.Outcome = repairOutcomeFailed
		row.Error = termsafe.SafeLine(cause.Error())
	}
	if err := c.appendRepairRowLocked(row); err != nil {
		slog.Error("Failed to complete a repair audit row; the intent row is left dangling, which is the honest record.",
			"id", id, "ref", row.Ref, "error", err)
	}
}

// repairRowFor builds the descriptive half of an audit row for a decodable
// record, so the three arms cannot disagree about what a row carries.
func repairRowFor(member breadcrumbMember, ref Ref, mode, windowID string) RepairRow {
	return RepairRow{
		Ref:        ref.String(),
		RecordPath: member.path,
		FromPhase:  string(member.breadcrumb.Phase),
		Mode:       mode,
		WindowID:   windowID,
		Workspace:  member.breadcrumb.Workspace,
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
