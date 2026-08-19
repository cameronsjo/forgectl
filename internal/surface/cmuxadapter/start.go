package cmuxadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/sockstat"
)

// Output caps.
//
// stdoutCap is written as the seam's ceiling rather than the literal that
// happens to equal it: validate() REFUSES a cap above MaxOutputBytes, so a
// hardcoded constant would make every command in this package fail validation at
// once the day that ceiling is lowered.
//
// The ceiling is worth naming because it is reachable here in a way it is not
// for tmux. One row of `workspace list --json` measures ~1.8 KB against a 64 KB
// cap, so a listing stops being readable somewhere around 35 open workspaces.
// The failure is the safe one — a truncated stream is reported unreadable and
// never as an absence, so nothing is orphaned and no rollback is falsely
// discharged — but it is a real limit and it is tracked as forgectl#359 rather
// than hidden. The compact text listing is not an escape: it omits
// `description`, which is the field reconciliation matches on.
const (
	stdoutCap = exec.MaxOutputBytes
	stderrCap = 1 << 14
)

// wantProtocol and minVersion are the compatibility floor.
//
// The protocol string is checked as well as the number because they are
// independent claims: a future cmux could keep version 2 while answering a
// different protocol on the same socket, and a numeric check alone would accept
// it.
const (
	wantProtocol = "cmux-socket"
	minVersion   = 2
)

// requiredMethods are the verbs this adapter issues. Readiness asserts they are
// advertised so a cmux that dropped or renamed one is refused at the readiness
// check — before any mutation — rather than presenting as a malformed reply
// partway through a launch.
var requiredMethods = []string{"workspace.create", "workspace.close", "workspace.list"}

// Start creates the surface workspace and returns what we know about whether the
// server changed state.
//
// The ownership name is derived BEFORE the create call, from the spec's tag, and
// travels in cmux's `description` field. That ordering is the whole reason a
// failed create is recoverable: the marker we would have written is known
// independently of any reply, so one bounded reconciliation can ask "does a
// workspace carrying it exist?" — a question that has an answer even when the
// create call told us nothing.
//
// `description` rather than the title, because the two fields have different
// jobs. The title is what the operator reads, so it carries StartSpec.Name;
// description is schema-defined metadata that survives into `workspace list
// --json` untouched, which makes it an exact matcher rather than a fuzzy search
// over a display name the operator may rename at any time.
func (a *Adapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	if err := spec.Validate(); err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	marker := spec.OwnershipName()

	// Readiness first, and it is the one check permitted to conclude NotMutated
	// on its own: a cmux that is not running, not ours, incompatible, or
	// refusing our credentials has certainly not created anything.
	server, cause := a.readiness(ctx)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}

	// The pre-snapshot bounds reconciliation. The ownership marker is a fresh
	// random tag, so nothing already running can be wearing it — but a RETRY of
	// the same StartSpec reuses the tag, and then the snapshot is what separates
	// the workspace this attempt made from the one the last attempt left behind.
	before, cause := a.snapshot(ctx, exec.KindCmuxSnapshot)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}

	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindCmuxCreate,
		exec.MustFixed("workspace"),
		exec.MustFixed("create"),
		exec.MustFixed("--json"),
		// Not focused. A launch puts a surface somewhere for the harness to run
		// in; stealing the operator's foreground window to do it is a side
		// effect they did not ask for, and tmux's equivalent creates detached.
		exec.MustFixed("--focus"),
		exec.MustFixed("false"),
		exec.MustFixed("--description"),
		exec.Opaque(marker),
		exec.MustFixed("--name"),
		spec.Name(),
		exec.MustFixed("--cwd"),
		spec.CWD(),
		// This is what makes the created workspace the SURFACE rather than an
		// idle shell: the bootstrap re-enters forgectl carrying the socket path
		// and the one-shot nonce the handshake authenticates. The tmux adapter
		// shipped without its equivalent and every launch died in the handshake
		// after creating a session it then rolled back, so this argument is
		// covered by a test that asserts the create argv carries it.
		//
		// Measured, and it is why nothing here treats a clean exit as proof the
		// harness started: `--command` is create-plus-send-text. The text
		// arrives after the workspace exists (~3s on 0.64.22), so cmux exiting
		// zero says a workspace was made and says nothing about whether the
		// command in it ran. Only the authenticated exec_started frame commits.
		exec.MustFixed("--command"),
		spec.Bootstrap().SensitiveArg(),
	))

	if runErr == nil {
		id, err := parseCreatedWorkspace(res.Stdout)
		if err == nil {
			ref, rerr := a.reference(id, spec.Tag(), server)
			if rerr == nil {
				return backend.NewRefKnown(ref)
			}
			// The workspace exists — cmux said so, with a valid UUID — but we
			// cannot build a reference to it. That is not "nothing happened".
			return a.reconcile(ctx, spec.Tag(), marker, before, server,
				backend.NewStartCause(backend.FailureMalformedResponse, rerr))
		}
		// A create that exited zero without a parseable UUID is the same
		// ambiguity as one that failed, and it is the shape the --id-format trap
		// produces: reconcile rather than guess.
		return a.reconcile(ctx, spec.Tag(), marker, before, server,
			backend.NewStartCause(backend.FailureMalformedResponse, err))
	}

	return a.reconcile(ctx, spec.Tag(), marker, before, server, a.classifyRunError(runErr, res.Stderr))
}

