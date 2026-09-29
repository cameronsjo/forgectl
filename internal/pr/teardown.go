package pr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/sandbox"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// sandboxTeardown is the workspace-removal seam. Production wires the real
// call; tests inject a mid-teardown failure, which is the only portable way to
// stage a LIVE branch that fails after mutation has begun — FakeRunner cannot
// force it, since sandbox.Teardown's failures come from the filesystem rather
// than from a Runner call. Tests must restore it and must not run in parallel
// while overriding it. Mirrors the classifier seams in workspace_state.go.
var sandboxTeardown = sandbox.Teardown

// staleMemberIsRegular is the mode-check seam. Production is exactly
// FileInfo.Mode().IsRegular; the test override forces only that verdict after
// the real pinned-root Lstat and SameFile checks have passed, so coverage does
// not depend on whether the host filesystem recycles an inode into a symlink.
var staleMemberIsRegular = func(info fs.FileInfo) bool { return info.Mode().IsRegular() }

// Teardown discards the review session recorded at path.
//
// path MUST resolve to a member of the current breadcrumb set — a
// set-membership check, never a glob or a prefix match — so code under review
// cannot invoke teardown against an arbitrary path. Membership yields the
// AUTHORITATIVE file (see resolveBreadcrumbMember); nothing downstream acts on
// the caller's operand.
//
// Two branches, decided by the recorded workspace's state, and the order is
// load-bearing:
//
//	membership -> record validation -> workspace classification
//	  missing -> identity/byte/field recheck -> breadcrumb-ONLY unlink
//	  live    -> strict live reload -> bounded tmux kill (a timeout parks the
//	          record in needs-repair and stops) -> quarantine restore
//	          -> sandbox teardown -> breadcrumb unlink
//	  invalid -> refusal
//
// The stale decision happens BEFORE any live teardown facility, and the live
// branch never falls back to the stale unlink once quarantine restore, sandbox
// teardown, or tmux action has begun. A live-to-missing race can fail and leak
// through the existing error handling, but it cannot cross branches after
// mutation has started.
func (c *Client) Teardown(ctx context.Context, path string) error {
	// The lifecycle lock is held across membership, classification, and the
	// final unlink so no forgectl process can race this one. It is
	// non-reentrant: Cleanup takes it ONCE and calls auditedTeardownLocked per
	// candidate rather than re-entering here.
	return c.withLifecycleLock(ctx, "teardown", func() error {
		return c.auditedTeardownLocked(ctx, auditVerbTeardown, path, newTmuxBudget())
	})
}

// teardownKind is which of the three removal arms a planned teardown will take.
type teardownKind int

const (
	teardownKindRecordOnly teardownKind = iota
	teardownKindStale
	teardownKindLive
)

// teardownPlan is everything a teardown decided before it touched anything:
// which record it is authorized to act on, which arm acts, and — for the live
// arm only — the session reloaded through the authoritative path.
type teardownPlan struct {
	member breadcrumbMember
	kind   teardownKind
	sess   Session
}

// planTeardownLocked runs every check and every refusal, and mutates nothing.
//
// It exists so the audited wrapper can write its intent row AFTER the last
// refusal: a dangling intent means a removal died mid-way, so a refused
// teardown that wrote one would forge that signal.
func (c *Client) planTeardownLocked(path string) (teardownPlan, error) {
	member, err := c.resolveBreadcrumbMember(path)
	if err != nil {
		return teardownPlan{}, err
	}

	// A record that legitimately has no workspace — queued, or a reservation
	// that never got its clean room — is classified NONE rather than missing,
	// and there is nothing to restore, kill, or remove but the record itself.
	// It is checked before classifyWorkspace because an empty pathname is not a
	// pathname that went away.
	if member.breadcrumb.Workspace == "" {
		return teardownPlan{member: member, kind: teardownKindRecordOnly}, nil
	}
	avail, availErr := classifyWorkspace(member.breadcrumb.Workspace)
	switch avail {
	case workspaceAvailabilityMissing:
		return teardownPlan{member: member, kind: teardownKindStale}, nil
	case workspaceAvailabilityLive:
		// Strict live reload through the authoritative path — never the
		// operand — so the session acted on is the record just verified.
		sess, err := c.loadSession(member.path)
		if err != nil {
			return teardownPlan{}, err
		}
		return teardownPlan{member: member, kind: teardownKindLive, sess: sess}, nil
	default:
		return teardownPlan{}, fmt.Errorf("cannot tear down breadcrumb %s: %w", member.displayPath, availErr)
	}
}

