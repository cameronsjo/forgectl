package herdradapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/sockstat"
)

// Output caps. stdoutCap is written as the seam's ceiling rather than the
// literal that happens to equal it: validate() refuses a cap above
// MaxOutputBytes, so a hardcoded constant would fail every command here at once
// the day that ceiling is lowered.
//
// herdr's rows are far leaner than cmux's — a workspace row is a handful of
// short fields rather than a wall of per-workspace state — so the listing
// ceiling that bites cmux at ~35 workspaces (forgectl#359) is not a practical
// limit here.
const (
	stdoutCap = exec.MaxOutputBytes
	stderrCap = 1 << 14
)

// minProtocol is the floor this adapter understands. Checked alongside herdr's
// own `compatible` flag rather than instead of it: that flag is the client's
// verdict about the client/server pair, and this is ours about the server.
const minProtocol = 20

// Start creates the surface workspace, starts the harness in its root pane, and
// reports what we know about whether the server changed state.
//
// The ownership marker is derived BEFORE the create call and travels in herdr's
// `label`. Unlike cmux, which has both a title and a description, herdr's create
// accepts only {cwd, env, focus, label} — so the one field has to carry the
// marker, and the human display name does not survive. That is the right
// trade: the marker is what makes a lost create recoverable, and a presentation
// string is not.
//
// WorkspaceInfo does carry a purpose-built `tokens` map, which would have been a
// better home. It is unreachable here: create cannot set it, so writing the
// marker there would need a second call, leaving a window in which a created
// workspace carries no ownership marker at all — exactly the window
// reconciliation exists to close.
func (a *Adapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	if err := spec.Validate(); err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	marker := spec.OwnershipName()

	// Readiness first, and it is the one check permitted to conclude NotMutated
	// on its own: a session that is not running, not ours, or incompatible has
	// certainly not created anything.
	server, cause := a.readiness(ctx)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}

	before, cause := a.snapshot(ctx, exec.KindHerdrSnapshot)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}

	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrCreate,
		exec.MustFixed("workspace"),
		exec.MustFixed("create"),
		exec.MustFixed("--no-focus"),
		exec.MustFixed("--label"),
		exec.Opaque(marker),
		exec.MustFixed("--cwd"),
		spec.CWD(),
	))
	if runErr != nil {
		return a.reconcile(ctx, spec.Tag(), marker, before, server, a.classifyRunError(runErr, res))
	}

	created, err := parseCreated(res.Stdout)
	if err != nil {
		// A create that exited zero without a readable identity is the same
		// ambiguity as one that failed.
		return a.reconcile(ctx, spec.Tag(), marker, before, server,
			backend.NewStartCause(backend.FailureMalformedResponse, err))
	}

	ref, err := a.reference(created, spec.Tag(), server)
	if err != nil {
		return a.reconcile(ctx, spec.Tag(), marker, before, server,
			backend.NewStartCause(backend.FailureMalformedResponse, err))
	}

	// The workspace exists and we can name it exactly. Everything from here on
	// returns THIS reference, whatever happens — which is the whole point of the
	// next call being separate.
	//
	// herdr's create takes no command, so starting the harness is a second
	// operation against the pane the create just reported. If it fails, the
	// workspace is still there: returning a bare error would strand it, because
	// the service would have nothing to close. RefKnown-with-cause is the
	// contract's answer — the launch failed AND we know exactly what to clean
	// up.
	if _, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrCreate,
		exec.MustFixed("pane"),
		// `pane run` sends the text and Enter in one call. The protocol has no
		// run method — it is a client-side composition of send_text and
		// send_keys — but the CLI verb is what this adapter invokes, so the CLI
		// is the surface that matters.
		exec.MustFixed("run"),
		exec.Opaque(created.PaneID),
		// The bootstrap is the last operand and it is what makes this workspace
		// the SURFACE rather than an idle shell. Separator first: exec.Opaque
		// refuses a leading dash without one, and whether the backend honours
		// `--` at this verb is OUR assertion, not the seam's.
		exec.EndOfOptions(),
		spec.Bootstrap().SensitiveArg(),
	)); runErr != nil {
		return backend.NewRefKnownWithCause(ref, a.classifyRunError(runErr, exec.SensitiveResult{}))
	}

	// Note what a clean exit here does and does not mean. `pane run` types a
	// line into a terminal; it reports that the keystrokes were delivered, not
	// that a process started, and herdr answers `{"result":{"type":"ok"}}` with
	// no output either way. Only the authenticated exec_started frame commits
	// the launch, which is the service's job and not this adapter's.
	return backend.NewRefKnown(ref)
}