// reconcile runs EXACTLY ONE listing to settle whether the create landed.
//
// One, not a retry loop: each attempt is another chance to match a workspace
// that appeared for some other reason, and the three-outcome contract would
// rather report an honest OutcomeUnknown than keep asking until it gets an
// answer it likes.
//
// A match must satisfy BOTH conditions — carry our exact ownership marker AND be
// absent from the pre-snapshot. Either alone is weaker than it looks: the marker
// alone cannot tell this attempt's workspace from a previous attempt's, and
// novelty alone would match any workspace the operator happened to open while
// the launch was in flight.
//
// Three ways out, mapping onto the matrix directly. Exactly one new marked
// workspace is RefKnown-with-cause: something was created and the launch still
// failed, so the service closes it. Confirmed absent is NotMutated. More than
// one match is OutcomeUnknown and nothing is closed — closing one of an
// ambiguous pair is how a rollback destroys the wrong object.
func (a *Adapter) reconcile(
	ctx context.Context,
	tag backend.RecoveryTag,
	marker string,
	before map[string]workspaceRow,
	server serverInfo,
	cause backend.StartCause,
) backend.StartResult {
	rows, rcause := a.snapshot(ctx, exec.KindCmuxReconcile)
	if rcause != nil {
		// A listing we could not read cannot prove absence — except in the one
		// case where the absence of the SERVER is itself the proof.
		if rcause.Class() == backend.FailureUnavailable && a.serverGone() {
			return backend.NewNotMutated(cause)
		}
		return backend.NewOutcomeUnknown(tag, cause)
	}

	var matches []string
	for key, row := range rows {
		if row.marker != marker {
			continue
		}
		if _, existed := before[key]; existed {
			continue
		}
		matches = append(matches, row.id)
	}
	switch len(matches) {
	case 0:
		return backend.NewNotMutated(cause)
	case 1:
		ref, err := a.reference(matches[0], tag, server)
		if err != nil {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		return backend.NewRefKnownWithCause(ref, cause)
	default:
		return backend.NewOutcomeUnknown(tag, cause)
	}
}

// serverInfo is what readiness establishes about the server behind the pin.
type serverInfo struct {
	// version is the protocol identity that goes into the fingerprint. It is
	// the protocol name and number rather than the application version because
	// the fingerprint's job is to detect an incompatible or different server,
	// and a cosmetic app upgrade that keeps the protocol is neither.
	version string

	// incarnation is the fingerprint taken BEFORE any mutation. Comparing it
	// against a fingerprint taken after is what turns "the server restarted
	// while we were creating" from an undetected mixed state into an honest
	// OutcomeUnknown.
	incarnation backend.ServerID
}

// readiness proves cmux is running, is ours, speaks a protocol we understand,
// advertises the verbs we use, and answers on the endpoint we pinned.
//
// The endpoint assertion is the one with no tmux counterpart and it is worth
// stating plainly: cmux reports the socket_path it actually bound, so comparing
// it against the path this adapter pinned detects a pin that did not take. The
// bootstrap carries the handshake socket path and a one-shot nonce, so sending
// it to a server we did not select is the failure this check exists to prevent —
// and without the check that failure is silent, because a wrong server answers
// perfectly well.
func (a *Adapter) readiness(ctx context.Context) (serverInfo, *backend.StartCause) {
	// Ownership before anything is sent. Lstat rather than Stat: following a
	// symlink would authenticate the target's ownership while talking through a
	// link somebody else controls.
	info, err := a.lstat(a.socket)
	if err != nil {
		cause := backend.NewStartCause(backend.FailureUnavailable,
			fmt.Errorf("cmux socket is not reachable: %w", err))
		return serverInfo{}, &cause
	}
	if info.Mode()&os.ModeSocket == 0 {
		cause := backend.NewStartCause(backend.FailureUnavailable,
			errors.New("the cmux endpoint is not a socket"))
		return serverInfo{}, &cause
	}
	// An owner we cannot READ is refused, not waved through. A check that
	// silently passes when it cannot see the owner is worse than no check,
	// because it reads as one.
	owner, ok := sockstat.OwnerUID(info)
	if !ok {
		cause := backend.NewStartCause(backend.FailurePermissionDenied,
			errors.New("the cmux socket's owner cannot be established"))
		return serverInfo{}, &cause
	}
	if owner != a.selfUID() {
		cause := backend.NewStartCause(backend.FailurePermissionDenied,
			errors.New("the cmux socket is owned by another user"))
		return serverInfo{}, &cause
	}

	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindCmuxReadiness,
		exec.MustFixed("capabilities"),
	))
	if runErr != nil {
		cause := a.classifyRunError(runErr, res.Stderr)
		return serverInfo{}, &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		cause := backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("the cmux capabilities reply was truncated"))
		return serverInfo{}, &cause
	}
	var reply capabilitiesReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("the cmux capabilities reply was not readable JSON"))
		return serverInfo{}, &cause
	}
	if reply.Protocol != wantProtocol {
		cause := backend.NewStartCause(backend.FailureIncompatible,
			errors.New("the server behind this endpoint speaks a different protocol"))
		return serverInfo{}, &cause
	}
	if reply.Version < minVersion {
		cause := backend.NewStartCause(backend.FailureIncompatible,
			fmt.Errorf("cmux protocol %d predates the %d floor", reply.Version, minVersion))
		return serverInfo{}, &cause
	}
	// Compared against the path we pinned, not merely required to be non-empty.
	// filepath.Clean on both sides so a trailing-slash or dot-segment spelling
	// of the same path is not read as a different endpoint.
	if filepath.Clean(reply.SocketPath) != filepath.Clean(a.socket) {
		cause := backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("cmux answered on a different endpoint than the one pinned"))
		return serverInfo{}, &cause
	}
	if missing := missingMethods(reply.Methods); missing != "" {
		cause := backend.NewStartCause(backend.FailureIncompatible,
			fmt.Errorf("this cmux does not advertise %s", missing))
		return serverInfo{}, &cause
	}

	version := fmt.Sprintf("%s/%d", reply.Protocol, reply.Version)
	id, err := a.fingerprint(info, version)
	if err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return serverInfo{}, &cause
	}
	return serverInfo{version: version, incarnation: id}, nil
}