// executeTeardownLocked performs the arm the plan chose. The discard functions
// re-prove every fact they act on, so a drift observed here is a refusal after
// the plan rather than a check the plan skipped.
//
// budget is the tmux time the live arm's window kill may draw on; a sweep
// passes the one it shares across every candidate.
func (c *Client) executeTeardownLocked(ctx context.Context, plan teardownPlan, budget *tmuxBudget) error {
	switch plan.kind {
	case teardownKindRecordOnly:
		return c.discardRecordOnly(plan.member)
	case teardownKindStale:
		return c.discardStale(plan.member)
	case teardownKindLive:
		return c.discard(ctx, plan.sess, budget)
	default:
		return fmt.Errorf("cannot tear down breadcrumb %s: unknown teardown arm", plan.member.displayPath)
	}
}

// teardownLocked is Teardown's core for a caller that already holds the
// lifecycle lock.
//
// IT WRITES NO AUDIT ROW, on purpose. Its callers are `pr repair --rollback`
// and `pr repair --forget-if-absent` (repair.go), and each has already written
// its own intent row before calling here. A row written here would nest inside
// that pair, giving one mutation two intents and two completions — which is the
// exact shape that means "a removal died mid-way" to whoever reads the trail.
// The verbs that own their mutation outright go through auditedTeardownLocked.
func (c *Client) teardownLocked(ctx context.Context, path string) error {
	plan, err := c.planTeardownLocked(path)
	if err != nil {
		return err
	}
	return c.executeTeardownLocked(ctx, plan, newTmuxBudget())
}

// auditedTeardownLocked is the teardown core plus the write-ahead pair, for the
// verbs whose mutation nothing else is recording: `pr teardown` and
// `pr cleanup`.
//
// THE ORDER IS THE POINT. Every refusal lives in the plan, which runs first, so
// a refused teardown writes nothing at all. Once the intent row is on disk the
// removal is underway, so a drift refusal raised inside discardStale or
// discardRecordOnly completes the pair as `failed` rather than leaving a
// dangling intent — a dangling intent is reserved for a removal that died
// without being able to say so.
//
// A failure to write the intent row REFUSES the mutation, matching the repair
// arms: the row is the only pointer left to a clean room once the record is
// gone.
//
// A live teardown whose tmux budget is already spent is SKIPPED here, with the
// other refusals and before the intent row: tmux stopped answering earlier in
// the same sweep, so asking again would only spend the lock hold on a call
// that will not return, and the record, window and workspace are all left
// exactly as they were.
func (c *Client) auditedTeardownLocked(ctx context.Context, verb, path string, budget *tmuxBudget) error {
	plan, err := c.planTeardownLocked(path)
	if err != nil {
		return err
	}
	if plan.kind == teardownKindLive && budget.exhausted() {
		return ErrTmuxBudgetSpent
	}
	row := teardownRowFor(verb, plan.member)
	if plan.kind == teardownKindLive {
		// The live arm deletes the workspace loadSession read on its second
		// pass over the record, not the one the member's first read carried.
		// The row is the recovery pointer once the record is gone, so it names
		// the path that is actually removed.
		row.Workspace = plan.sess.Workspace
	}
	rowID, err := c.beginRepairRow(row)
	if err != nil {
		return err
	}
	execErr := c.executeTeardownLocked(ctx, plan, budget)
	c.completeRepairRow(rowID, row, execErr)
	return execErr
}

// teardownRowFor builds the descriptive half of a teardown or cleanup row, so
// the two verbs cannot disagree about what a row carries.
//
// Mode stays empty: it holds `pr repair`'s flag spelling, and filling it in here
// would make the trail claim a repair ran. Record and RecordBytes stay empty
// too — those carry the raw bytes of a record that could not be DECODED, and
// every field here comes from a decode that succeeded.
func teardownRowFor(verb string, member breadcrumbMember) RepairRow {
	return RepairRow{
		Verb:       verb,
		Ref:        member.breadcrumb.Ref,
		RecordPath: member.path,
		FromPhase:  string(member.breadcrumb.Phase),
		WindowID:   member.breadcrumb.WindowID,
		Workspace:  member.breadcrumb.Workspace,
	}
}