// reconcile runs EXACTLY ONE listing to settle whether the create landed.
//
// A match must carry our exact marker AND be absent from the pre-snapshot.
// Either alone is weaker than it looks: the marker cannot tell this attempt's
// workspace from a retry's, and novelty would match anything the operator opened
// while the launch was in flight.
//
// A reconciled workspace yields a WORKSPACE-ONLY reference — no tab, no pane —
// and NewHerdrIdentity accepts that deliberately. The listing does not report a
// root pane, and requiring one would make the partial case unrepresentable,
// which is precisely the case that needs cleaning up rather than abandoning.
func (a *Adapter) reconcile(
	ctx context.Context,
	tag backend.RecoveryTag,
	marker string,
	before map[string]workspaceRow,
	server serverInfo,
	cause backend.StartCause,
) backend.StartResult {
	rows, rcause := a.snapshot(ctx, exec.KindHerdrReconcile)
	if rcause != nil {
		if rcause.Class() == backend.FailureUnavailable && a.serverGone(server) {
			return backend.NewNotMutated(cause)
		}
		return backend.NewOutcomeUnknown(tag, cause)
	}

	var matches []string
	for id, row := range rows {
		if row.marker != marker {
			continue
		}
		if _, existed := before[id]; existed {
			continue
		}
		matches = append(matches, row.id)
	}
	switch len(matches) {
	case 0:
		return backend.NewNotMutated(cause)
	case 1:
		ref, err := a.reference(createdWorkspace{WorkspaceID: matches[0]}, tag, server)
		if err != nil {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		return backend.NewRefKnownWithCause(ref, cause)
	default:
		// Closing one of an ambiguous pair is how a rollback destroys the wrong
		// object. The tag travels so an operator can find both by hand.
		return backend.NewOutcomeUnknown(tag, cause)
	}
}

// serverInfo is what readiness establishes about the session behind the pin.
type serverInfo struct {
	// socket is the endpoint herdr REPORTED for this session, not one we
	// derived. It is kept so absence can be settled by a stat rather than by a
	// diagnostic string.
	socket string

	// version is the protocol identity that goes into the fingerprint —
	// protocol rather than application version, because the fingerprint's job is
	// to detect an incompatible or different server and a cosmetic upgrade is
	// neither.
	version string

	// incarnation is the fingerprint taken BEFORE any mutation.
	incarnation backend.ServerID
}

// readiness proves the session exists, is running, is ours, and speaks a
// protocol we understand.
//
// The ordering is a safety property, not a convenience. The session roster is
// read FIRST and WITHOUT the pin, because `herdr --session <name>` STARTS a
// server when that session is not running — so pinning a name in order to ask
// whether it exists is the one question that can answer itself by creating the
// thing. `session list` is global and read-only, and refusing a session that is
// not already running is what keeps this adapter from silently minting a second,
// differently-privileged server (forgectl#364).
func (a *Adapter) readiness(ctx context.Context) (serverInfo, *backend.StartCause) {
	res, runErr := a.run.RunSensitive(ctx, exec.SensitiveCommand{
		Kind: exec.KindHerdrReadiness,
		Path: exec.Secret(a.herdrPath),
		// Deliberately NOT a.command(): no --session. See above.
		Args: []exec.Arg{
			exec.MustFixed("session"),
			exec.MustFixed("list"),
			exec.MustFixed("--json"),
		},
		StdoutCap: stdoutCap,
		StderrCap: stderrCap,
	})
	if runErr != nil {
		cause := a.classifyRunError(runErr, res)
		return serverInfo{}, &cause
	}
	sessions, err := parseSessions(res.Stdout)
	if err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return serverInfo{}, &cause
	}
	// Two refusals, two sentinels, and they are separate on purpose.
	//
	// Both are FailureUnavailable, so the class cannot tell them apart — and
	// they send an operator to different places: an absent session is a typo in
	// HERDR_SESSION, a stopped one is a session to start. Without distinct
	// sentinels the first check is also untestable, because a name absent from
	// the roster yields a zero row whose Running is false, so the second check
	// refuses it anyway and deleting the first changes nothing observable. A
	// sentinel makes the distinction real rather than cosmetic.
	row, ok := sessions[a.session]
	if !ok {
		cause := backend.NewStartCause(backend.FailureUnavailable, ErrNoSuchSession)
		return serverInfo{}, &cause
	}
	if !row.Running {
		// Refused rather than started. Starting it would give the launch a
		// server the operator never opened — and in this estate a stray herdr
		// server silently captures later invocations.
		cause := backend.NewStartCause(backend.FailureUnavailable, ErrSessionNotRunning)
		return serverInfo{}, &cause
	}
	if err := checkSocketPath(row.SocketPath); err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return serverInfo{}, &cause
	}

	info, cause := a.checkSocketOwner(row.SocketPath)
	if cause != nil {
		return serverInfo{}, cause
	}

	version, cause := a.protocol(ctx)
	if cause != nil {
		return serverInfo{}, cause
	}

	id, err := a.fingerprint(row.SocketPath, info, version)
	if err != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return serverInfo{}, &c
	}
	return serverInfo{socket: row.SocketPath, version: version, incarnation: id}, nil
}