// reference binds a workspace UUID to the incarnation that minted it, after
// proving the server did not turn over while the create was in flight.
//
// The re-fingerprint is the point. cmux reports no server pid and no start time,
// so unlike tmux — which carries three independent witnesses to a restart — the
// only witness here is the socket's inode. Taking it again after the mutation
// is what converts "the server restarted mid-create, and we cannot say which
// incarnation holds this workspace" from a silent mixed state into a refusal.
// The single-witness weakness itself is shared with herdr and tracked as
// forgectl#344.
func (a *Adapter) reference(workspace string, tag backend.RecoveryTag, server serverInfo) (backend.Ref, error) {
	info, err := a.lstat(a.socket)
	if err != nil {
		return backend.Ref{}, fmt.Errorf("stat cmux socket: %w", err)
	}
	now, err := a.fingerprint(info, server.version)
	if err != nil {
		return backend.Ref{}, err
	}
	if !server.incarnation.Matches(now) {
		return backend.Ref{}, errors.New("the cmux server restarted while the workspace was being created")
	}
	id, err := backend.NewCMuxIdentity(workspace)
	if err != nil {
		return backend.Ref{}, err
	}
	return backend.NewCmuxRef(a.source, now, tag, id)
}

// fingerprint hashes the incarnation the endpoint currently represents.
func (a *Adapter) fingerprint(info os.FileInfo, version string) (backend.ServerID, error) {
	in := backend.IncarnationInput{
		Endpoint: a.socket,
		Version:  version,
	}
	// The inode is required by Fingerprint and is the field that turns over when
	// a server restarts on the same path. cmux supplies no pid or start time, so
	// this is the sole witness — see reference for what is done about that.
	sockstat.Fill(&in, info)
	return backend.Fingerprint(in)
}