// resolvePath returns the symlink-resolved absolute form of path, falling back
// to a lexical Clean+Abs when resolution fails (a not-yet-created path).
func resolvePath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// discardStale removes a breadcrumb whose workspace is gone — and removes
// NOTHING else.
//
// This is the one path in forgectl that deletes a file on the strength of a
// record rather than a live sandbox, so it re-proves every fact it is acting
// on immediately before acting. Everything membership captured is compared
// against the filesystem again, and every step runs through ONE pinned
// directory handle opened at the top:
//
//	os.OpenRoot(sessionsDir)             -> pin the directory for every step below
//	Lstat "." through the handle         -> SameFile as at check time
//	Lstat the member's base name         -> SameFile as at check time
//	re-read that name through the handle -> byte-identical
//	re-decode and re-validate            -> every security field identical
//	re-classify the recorded workspace   -> still cleanly missing
//	Remove that same base name
//
// PINNED HANDLE, UNRESOLVED NAME — and the pin is what makes the directory
// identity check load-bearing. The earlier form Lstat'd
// EvalSymlinks(c.sessionsDir) but unlinked through the UNRESOLVED
// c.sessionsDir: the object checked and the object acted through were
// different by construction, not merely racing, so a directory swap could
// pass the check and still be deleted from. os.Root closes that: every check
// and the unlink traverse the same file descriptor, and the unlink names only
// the member's base name.
//
// This does NOT conflict with the VALIDATE RESOLVED, ACT UNRESOLVED convention
// spelled out on validateWorkspace. That warning is about following a symlink
// to its target; os.Root REFUSES an escaping symlink rather than following it,
// so acting through the handle can never widen into a deletion elsewhere.
// OpenRoot resolves c.sessionsDir itself in the ordinary way, so a symlinked
// session directory remains supported.
//
// Any drift refuses: an identity mismatch, a symlink swapped in, a byte or
// field change, the record disappearing or being recreated, the parent
// directory swapped, an invalid decode, the workspace REAPPEARING, or any
// permission or I/O error. A refusal leaks a breadcrumb; deleting on
// uncertainty loses a record that may still describe a real review. Leaking is
// recoverable, so leaking is the failure mode chosen.
//
// It issues ZERO Runner calls and performs no quarantine restore, sandbox
// teardown, git, tmux, workspace write, or workspace removal. There is nothing
// to restore — the workspace is already gone — and a stale record must never
// be able to reach a facility that deletes directories.
//
// HONEST RESIDUAL: the pinned handle removes the parent-rename race, but Go
// still has no compare-and-unlink for the FILE itself. A same-uid actor can
// replace the breadcrumb between the final Lstat and Remove, and the unlink
// would take the replacement. That is accepted, not overlooked: the session
// directory is 0700, so such an actor can already unlink or rewrite the
// breadcrumb directly without going through forgectl. This protocol prevents
// benign in-process races and refuses observed drift; it does not claim to
// defeat a hostile same-uid concurrent writer.
//
// A second, narrower residual sits inside the identity check itself: SameFile
// compares dev+ino, and an inode number is unique only among LIVE files. A
// filesystem may hand a just-unlinked number back to the next create — ext4
// routinely does, APFS does not — so a replacement can land on the original
// identity. That is contained rather than dangerous, but by two separate
// facts, and they are worth keeping apart. The checks decide WHETHER to
// unlink, never WHAT is unlinked: Remove is a fresh name resolution after the
// last Lstat, so under the residual above it takes whatever sits at the name
// at that instant. What the byte and record comparisons below prove is that
// the only file these checks can APPROVE is one holding exactly the authorized
// record. What bounds the unlink itself is separate and unconditional:
// root.Remove is handed filepath.Base(member.path) and nothing else, through a
// directory handle whose identity was verified above — so the deletion can
// never leave that one name in that one directory, whatever landed there.
// There is no portable strengthening available; the identity check earns its
// place against the replacements that differ, which is every case an operator
// or a bug produces.
func (c *Client) discardStale(member breadcrumbMember) error {
	slog.Debug("Preparing to discard a stale review breadcrumb.",
		"ref", member.breadcrumb.Ref, "path", member.path)

	root, err := os.OpenRoot(c.sessionsDir)
	if err != nil {
		return fmt.Errorf("pin pr sessions dir %s: %w", c.sessionsDir, err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil {
			slog.Debug("Failed to close the pinned pr sessions dir handle.", "error", cerr)
		}
	}()

	dirInfo, err := root.Lstat(".")
	if err != nil {
		return fmt.Errorf("re-stat pr sessions dir: %w", err)
	}
	if !os.SameFile(dirInfo, member.dirInfo) {
		return fmt.Errorf("pr sessions dir %s changed identity during teardown; refusing to remove %s",
			c.sessionsDir, member.displayPath)
	}

	// Membership already proved this entry sits directly in the canonical
	// session directory, so its base name is the exact name to operate on
	// through the pinned handle — and a base name cannot escape it.
	name := filepath.Base(member.path)

	// A member that vanished before this check is refused rather than treated
	// as an already-successful removal: a concurrently completed teardown can
	// report its own success, and silently succeeding here would hide a
	// replacement race.
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("re-stat breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	if !os.SameFile(info, member.info) {
		return fmt.Errorf("breadcrumb %s changed identity during teardown; refusing to remove it", member.displayPath)
	}
	if !staleMemberIsRegular(info) {
		return fmt.Errorf("breadcrumb %s is no longer a regular file; refusing to remove it", member.displayPath)
	}

	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	data, readErr := readBreadcrumbBytes(file)
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(readErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close breadcrumb %s after re-read: %w", member.displayPath, termsafe.Error(closeErr))
	}
	if !bytes.Equal(data, member.bytes) {
		return fmt.Errorf("breadcrumb %s changed on disk during teardown; refusing to remove it", member.displayPath)
	}
	bc, err := decodeBreadcrumbRecord(data, member.path)
	if err != nil {
		return fmt.Errorf("re-validate breadcrumb before removal: %w", err)
	}
	if !sameBreadcrumbRecord(bc, member.breadcrumb) {
		return fmt.Errorf("breadcrumb %s decoded differently during teardown; refusing to remove it", member.displayPath)
	}

	// The authority for this deletion is the workspace's absence, so re-prove
	// it last, closest to the unlink. A workspace that came back means the
	// record is live again and must not be discarded as stale.
	avail, availErr := classifyWorkspace(bc.Workspace)
	if avail != workspaceAvailabilityMissing {
		// classifyWorkspace returns a NIL error for Live, and Live is the most
		// likely way to reach this refusal — the workspace reappeared between
		// the classification that chose this branch and the unlink. Wrapping
		// unconditionally rendered that operator-facing message as
		// "%!w(<nil>)", so the cause is wrapped only when there is one.
		if availErr == nil {
			return fmt.Errorf("workspace for breadcrumb %s is no longer cleanly absent; refusing to remove it",
				member.displayPath)
		}
		return fmt.Errorf("workspace for breadcrumb %s is no longer cleanly absent; refusing to remove it: %w",
			member.displayPath, availErr)
	}

	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	slog.Info("Successfully discarded a stale review breadcrumb.", "ref", bc.Ref)
	return nil
}

// discardRecordOnly removes a record that never had a workspace — a queued
// entry, or a reservation whose clean room was never created.
//
// It runs the same pinned-handle identity protocol discardStale does, minus
// the workspace re-classification, and it re-proves the one fact that
// authorizes it: the record STILL carries no workspace. That re-proof is the
// whole safety argument. Without it, a record that gained a workspace between
// the classification and the unlink would have its only pointer deleted, which
// is exactly the leak the stale path exists to prevent.
//
// It issues ZERO Runner calls and performs no quarantine restore, sandbox
// teardown, git, or tmux action. There is nothing to act on.
func (c *Client) discardRecordOnly(member breadcrumbMember) error {
	slog.Debug("Preparing to discard a session record with no workspace.",
		"ref", member.breadcrumb.Ref, "path", member.path, "phase", string(member.breadcrumb.Phase))

	root, err := os.OpenRoot(c.sessionsDir)
	if err != nil {
		return fmt.Errorf("pin pr sessions dir %s: %w", c.sessionsDir, err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil {
			slog.Debug("Failed to close the pinned pr sessions dir handle.", "error", cerr)
		}
	}()

	dirInfo, err := root.Lstat(".")
	if err != nil {
		return fmt.Errorf("re-stat pr sessions dir: %w", err)
	}
	if !os.SameFile(dirInfo, member.dirInfo) {
		return fmt.Errorf("pr sessions dir %s changed identity during teardown; refusing to remove %s",
			c.sessionsDir, member.displayPath)
	}

	name := filepath.Base(member.path)
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("re-stat breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	if !os.SameFile(info, member.info) {
		return fmt.Errorf("breadcrumb %s changed identity during teardown; refusing to remove it", member.displayPath)
	}
	if !staleMemberIsRegular(info) {
		return fmt.Errorf("breadcrumb %s is no longer a regular file; refusing to remove it", member.displayPath)
	}

	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	data, readErr := readBreadcrumbBytes(file)
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(readErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close breadcrumb %s after re-read: %w", member.displayPath, termsafe.Error(closeErr))
	}
	if !bytes.Equal(data, member.bytes) {
		return fmt.Errorf("breadcrumb %s changed on disk during teardown; refusing to remove it", member.displayPath)
	}
	bc, err := decodeBreadcrumbRecord(data, member.path)
	if err != nil {
		return fmt.Errorf("re-validate breadcrumb before removal: %w", err)
	}
	if !sameBreadcrumbRecord(bc, member.breadcrumb) {
		return fmt.Errorf("breadcrumb %s decoded differently during teardown; refusing to remove it", member.displayPath)
	}
	if bc.Workspace != "" {
		return fmt.Errorf("breadcrumb %s now names a workspace; refusing to remove it as a record-only entry",
			member.displayPath)
	}

	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	slog.Info("Successfully discarded a session record that had no workspace.", "ref", bc.Ref)
	return nil
}

// unreadableSuffix is what an undecodable record's name becomes when it is set
// aside. The extension is deliberately no longer ".json": every enumeration in
// this package filters on that (listLocked, resolveBreadcrumbEntry,
// recordedWorkspaceFor), so the rename clears the block on admission without
// destroying anything.
const unreadableSuffix = ".unreadable-"

// setAsideUndecodableRecord RENAMES a record this build cannot decode out of
// the enumerated set. It is reachable only through
// `pr repair --apply --forget-if-absent`.
//
// IT DOES NOT UNLINK, and that is the whole design. "Cannot decode" is not only
// a torn write: validateLifecycleFields refuses any record whose version this
// build does not know, so an INTACT record written by a newer forgectl — live
// clean room, live window — lands here too. Deleting it would orphan a
// directory with nothing naming it (the exact leak discardStale exists to
// prevent) and make a newer build's session invisible to the build that owns
// it. A rename costs nothing, clears the block just as well, and leaves the
// bytes for whoever can read them.
//
// It runs the same pinned-handle identity protocol as discardStale and
// discardRecordOnly, minus every step that needs a decode. That is the honest
// minimum: the protocol's authority comes from dev+ino identity and byte
// equality, neither of which requires understanding the file. So this proves
// exactly WHICH file it moves while claiming nothing about what the file said.
//
// It returns the new base name so the caller can name it to the operator.
func (c *Client) setAsideUndecodableRecord(member breadcrumbMember) (string, error) {
	slog.Debug("Preparing to set aside a session record this build cannot read.", "path", member.path)

	root, err := os.OpenRoot(c.sessionsDir)
	if err != nil {
		return "", fmt.Errorf("pin pr sessions dir %s: %w", c.sessionsDir, err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil {
			slog.Debug("Failed to close the pinned pr sessions dir handle.", "error", cerr)
		}
	}()

	dirInfo, err := root.Lstat(".")
	if err != nil {
		return "", fmt.Errorf("re-stat pr sessions dir: %w", err)
	}
	if !os.SameFile(dirInfo, member.dirInfo) {
		return "", fmt.Errorf("pr sessions dir %s changed identity during repair; refusing to move %s",
			c.sessionsDir, member.displayPath)
	}

	name := filepath.Base(member.path)
	info, err := root.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("re-stat breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	if !os.SameFile(info, member.info) {
		return "", fmt.Errorf("breadcrumb %s changed identity during repair; refusing to move it", member.displayPath)
	}
	if !staleMemberIsRegular(info) {
		return "", fmt.Errorf("breadcrumb %s is no longer a regular file; refusing to move it", member.displayPath)
	}

	file, err := root.Open(name)
	if err != nil {
		return "", fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	data, readErr := readBreadcrumbBytes(file)
	closeErr := file.Close()
	if readErr != nil {
		return "", fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(readErr))
	}
	if closeErr != nil {
		return "", fmt.Errorf("close breadcrumb %s after re-read: %w", member.displayPath, termsafe.Error(closeErr))
	}
	if !bytes.Equal(data, member.bytes) {
		return "", fmt.Errorf("breadcrumb %s changed on disk during repair; refusing to move it", member.displayPath)
	}
	// A file that became READABLE between the report and the apply is no longer
	// the thing this verb was authorized for — byte equality above already
	// refuses that, and this is the assertion that says so out loud.
	if _, err := decodeBreadcrumbRecord(data, member.path); err == nil {
		return "", fmt.Errorf("breadcrumb %s is readable after all; settle it as an ordinary record rather than setting it aside",
			member.displayPath)
	}

	aside, err := c.freeAsideName(root, name)
	if err != nil {
		return "", err
	}
	if err := root.Rename(name, aside); err != nil {
		return "", fmt.Errorf("set breadcrumb %s aside: %w", member.displayPath, termsafe.Error(err))
	}
	slog.Info("Successfully set an unreadable session record aside; its bytes are preserved under a new name.",
		"was", member.path, "now", filepath.Join(c.sessionsDir, aside))
	return aside, nil
}

// freeAsideName picks the set-aside name, and refuses to clobber. The
// second-resolution timestamp alone can collide when two records are settled
// inside one second, and this function exists so that collision costs a random
// suffix rather than the other record's bytes.
func (c *Client) freeAsideName(root *os.Root, name string) (string, error) {
	candidate := name + unreadableSuffix + strconv.FormatInt(time.Now().UTC().Unix(), 10)
	if _, err := root.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
		return candidate, nil
	}
	suffix, err := randomSuffix()
	if err != nil {
		return "", fmt.Errorf("derive a free set-aside name for %s: %w", termsafe.QuotePath(name), err)
	}
	candidate += "-" + suffix
	if _, err := root.Lstat(candidate); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("cannot find a free set-aside name for %s; %s is taken",
			termsafe.QuotePath(name), termsafe.QuotePath(candidate))
	}
	return candidate, nil
}

// sameBreadcrumbRecord reports whether two decoded records agree on every
// field that carries security meaning. Byte equality already implies this;
// checking both means a future decoder change cannot quietly widen what
// "unchanged" means.
func sameBreadcrumbRecord(a, b Breadcrumb) bool {
	return a.Workspace == b.Workspace &&
		a.Ref == b.Ref &&
		a.Host == b.Host &&
		a.Agent == b.Agent &&
		a.Local == b.Local &&
		a.CreatedAt.Equal(b.CreatedAt) &&
		a.Provenance == b.Provenance &&
		a.Version == b.Version &&
		a.Phase == b.Phase &&
		a.Revision == b.Revision &&
		a.WindowID == b.WindowID &&
		a.RepairReason == b.RepairReason
}

// ErrWindowKillTimedOut is what a live teardown returns when tmux did not
// answer within lockedTmuxBudget, so it cannot say whether the review window
// still exists. The record has been parked in needs-repair and the workspace
// left in place; nothing was discarded.
var ErrWindowKillTimedOut = errors.New("review window kill timed out (tmux unresponsive)")

// ErrRecordNotParked is wrapped alongside ErrWindowKillTimedOut when the
// timeout left the record exactly as it was because it could not be parked in
// needs-repair (a legacy record with no version, or a failed write). Callers
// must not tell the operator the record was parked when this is present.
var ErrRecordNotParked = errors.New("the record could not be parked in needs-repair")

// ErrTmuxBudgetSpent is what a sweep returns for a live session it did not
// attempt: tmux already failed to answer within the sweep's shared budget, so
// the session was left exactly as it was — record, window and workspace.
var ErrTmuxBudgetSpent = errors.New("skipped: tmux stopped answering earlier in this sweep")

// windowKillTimeoutReason is the needs-repair reason such a record carries,
// followed by the window it could not account for (see unknownWindow).
const windowKillTimeoutReason = "window kill timed out (tmux unresponsive)"

// windowAmbiguousReason is the needs-repair reason a teardown parks with when
// more than one window in the review session carries the review's name.
const windowAmbiguousReason = "more than one review window carries this review's name; close the one that is not this review, then tear it down again"

// killReviewWindow kills the review window if it is still open, within one
// lockedTmuxBudget, and reports whether the window's state is still UNKNOWN
// afterwards. Resolution is exact — the window must carry this review's name
// AND sit under the review session's native id — and the kill revalidates
// that before issuing. A failure to resolve means there is nothing of ours to
// kill, which is the ordinary case after the reviewer exits; it must never
// widen into killing whatever tmux would have matched. Other kill failures stay
// best-effort, as before.
//
// Two outcomes are NOT "nothing to kill", and the caller fails closed on both:
//
//   - timedOut: tmux did not answer within the budget (or the caller
//     cancelled), so the window may be live.
//   - ambiguous: more than one window in the review session carries this
//     review's name (tmux allows that), so resolution refused to pick one and
//     at least one of them is live.
//
// Window names depend only on owner/repo/number, so discarding the record in
// either case would leave an orphan that a later same-ref review, teardown or
// repair could collide with.
//
// The kill stays under the lock on purpose. The window is found by the
// review's NAME under the shared review session, so once the lock is released
// a new admission for the same ref can create a same-named window, and an
// unlocked kill would then take out the live review that replaced this one.
// The budget, not a release, is what keeps a hung tmux from holding the lock.
//
// window names what the caller must record when the state is unknown: the
// derived window name always, plus the window's native id when resolution got
// that far before tmux stopped answering (see unknownWindow).
//
// The bounded context is used ONLY here; the caller's own ctx continues on.
func (c *Client) killReviewWindow(ctx context.Context, ref Ref, budget *tmuxBudget) (timedOut bool, window string, ambiguous error) {
	tctx, done := budget.bound(ctx)
	defer done()
	resolved, err := c.resolveReviewWindow(tctx, ref)
	if err != nil {
		if tctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("Timed out resolving the review window under the lifecycle lock.", "budget", lockedTmuxBudget)
			return true, unknownWindow(ref, ""), nil
		}
		if errors.Is(err, tmux.ErrAmbiguousWindow) {
			slog.Warn("More than one review window carries this review's name; refusing to pick one.",
				"ref", ref.String(), "error", err)
			return false, "", err
		}
		// The name is diagnostic only here; a ref that cannot even be keyed logs
		// as such rather than shadowing the resolve failure being reported.
		name, nameErr := ReviewWindowName(ref)
		if nameErr != nil {
			name = "<no derivable identity>"
		}
		slog.Debug("No review window to kill (already gone).", "window", name, "error", err)
		return false, "", nil
	}
	if err := c.tmuxClient.KillWindow(tctx, resolved); err != nil {
		if tctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("Timed out killing the review window under the lifecycle lock.",
				"window_id", resolved.ID, "budget", lockedTmuxBudget)
			return true, unknownWindow(ref, resolved.ID), nil
		}
		slog.Debug("Review window could not be killed.", "window_id", resolved.ID, "error", err)
	}
	return false, "", nil
}

