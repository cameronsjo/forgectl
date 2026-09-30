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

// beforeTeardownReread runs between a pinned-handle protocol's root.Lstat of
// the member and its re-read open (discardStale, discardRecordOnly,
// setAsideUndecodableRecord). It is a no-op in production; a test sets it to
// swap the entry in exactly the window the Lstat cannot cover (forgectl#776).
// Tests must restore it and must not run in parallel while overriding it.
var beforeTeardownReread = func(string) {}

// rereadPinnedMember re-reads member's bytes through root, the pinned
// sessions-dir handle, for the byte comparison each pinned-handle protocol
// makes before it acts. name is the member's base name in root.
//
// The caller's root.Lstat checked the NAME; this proves what the open
// reached. openRegularInRoot refuses a symlink (even one inside the root) and a
// FIFO without blocking, and the descriptor must be the very file the member
// was resolved from (os.SameFile against member.info). So a same-bytes copy,
// or the original reached through a link, is refused before its bytes are
// compared (forgectl#776).
func rereadPinnedMember(root *os.Root, name string, member breadcrumbMember) ([]byte, error) {
	beforeTeardownReread(member.path)
	file, info, err := openRegularInRoot(root, name)
	if err != nil {
		return nil, fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(err))
	}
	if !os.SameFile(info, member.info) {
		_ = file.Close()
		return nil, fmt.Errorf("breadcrumb %s changed identity before its re-read; refusing to act on it",
			member.displayPath)
	}
	data, readErr := readBreadcrumbBytes(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("re-read breadcrumb %s: %w", member.displayPath, termsafe.Error(readErr))
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close breadcrumb %s after re-read: %w", member.displayPath, termsafe.Error(closeErr))
	}
	return data, nil
}

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
// A live teardown whose tmux budget is exhausted — a tmux call already timed
// out earlier in the same sweep — is SKIPPED here, with the other refusals and
// before the intent row: asking again would only spend the lock hold on a call
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

	data, err := rereadPinnedMember(root, name, member)
	if err != nil {
		return err
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

	data, err := rereadPinnedMember(root, name, member)
	if err != nil {
		return err
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

	data, err := rereadPinnedMember(root, name, member)
	if err != nil {
		return "", err
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
// needs-repair (no record on disk, or a failed write; a legacy record is
// converted and parked, forgectl#696). Callers
// must not tell the operator the record was parked when this is present.
var ErrRecordNotParked = errors.New("the record could not be parked in needs-repair")

// ErrTmuxBudgetSpent is what a sweep returns for a live session it did not
// attempt: an earlier tmux call in the same sweep already timed out, so the
// session was left exactly as it was — record, window and workspace.
var ErrTmuxBudgetSpent = errors.New("skipped: tmux stopped answering earlier in this sweep")

// windowKillTimeoutReason is the needs-repair reason such a record carries,
// followed by the window it could not account for (see unknownWindow).
const windowKillTimeoutReason = "window kill timed out (tmux unresponsive)"

// ErrWindowStateUnreadable is wrapped into a live teardown's error when tmux
// answered the review-window resolve with something other than a clean
// listing, so whether the window still exists is unknown (forgectl#702). The
// record is parked in needs-repair (unless ErrRecordNotParked is also present)
// and nothing is removed.
var ErrWindowStateUnreadable = errors.New("the review window's state could not be read from tmux")

// windowUnreadableReason is the needs-repair reason such a record carries,
// followed by the window it could not account for (see unknownWindow).
const windowUnreadableReason = "review window state could not be read from tmux"

// windowAmbiguousReason is the needs-repair reason a teardown parks with when
// more than one window in the review session carries the review's name.
const windowAmbiguousReason = "more than one review window carries this review's name; close the one that is not this review, then tear it down again"

// killReviewWindow kills the review window if it is still open, within one
// lockedTmuxBudget, and reports whether the window's state is still UNKNOWN
// afterwards. Resolution is exact — the window must carry this review's name
// AND sit under the review session's native id — and the kill revalidates
// that before issuing. Only a CONFIRMED absence means there is nothing of ours
// to kill: the pinned server answered cleanly and the review session or the
// window was not in its listing (windowConfirmedAbsent). That is the ordinary
// case after the reviewer exits; it must never widen into killing whatever
// tmux would have matched. The kill step is held to the same standard: its
// revalidation re-reads the window list, and a failure there or in
// kill-window itself is gone only when it is a confirmed absence, tmux's
// exact "can't find window" for the revalidated id, or a server generation
// change (windowGoneAtKill); every other kill failure is unsettled too.
//
// Three outcomes are NOT "nothing to kill", and the caller fails closed on
// each (forgectl#702):
//
//   - timedOut: tmux did not answer within the budget (or the caller
//     cancelled), so the window may be live.
//   - unsettled wrapping tmux.ErrAmbiguousWindow: more than one window in the
//     review session carries this review's name (tmux allows that), so
//     resolution refused to pick one and at least one of them is live.
//   - unsettled wrapping ErrWindowStateUnreadable: tmux answered, but not with
//     a listing that settles the question — an unreadable server, a pin
//     mismatch, a parse failure, a reparented window, or a kill-window that
//     failed. An unreadable server is not an absent window.
//
// Window names depend only on owner/repo/number, so discarding the record in
// any of these cases would leave an orphan that a later same-ref review,
// teardown or repair could collide with.
//
// A ref whose window name cannot be derived at all is the one non-tmux
// failure, and it still reads as nothing to kill: launch derives the same
// name, so no window of ours can carry it.
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
func (c *Client) killReviewWindow(ctx context.Context, ref Ref, budget *tmuxBudget) (timedOut bool, window string, unsettled error) {
	name, err := ReviewWindowName(ref)
	if err != nil {
		slog.Debug("No review window to kill (no derivable window name).", "ref", ref.String(), "error", err)
		return false, "", nil
	}
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
		if windowConfirmedAbsent(err) {
			slog.Debug("No review window to kill (already gone).", "window", name, "error", err)
			return false, "", nil
		}
		slog.Warn("Could not read the review window's state from tmux; refusing to treat it as gone.",
			"window", name, "error", err)
		return false, unknownWindow(ref, ""), fmt.Errorf("%w: %w", ErrWindowStateUnreadable, err)
	}
	if err := c.tmuxClient.KillWindow(tctx, resolved); err != nil {
		if tctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("Timed out killing the review window under the lifecycle lock.",
				"window_id", resolved.ID, "budget", lockedTmuxBudget)
			return true, unknownWindow(ref, resolved.ID), nil
		}
		if windowGoneAtKill(err) {
			slog.Debug("Review window was already gone at kill time.", "window_id", resolved.ID, "error", err)
			return false, "", nil
		}
		slog.Warn("Could not kill the review window; refusing to treat it as gone.",
			"window_id", resolved.ID, "error", err)
		return false, unknownWindow(ref, resolved.ID), fmt.Errorf("%w: %w", ErrWindowStateUnreadable, err)
	}
	return false, "", nil
}

// windowGoneAtKill reports whether a KillWindow failure means the resolved
// window is gone: a clean listing without it (windowConfirmedAbsent), tmux's
// exact "can't find window" answer to the kill of the id just revalidated,
// confirmed by a re-read showing the same server generation (ErrObjectGone,
// forgectl#746), or a generation change — the socket now
// answers from a different server, and the old one's windows are gone or
// unreachable. "Unreachable" is the blind spot: an old server whose socket was
// unlinked and replaced keeps its windows running where no command can reach
// them, which the resolve step's ErrSessionNotFound shares. Anything else — an
// unreadable list, a pin mismatch (ErrSelectorChanged), a reparented window
// (ErrWrongParent), kill-window itself failing for another reason — leaves the
// window possibly live (forgectl#702).
func windowGoneAtKill(err error) bool {
	return windowConfirmedAbsent(err) || errors.Is(err, tmux.ErrGenerationChanged)
}

// windowConfirmedAbsent reports whether a resolve failure is a clean answer
// that the review window does not exist, as opposed to a failure to find out.
// Only two verdicts qualify, and both come from a listing the selected server
// returned without error (an absent server lists as empty, which surfaces as
// ErrSessionNotFound): no session carries the review session's exact name, or
// that session holds no window with the review's exact name. Everything else —
// ErrServerUnreadable, ErrUnpinnedCommand, ErrNoServer, a parse failure, a
// malformed id — is not an answer, so it must never read as "already gone".
func windowConfirmedAbsent(err error) bool {
	return errors.Is(err, tmux.ErrSessionNotFound) || errors.Is(err, tmux.ErrObjectGone)
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
// parked (none on disk, a failed write) — cause wrapped with
// ErrRecordNotParked so no caller claims a park that never happened. Nothing is
// removed either way. The lock is held, so the *Locked park is the right form.
//
// A legacy (versionless) record accepts no phase transition, so it is
// converted in place to a v2 needs-repair record instead (forgectl#696):
// without that, a legacy record whose window could not be settled had no
// repair path at all — only an error naming the window.
func (c *Client) parkForUnknownWindow(sess Session, reason string, cause error) error {
	if sess.Path == "" {
		return fmt.Errorf("%w; %w", cause, ErrRecordNotParked)
	}
	err := c.markNeedsRepairLocked(sess.Path, reason)
	if errors.Is(err, errLegacyRecordNoTransition) {
		err = c.parkLegacyRecordLocked(sess.Path, reason)
	}
	if err != nil {
		return fmt.Errorf("%w; %w: %w", cause, ErrRecordNotParked, err)
	}
	return cause
}

// parkLegacyRecordLocked re-reads the legacy record at path under the lock and
// converts it to a v2 needs-repair record carrying reason. The re-read is what
// the conversion builds on, and the legacy write expectation refuses it if the
// record stopped being legacy in between.
func (c *Client) parkLegacyRecordLocked(path, reason string) error {
	bc, _, err := loadBreadcrumbRecord(path, c.sessionsDir)
	if err != nil {
		return err
	}
	if bc.Version != 0 {
		return fmt.Errorf("session record %s is no longer a legacy record; nothing was changed", termsafe.QuotePath(path))
	}
	return c.convertLegacyRecordLocked(path, bc, PhaseNeedsRepair, func(rec *Breadcrumb) {
		rec.RepairReason = termsafe.SafeLine(reason)
	})
}

// discard performs the actual teardown for an already-validated session: undo
// the quarantine (recomputed precisely from the sandbox's canonical
// scheme+targets), remove the workspace, kill the window, delete the
// breadcrumb.
func (c *Client) discard(ctx context.Context, sess Session, budget *tmuxBudget) error {
	slog.Debug("Preparing to tear down review session.", "ref", sess.Ref.String(), "workspace", sess.Workspace)

	// The window goes first, so a tmux that cannot answer, a window list that
	// cannot be read, or a window name that resolves to more than one window
	// stops the teardown BEFORE anything is removed. Failing closed here means parking the record in
	// needs-repair with the workspace intact: the record and the clean room are
	// what let the operator find and finish this later.
	timedOut, window, unsettled := c.killReviewWindow(ctx, sess.Ref, budget)
	if timedOut {
		// The window is named in both the parked reason and the error, so it is
		// recorded even when the park itself fails (a failed write): the
		// operator reads it on stderr instead.
		return c.parkForUnknownWindow(sess, windowKillTimeoutReason+"; "+window+" may still be running",
			fmt.Errorf("%w: %s may still be running", ErrWindowKillTimedOut, window))
	}
	if errors.Is(unsettled, tmux.ErrAmbiguousWindow) {
		return c.parkForUnknownWindow(sess, windowAmbiguousReason,
			fmt.Errorf("refusing to tear down %s, nothing was removed: %w", sess.Ref.String(), unsettled))
	}
	if unsettled != nil {
		return c.parkForUnknownWindow(sess, windowUnreadableReason+"; "+window+" may still be running",
			fmt.Errorf("refusing to tear down %s, nothing was removed: %w", sess.Ref.String(), unsettled))
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
// Later candidates continue past a failure. The returned error is the FIRST
// failure, matching the existing cleanup contract; the report carries every
// session's outcome, so a caller can describe each failure rather than only
// the first (forgectl#666).
//
// Every live teardown in the sweep gets its own full lockedTmuxBudget, but the
// sweep shares ONE tmuxBudget to notice an unresponsive tmux (forgectl#648):
// once any teardown's tmux work actually times out, the remaining LIVE
// sessions are skipped with ErrTmuxBudgetSpent and left untouched, so a hung
// tmux holds the lock for one budget rather than one per session. A slow but
// answering tmux never trips it. Stale and record-only sessions never call
// tmux, so the sweep still settles them.
func (c *Client) Cleanup(ctx context.Context, date string) (CleanupReport, error) {
	var report CleanupReport
	// One lock hold for the whole sweep: the lock is non-reentrant, so the
	// listing and every teardown go through the *Locked cores.
	err := c.withLifecycleLock(ctx, "cleanup", func() error {
		summaries, unreadable, err := c.listLocked()
		if err != nil {
			return err
		}
		if len(unreadable) > 0 {
			slog.Warn("Cleanup is sweeping past records it could not read.",
				"unreadable", len(unreadable), "first", unreadable[0].path)
		}
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
				report.Failed = append(report.Failed, CleanupFailure{Path: sum.Path(), Ref: sum.Ref().String(), Err: err})
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			report.Discarded++
		}
		slog.Info("Cleanup complete.", "date", date, "discarded", report.Discarded, "failed", len(report.Failed))
		return firstErr
	})
	return report, err
}

// CleanupReport is what one cleanup sweep did: how many sessions it discarded,
// and each session it did not, in sweep order. Failed is empty when the sweep
// never started (the lock was busy, the listing failed); the error says why.
type CleanupReport struct {
	Discarded int
	Failed    []CleanupFailure
}

// CleanupFailure is one session a sweep did not discard. Err is that session's
// own error: ErrWindowKillTimedOut (with or without ErrRecordNotParked) for a
// kill tmux never answered, ErrWindowStateUnreadable (likewise) for a window
// tmux could not say was gone, ErrTmuxBudgetSpent for a live session skipped
// after a timeout, or whatever refused or failed the teardown.
type CleanupFailure struct {
	Path string
	Ref  string
	Err  error
}