// workspaceRow is the part of a listing row this package reads. The id is kept
// alongside the map key because the key is case-folded and the id is not: what
// goes back to cmux must be the bytes cmux wrote.
type workspaceRow struct {
	id     string
	marker string
	title  string
}

// foldID is the comparison spelling of a workspace UUID. It exists so a lookup
// cannot be broken by a backend that reports the same identifier in two cases,
// and it is used ONLY as a map key — never as a value handed back to cmux.
func foldID(id string) string { return strings.ToUpper(id) }

// snapshot lists workspaces, keyed by UUID.
//
// It is the shared read for the pre-snapshot, reconciliation, and locate, so all
// three apply the same completeness and parse rules — two implementations of
// that would be two chances to let them drift, with the drift showing up as a
// probe that says present and a close that refuses, or worse, the reverse.
func (a *Adapter) snapshot(ctx context.Context, kind exec.CommandKind) (map[string]workspaceRow, *backend.StartCause) {
	res, runErr := a.run.RunSensitive(ctx, a.command(kind,
		exec.MustFixed("workspace"),
		exec.MustFixed("list"),
		exec.MustFixed("--json"),
	))
	if runErr != nil {
		cause := a.classifyRunError(runErr, res.Stderr)
		return nil, &cause
	}
	rows, err := parseWorkspaceList(res.Stdout)
	if err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return nil, &cause
	}
	return rows, nil
}

// capabilitiesReply is the readiness document. Unknown fields are ignored on
// purpose: cmux adds capabilities freely, and a strict decoder would turn every
// upgrade into an outage.
type capabilitiesReply struct {
	Protocol   string   `json:"protocol"`
	Version    int      `json:"version"`
	SocketPath string   `json:"socket_path"`
	Methods    []string `json:"methods"`
}

// createReply is the create envelope under `--id-format uuids`.
//
// Only workspace_id is read, and its absence is the point. Without the global
// flag cmux names this field workspace_ref and fills it with a positional
// index — so a reply parsed without the flag leaves this empty and is refused as
// malformed, rather than binding a reference to a number that means a different
// workspace tomorrow.
type createReply struct {
	WorkspaceID string `json:"workspace_id"`
}

// listReply is the listing envelope. `id` is the UUID in both id-formats — the
// flag only adds or drops a separate `ref` key — so this parse does not depend
// on the flag the way create's does.
type listReply struct {
	Workspaces []struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		CustomTitle string `json:"custom_title"`
	} `json:"workspaces"`
}

// parseCreatedWorkspace reads a create reply and requires an exact UUID.
//
// Requiring the UUID grammar, not merely a non-empty string, is what closes the
// --id-format trap from both directions: a missing field and a positional ref
// are both refused here, and neither can become a reference.
func parseCreatedWorkspace(out exec.BoundedOutput) (string, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return "", errors.New("the cmux create reply was truncated")
	}
	var reply createReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", errors.New("the cmux create reply was not readable JSON")
	}
	if _, err := backend.NewCMuxIdentity(reply.WorkspaceID); err != nil {
		return "", errors.New("the cmux create reply named no workspace uuid")
	}
	return reply.WorkspaceID, nil
}