// unknownWindow names a review window whose state a teardown could not settle,
// for the needs-repair reason and the error (forgectl#648). The record schema
// allows a windowId only on an active record, so the park carries the window
// in its reason — the pointer completeLaunch leaves for the same situation.
// The derived name is always known; nativeID is the "@N" when resolution got
// that far, and is omitted otherwise rather than guessed.
func unknownWindow(ref Ref, nativeID string) string {
	name, err := ReviewWindowName(ref)
	if err != nil {
		name = "<no derivable identity>"
	}
	if nativeID == "" {
		return fmt.Sprintf("review window %s", name)
	}
	return fmt.Sprintf("review window %s (%s)", name, nativeID)
}

// parkForUnknownWindow fails a live teardown closed: it parks the record in
// needs-repair with reason and returns cause, or — when the record cannot be
// parked (none on disk, a legacy record, a failed write) — cause wrapped with
// ErrRecordNotParked so no caller claims a park that never happened. Nothing is
// removed either way. The lock is held, so the *Locked park is the right form.
func (c *Client) parkForUnknownWindow(sess Session, reason string, cause error) error {
	if sess.Path == "" {
		return fmt.Errorf("%w; %w", cause, ErrRecordNotParked)
	}
	if err := c.markNeedsRepairLocked(sess.Path, reason); err != nil {
		return fmt.Errorf("%w; %w: %w", cause, ErrRecordNotParked, err)
	}
	return cause
}