// checkSocketOwner refuses an endpoint this uid does not privately own.
//
// Lstat rather than Stat: following a symlink would authenticate the target's
// ownership while talking through a link somebody else controls. An owner that
// cannot be READ is refused rather than waved through — a check that silently
// passes when it cannot see the owner is worse than no check, because it reads
// as one.
func (a *Adapter) checkSocketOwner(socket string) (os.FileInfo, *backend.StartCause) {
	info, err := a.lstat(socket)
	if err != nil {
		cause := backend.NewStartCause(backend.FailureUnavailable,
			fmt.Errorf("herdr socket is not reachable: %w", err))
		return nil, &cause
	}
	if info.Mode()&os.ModeSocket == 0 {
		cause := backend.NewStartCause(backend.FailureUnavailable,
			errors.New("the herdr endpoint is not a socket"))
		return nil, &cause
	}
	owner, ok := sockstat.OwnerUID(info)
	if !ok {
		cause := backend.NewStartCause(backend.FailurePermissionDenied,
			errors.New("the herdr socket's owner cannot be established"))
		return nil, &cause
	}
	if owner != a.selfUID() {
		cause := backend.NewStartCause(backend.FailurePermissionDenied,
			errors.New("the herdr socket is owned by another user"))
		return nil, &cause
	}
	return info, nil
}

// protocol asks the pinned session what it speaks.
//
// Safe to pin here, unlike the roster read above: readiness has already
// established that this session is running, so naming it cannot create it.
func (a *Adapter) protocol(ctx context.Context) (string, *backend.StartCause) {
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrReadiness,
		exec.MustFixed("status"),
		exec.MustFixed("--json"),
	))
	if runErr != nil {
		cause := a.classifyRunError(runErr, res)
		return "", &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		cause := backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("the herdr status reply was truncated"))
		return "", &cause
	}
	var reply statusReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("the herdr status reply was not readable JSON"))
		return "", &cause
	}
	if !reply.Server.Running {
		cause := backend.NewStartCause(backend.FailureUnavailable,
			errors.New("the herdr session stopped running"))
		return "", &cause
	}
	// herdr's own verdict about the client/server pair, and ours about the
	// server. Both are checked because they are different claims: `compatible`
	// can be true across a protocol this adapter has never seen.
	if !reply.Server.Compatible {
		cause := backend.NewStartCause(backend.FailureIncompatible,
			errors.New("herdr reports this client and server are not compatible"))
		return "", &cause
	}
	if reply.Server.Protocol < minProtocol {
		cause := backend.NewStartCause(backend.FailureIncompatible,
			fmt.Errorf("herdr protocol %d predates the %d floor", reply.Server.Protocol, minProtocol))
		return "", &cause
	}
	return fmt.Sprintf("herdr/%d", reply.Server.Protocol), nil
}