// parseWorkspaceList reads a listing, failing closed on anything it cannot read
// whole.
//
// The fail-closed contract is the same one internal/tmux states for its own
// parser and the tmux adapter had to be taught: a reply we could not read is not
// an empty reply. Here the document is JSON rather than a line grammar, so a
// truncated stream cannot parse at all and the completeness flag is checked
// besides — the two guards overlap deliberately, because a JSON document that
// happened to be valid at its truncation point would otherwise present as a
// SHORTER listing, which is exactly a false absence.
//
// A complete document with an empty workspaces array is a legitimate absence and
// is reported as one. A row whose id is not a UUID is dropped rather than
// erroring, for the same reason internal/tmux drops a malformed row: erroring
// would let anything that can produce one row break every lookup on the server.
// Total loss is the case that must not be silent, so a listing that yielded rows
// in the document but none this package could use is refused.
func parseWorkspaceList(out exec.BoundedOutput) (map[string]workspaceRow, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return nil, errors.New("the cmux workspace listing was truncated")
	}
	var reply listReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errors.New("the cmux workspace listing was not readable JSON")
	}
	rows := make(map[string]workspaceRow, len(reply.Workspaces))
	for _, w := range reply.Workspaces {
		if _, err := backend.NewCMuxIdentity(w.ID); err != nil {
			continue
		}
		// Keyed by the folded spelling, holding the id EXACTLY as cmux wrote it.
		//
		// Folding only the key is the whole point. cmux emits uppercase in both
		// create and list today — measured on 0.64.22 — so the two agree and
		// this changes nothing. It is here because the cost of them ever
		// disagreeing is not a failed lookup but a stranded surface: a Close
		// that cannot find its own workspace reports it already gone and
		// discharges the rollback. The id itself is never rewritten, because it
		// is an opaque handle that goes straight back to cmux.
		rows[foldID(w.ID)] = workspaceRow{id: w.ID, marker: w.Description, title: w.CustomTitle}
	}
	if len(reply.Workspaces) > 0 && len(rows) == 0 {
		return nil, errors.New("no row of the cmux workspace listing was usable")
	}
	return rows, nil
}

// missingMethods names the first required verb the server does not advertise,
// or "" when all of them are present.
func missingMethods(advertised []string) string {
	have := make(map[string]struct{}, len(advertised))
	for _, m := range advertised {
		have[m] = struct{}{}
	}
	for _, want := range requiredMethods {
		if _, ok := have[want]; !ok {
			return want
		}
	}
	return ""
}

// serverGone reports whether the endpoint is definitively absent.
//
// It asks the FILESYSTEM, not the diagnostic. cmux does emit a recognisable
// "Socket not found at <path>" for this case, but the tmux adapter was bitten
// twice by classifying absence from a diagnostic string — once by reading a
// permission error as absence, once by removing the arm and losing the case
// where absence is the correct answer. A stat answers the question the text only
// hints at, and it cannot be changed by a locale or a release note.
func (a *Adapter) serverGone() bool {
	_, err := a.lstat(a.socket)
	return errors.Is(err, os.ErrNotExist)
}

// authFailed matches cmux's exact refusal for a rejected credential. Measured on
// 0.64.22: every verb answers "Error: ERROR: Invalid password" with exit 1.
func authFailed(out exec.BoundedOutput) bool {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return false
	}
	return strings.Contains(string(raw), "Invalid password")
}

// classifyRunError maps a runner error onto the closed failure vocabulary.
//
// It matches the SEAM's sentinels, not context's or os's. The sensitive runner
// returns only *exec.SensitiveError, and that type unwraps to its outcome's
// package sentinel and deliberately never to the underlying error — whose text
// can carry the path that failed to start. So errors.Is(err, context.Canceled)
// is structurally false even for a command the runner killed on cancellation,
// and classifying on it would leave FailureCanceled, FailureTimeout, and
// FailurePermissionDenied unreachable while every failure reported
// FailureUnavailable.
//
// Everything it cannot recognize becomes FailureUnavailable rather than
// FailureInternal: an unrecognized cmux failure is a statement about cmux, and
// calling it an internal defect would send the reader to the wrong code.
func (a *Adapter) classifyRunError(err error, stderr exec.BoundedOutput) backend.StartCause {
	switch {
	case errors.Is(err, exec.ErrCanceled):
		return backend.NewStartCause(backend.FailureCanceled, err)
	case errors.Is(err, exec.ErrTimeout):
		return backend.NewStartCause(backend.FailureTimeout, err)
	case errors.Is(err, exec.ErrInvalidCommand):
		// Refused before start: the argv this package built is wrong, which is
		// our defect and not the operator's cmux.
		return backend.NewStartCause(backend.FailureInternal, err)
	case authFailed(stderr):
		return backend.NewStartCause(backend.FailureAuthentication, err)
	default:
		return backend.NewStartCause(backend.FailureUnavailable, err)
	}
}