// discard performs the actual teardown for an already-validated session: undo
// the quarantine (recomputed precisely from the sandbox's canonical
// scheme+targets), remove the workspace, kill the window, delete the
// breadcrumb.
func (c *Client) discard(ctx context.Context, sess Session, budget *tmuxBudget) error {
	slog.Debug("Preparing to tear down review session.", "ref", sess.Ref.String(), "workspace", sess.Workspace)

	// The window goes first, so a tmux that cannot answer — or a window name
	// that resolves to more than one window — stops the teardown BEFORE
	// anything is removed. Failing closed here means parking the record in
	// needs-repair with the workspace intact: the record and the clean room are
	// what let the operator find and finish this later.
	timedOut, window, ambiguous := c.killReviewWindow(ctx, sess.Ref, budget)
	if timedOut {
		// The window is named in both the parked reason and the error, so it is
		// recorded even when the park itself fails (a legacy record): the
		// operator reads it on stderr instead.
		return c.parkForUnknownWindow(sess, windowKillTimeoutReason+"; "+window+" may still be running",
			fmt.Errorf("%w: %s may still be running", ErrWindowKillTimedOut, window))
	}
	if ambiguous != nil {
		return c.parkForUnknownWindow(sess, windowAmbiguousReason,
			fmt.Errorf("refusing to tear down %s, nothing was removed: %w", sess.Ref.String(), ambiguous))
	}

	// Restore quarantined files, while the workspace still exists.
	// ExpandTargets is the same call sandboxAndQuarantine made, and it finds a
	// nested target by its RENAMED form as readily as its original — so the
	// stable logical target list and move graph recomputed here match the ones
	// Hide worked from. Quarantine does not promise a multi-path atomic rename
	// against concurrent writers; discard operates after that review boundary.
	targets, err := quarantine.ExpandTargets(sess.Workspace, quarantine.SuffixQuarantined, quarantine.DefaultTargets)
	if err != nil {
		return fmt.Errorf("expand quarantine targets: %w", err)
	}
	moves, err := quarantine.ComputeMoves(sess.Workspace, quarantine.SuffixQuarantined, targets)
	if err != nil {
		return fmt.Errorf("recompute quarantine moves: %w", err)
	}
	if err := quarantine.New(c.run).Restore(ctx, moves); err != nil {
		return fmt.Errorf("restore quarantined files: %w", err)
	}

	if err := sandboxTeardown(ctx, c.run, sess.Workspace); err != nil {
		return fmt.Errorf("teardown workspace: %w", err)
	}

	if sess.Path != "" {
		if err := os.Remove(sess.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove breadcrumb %s: %w", termsafe.QuotePath(sess.Path), termsafe.Error(err))
		}
	}
	slog.Info("Successfully tore down review session.", "ref", sess.Ref.String())
	return nil
}