// reference binds a created workspace to the incarnation that minted it, after
// proving the server did not turn over while the create was in flight.
//
// herdr reports no server pid and no start time, so the socket's inode is the
// only witness to a restart — the same single-witness weakness cmux has, and the
// one forgectl#344 tracks. Taking the fingerprint again after the mutation is
// what turns "the server restarted mid-create and we cannot say which
// incarnation holds this workspace" into a refusal instead of a silent mixed
// state.
func (a *Adapter) reference(created createdWorkspace, tag backend.RecoveryTag, server serverInfo) (backend.Ref, error) {
	info, err := a.lstat(server.socket)
	if err != nil {
		return backend.Ref{}, fmt.Errorf("stat herdr socket: %w", err)
	}
	now, err := a.fingerprint(server.socket, info, server.version)
	if err != nil {
		return backend.Ref{}, err
	}
	if !server.incarnation.Matches(now) {
		return backend.Ref{}, errors.New("the herdr server restarted while the workspace was being created")
	}
	id, err := backend.NewHerdrIdentity(created.WorkspaceID, created.TabID, created.PaneID)
	if err != nil {
		return backend.Ref{}, err
	}
	return backend.NewHerdrRef(a.source, now, tag, id)
}

func (a *Adapter) fingerprint(socket string, info os.FileInfo, version string) (backend.ServerID, error) {
	in := backend.IncarnationInput{Endpoint: socket, Version: version}
	sockstat.Fill(&in, info)
	return backend.Fingerprint(in)
}

// serverGone reports that the endpoint PATH is absent. It does not report that
// the server is gone, and the difference matters because callers use it to
// discharge a rollback: a live herdr whose socket was unlinked satisfies this
// while still holding the workspace. Nothing here can close that gap, and the
// alternative — treating an absent path as unknown — makes the ordinary
// stopped-session case permanently unresolvable.
func (a *Adapter) serverGone(server serverInfo) bool {
	if server.socket == "" {
		return false
	}
	_, err := a.lstat(server.socket)
	return errors.Is(err, os.ErrNotExist)
}

// workspaceRow is the part of a listing row this package reads.
type workspaceRow struct {
	id     string
	marker string
}

// snapshot lists workspaces, keyed by id. Shared by the pre-snapshot,
// reconciliation, and locate so all three apply the same parse rules — two
// implementations would be two chances to drift, showing up as a probe that says
// present and a close that refuses.
func (a *Adapter) snapshot(ctx context.Context, kind exec.CommandKind) (map[string]workspaceRow, *backend.StartCause) {
	res, runErr := a.run.RunSensitive(ctx, a.command(kind,
		exec.MustFixed("workspace"),
		exec.MustFixed("list"),
	))
	if runErr != nil {
		cause := a.classifyRunError(runErr, res)
		return nil, &cause
	}
	rows, err := parseWorkspaceList(res.Stdout)
	if err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return nil, &cause
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// Wire shapes. Unknown fields are ignored on purpose: herdr adds them freely,
// and a strict decoder would turn every upgrade into an outage.
// ---------------------------------------------------------------------------

type sessionRow struct {
	Name       string `json:"name"`
	Running    bool   `json:"running"`
	SocketPath string `json:"socket_path"`
}

type sessionListReply struct {
	Sessions []sessionRow `json:"sessions"`
}

type statusReply struct {
	Server struct {
		Running    bool `json:"running"`
		Compatible bool `json:"compatible"`
		Protocol   int  `json:"protocol"`
	} `json:"server"`
}

// createdWorkspace is the identity a create reports. herdr returns the
// workspace, its tab, and its root pane in ONE envelope, which is why nothing
// here has to look the pane up afterwards.
type createdWorkspace struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

type createReply struct {
	Result struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	} `json:"result"`
}

type listReply struct {
	Result struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
		} `json:"workspaces"`
	} `json:"result"`
}

// errorReply is herdr's refusal envelope. The CODE is the field worth reading —
// it is machine-readable and stable in a way the message is not, which is the
// thing the cmux adapter had to approximate with a substring match on prose.
type errorReply struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// parseSessions reads the session roster, keyed by exact name.
func parseSessions(out exec.BoundedOutput) (map[string]sessionRow, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return nil, errors.New("the herdr session list was truncated")
	}
	var reply sessionListReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errors.New("the herdr session list was not readable JSON")
	}
	rows := make(map[string]sessionRow, len(reply.Sessions))
	for _, s := range reply.Sessions {
		if s.Name == "" {
			continue
		}
		rows[s.Name] = s
	}
	if len(reply.Sessions) > 0 && len(rows) == 0 {
		return nil, errors.New("no row of the herdr session list was usable")
	}
	return rows, nil
}