// Cleanup discards every session created on the given date (YYYY-MM-DD, UTC),
// stale records included — a day's sweep that silently skipped the leftovers
// would be the very accumulation #212 exists to end.
//
// List supplies only the CANDIDATE set and its recorded dates; it is not
// trusted for anything else. Every candidate re-enters Teardown, which
// re-resolves membership, re-validates the record, and re-classifies the
// workspace from scratch — so a record that changed between the listing and
// the teardown is judged on what it is NOW, not on what List saw.
//
// One failure is retained as the first error while later candidates continue,
// matching the existing cleanup contract.
//
// Every live teardown in the sweep draws on ONE tmux budget (forgectl#648), so
// a hung tmux holds the lock for one lockedTmuxBudget rather than one per
// session. Once it is spent, the remaining LIVE sessions are skipped with
// ErrTmuxBudgetSpent and left untouched; stale and record-only sessions never
// call tmux, so the sweep still settles them.
func (c *Client) Cleanup(ctx context.Context, date string) error {
	// One lock hold for the whole sweep: the lock is non-reentrant, so the
	// listing and every teardown go through the *Locked cores.
	return c.withLifecycleLock(ctx, "cleanup", func() error {
		summaries, unreadable, err := c.listLocked()
		if err != nil {
			return err
		}
		if len(unreadable) > 0 {
			slog.Warn("Cleanup is sweeping past records it could not read.",
				"unreadable", len(unreadable), "first", unreadable[0].path)
		}
		var discarded int
		var firstErr error
		budget := newTmuxBudget()
		warnedSkip := false
		for _, sum := range summaries {
			if sum.CreatedAt().UTC().Format("2006-01-02") != date {
				continue
			}
			if err := c.auditedTeardownLocked(ctx, auditVerbCleanup, sum.Path(), budget); err != nil {
				if errors.Is(err, ErrTmuxBudgetSpent) {
					if !warnedSkip {
						slog.Warn("tmux stopped answering during cleanup; skipping the remaining live sessions.",
							"budget", lockedTmuxBudget)
						warnedSkip = true
					}
				} else {
					slog.Error("Failed to tear down session during cleanup.", "path", sum.Path(), "error", err)
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			discarded++
		}
		slog.Info("Cleanup complete.", "date", date, "discarded", discarded)
		return firstErr
	})
}