// parseCreated reads a create reply and requires the full identity.
//
// All three ids are required even though NewHerdrIdentity accepts a
// workspace-only ref: a CREATE reports all three, so a reply missing one is a
// reply we do not understand rather than a partial success. The partial shape is
// for RECONCILIATION, where the listing genuinely cannot report a pane.
func parseCreated(out exec.BoundedOutput) (createdWorkspace, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return createdWorkspace{}, errors.New("the herdr create reply was truncated")
	}
	var reply createReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return createdWorkspace{}, errors.New("the herdr create reply was not readable JSON")
	}
	out2 := createdWorkspace{
		WorkspaceID: reply.Result.Workspace.WorkspaceID,
		TabID:       reply.Result.Tab.TabID,
		PaneID:      reply.Result.RootPane.PaneID,
	}
	if out2.WorkspaceID == "" || out2.TabID == "" || out2.PaneID == "" {
		return createdWorkspace{}, errors.New("the herdr create reply named no complete workspace identity")
	}
	return out2, nil
}

// parseWorkspaceList reads a listing, failing closed on anything unreadable.
//
// A reply we could not read is not an empty reply — the contract internal/tmux
// states for its own parser and both earlier adapters had to be taught. The
// completeness flag is checked as well as the JSON parse because a document that
// happened to be valid at its truncation point would otherwise present as a
// SHORTER listing, which is exactly a false absence.
func parseWorkspaceList(out exec.BoundedOutput) (map[string]workspaceRow, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return nil, errors.New("the herdr workspace listing was truncated")
	}
	var reply listReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errors.New("the herdr workspace listing was not readable JSON")
	}
	rows := make(map[string]workspaceRow, len(reply.Result.Workspaces))
	for _, w := range reply.Result.Workspaces {
		if _, err := backend.NewHerdrIdentity(w.WorkspaceID, "", ""); err != nil {
			continue
		}
		rows[w.WorkspaceID] = workspaceRow{id: w.WorkspaceID, marker: w.Label}
	}
	if len(reply.Result.Workspaces) > 0 && len(rows) == 0 {
		return nil, errors.New("no row of the herdr workspace listing was usable")
	}
	return rows, nil
}

// errorCode reads herdr's structured refusal code, or "" when the stream is not
// one. Matching the code rather than the message is what makes a reworded
// diagnostic harmless.
func errorCode(out exec.BoundedOutput) string {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return ""
	}
	var reply errorReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return ""
	}
	return reply.Error.Code
}

// classifyRunError maps a runner error onto the closed failure vocabulary.
//
// It matches the SEAM's sentinels, never context's or os's: the sensitive runner
// returns only *exec.SensitiveError, which unwraps to its outcome's package
// sentinel and deliberately never to the underlying error, so a branch written
// against context.Canceled is structurally unreachable in production.
//
// Everything unrecognized becomes FailureUnavailable rather than
// FailureInternal: an unrecognized herdr failure is a statement about herdr, and
// calling it an internal defect sends the reader to the wrong code.
func (a *Adapter) classifyRunError(err error, res exec.SensitiveResult) backend.StartCause {
	switch {
	case errors.Is(err, exec.ErrCanceled):
		return backend.NewStartCause(backend.FailureCanceled, err)
	case errors.Is(err, exec.ErrTimeout):
		return backend.NewStartCause(backend.FailureTimeout, err)
	case errors.Is(err, exec.ErrInvalidCommand):
		// Refused before start: the argv this package built is wrong, which is
		// our defect and not the operator's herdr.
		return backend.NewStartCause(backend.FailureInternal, err)
	}
	switch errorCode(res.Stderr) {
	case "workspace_not_found":
		return backend.NewStartCause(backend.FailureIdentityMismatch, err)
	case "permission_denied", "unauthorized":
		return backend.NewStartCause(backend.FailurePermissionDenied, err)
	case "protocol_mismatch", "incompatible":
		return backend.NewStartCause(backend.FailureIncompatible, err)
	}
	return backend.NewStartCause(backend.FailureUnavailable, err)
}
