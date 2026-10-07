package herdradapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/wire"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/sockstat"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
	//
	// The root pane must be an idle shell before anything is typed into it. A
	// layout plugin can start an agent in a new workspace's root pane, and
	// `pane run` would then submit the bootstrap line, nonce included, to that
	// agent as a prompt. The check reads process state only. It refuses a pane
	// already taken, but it does not prove the shell has reached its prompt:
	// typed input waits in the terminal queue for whatever reads it next, and a
	// shell still loading its rc files passes. A prompt-ready signal is
	// forgectl#1051; binding the handshake to the pane is forgectl#1041.
	if cause := a.rootPaneIdle(ctx, created.PaneID); cause != nil {
		return backend.NewRefKnownWithCause(ref, *cause)
	}
	if runRes, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrBootstrap,
		exec.MustFixed("pane"),
		// `pane run` sends the text and Enter in one call. The protocol has no
		// run method — it is a client-side composition of send_text and
		// send_keys — but the CLI verb is what this adapter invokes, so the CLI
		// is the surface that matters.
		exec.MustFixed("run"),
		exec.Opaque(created.PaneID),
		// The bootstrap is the last operand and it is what makes this workspace
		// the SURFACE rather than an idle shell.
		//
		// NO end-of-options separator, and that is measured rather than
		// preferred. `pane run` forwards its COMMAND operand to the terminal
		// instead of parsing it, so a `--` does not separate anything — it gets
		// TYPED, and the pane answers `zsh: command not found: --` while the
		// real bootstrap never runs. The seam says plainly that whether a
		// backend honours `--` at a specific verb is the caller's assertion;
		// the tmux adapter probed it before relying on it and this one did not,
		// until a live launch showed the separator sitting in the pane.
		//
		// Safe without it because exec.Opaque still refuses a leading dash, and
		// the bootstrap is a shell-quoted absolute path — it begins with a quote
		// or a slash. A bootstrap that somehow began with a dash would be
		// refused before start rather than mis-parsed, which is the right
		// direction.
		spec.Bootstrap().SensitiveArg(),
	)); runErr != nil {
		// The RESULT is passed, not an empty one. It was discarded here, so
		// classifyRunError's structured-code switch read an empty stderr and
		// every pane-run failure classified as FailureUnavailable — on the one
		// call where herdr's code is most worth having. A permission_denied
		// became "check whether herdr is up" instead of "fix a permission". The
		// create call two blocks above already did this correctly, so it was an
		// inconsistency inside one function.
		return backend.NewRefKnownWithCause(ref, a.classifyRunError(runErr, runRes))
	}

	// Note what a clean exit here does and does not mean. `pane run` types a
	// line into a terminal; it reports that the keystrokes were delivered, not
	// that a process started, and herdr answers `{"result":{"type":"ok"}}` with
	// no output either way. Only the authenticated exec_started frame commits
	// the launch, which is the service's job and not this adapter's.
	return backend.NewRefKnown(ref)
}

// rootPaneIdle reports nil when pane's foreground belongs to its own shell.
//
// "Idle" is the shell owning the terminal's foreground process group. It is
// not "the shell is the only process listed": prompt hooks (direnv, env) run
// as short-lived children inside the shell's own group, and a fresh pane shows
// them for a moment. A job the shell starts — an agent, an editor — gets a
// process group of its own and takes the foreground, which is the case this
// refuses.
func (a *Adapter) rootPaneIdle(ctx context.Context, pane string) *backend.StartCause {
	var info processInfo
	for attempt := 0; attempt < idleAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				cause := backend.NewStartCause(backend.FailureCanceled, ctx.Err())
				return &cause
			case <-time.After(a.idleInterval):
			}
		}
		res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrPaneInspect,
			exec.MustFixed("pane"),
			exec.MustFixed("process-info"),
			exec.MustFixed("--pane"),
			exec.Opaque(pane),
		))
		if runErr != nil {
			cause := a.classifyRunError(runErr, res)
			return &cause
		}
		var err error
		if info, err = parseProcessInfo(res.Stdout); err != nil {
			cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
			return &cause
		}
		if info.idle() {
			return nil
		}
	}
	if info.ShellPID <= 0 || info.ForegroundGroup <= 0 {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errNoShell)
		return &cause
	}
	// The process name comes from herdr and ultimately from whatever runs in
	// the pane, so it is quoted for the terminal. Only the name: the full
	// command line can carry arguments the operator never meant to print.
	_, _ = fmt.Fprintf(a.warnings,
		"herdr: the new workspace's root pane is running %s, not an idle shell; forgectl will not type into it\n",
		termsafe.QuoteTextMax(info.foregroundName(), 64))
	cause := backend.NewStartCause(backend.FailureTargetBusy, ErrRootPaneBusy)
	return &cause
}

// idleAttempts and defaultIdleInterval bound the wait for a busy root pane:
// about 1.5 s. A shell's prompt hooks can briefly hold the foreground as a job
// of their own (measured: 1 sample in 75 on herdr 0.9.1), and that clears
// within one interval; an agent a layout started does not.
const (
	idleAttempts        = 10
	defaultIdleInterval = 150 * time.Millisecond
)

// ErrRootPaneBusy reports a root pane that something other than its shell
// holds.
var ErrRootPaneBusy = errors.New("herdradapter: the root pane is not an idle shell")

// processInfo is the part of `pane process-info` this package reads.
type processInfo struct {
	ForegroundGroup int           `json:"foreground_process_group_id"`
	ShellPID        int           `json:"shell_pid"`
	Processes       []paneProcess `json:"foreground_processes"`
}

// paneProcess is one foreground process. Argv is what tells an interactive
// shell from one running a command string.
type paneProcess struct {
	Name string   `json:"name"`
	PID  int      `json:"pid"`
	Argv []string `json:"argv"`
}

// knownShells are the process names an idle root pane may show. herdr's
// shell_pid is the pane's direct child, whatever binary that is, so a pane a
// layout started an agent in directly, or a shell that exec'd into one, owns
// its own foreground with the agent as leader. The name check refuses that.
var knownShells = []string{"sh", "bash", "zsh", "fish", "dash", "ksh", "mksh", "tcsh", "csh", "nu", "xonsh", "elvish", "pwsh"}

// idle reports a pane whose foreground group is its own process alone, and
// that process is a shell. A leader missing from the listing is not idle.
//
// "Alone" is what refuses a wrapper: `bash -lc 'source env.sh; claude'` has no
// job control, so the agent runs as a child inside the shell's own group, led
// by a shell name. A prompt hook (direnv, starship) is also a child in that
// group, but only for a moment; rootPaneIdle re-reads the pane, so a hook
// delays the launch while an agent child refuses it.
func (p processInfo) idle() bool {
	if p.ShellPID <= 0 || p.ForegroundGroup != p.ShellPID {
		return false
	}
	leaderIsShell := false
	for _, proc := range p.Processes {
		if proc.PID != p.ShellPID {
			return false
		}
	}
	for _, proc := range p.Processes {
		if proc.PID == p.ShellPID {
			// A login shell can show as "-zsh".
			leaderIsShell = slices.Contains(knownShells, strings.TrimPrefix(filepath.Base(proc.Name), "-")) &&
				interactiveArgv(proc.Argv)
		}
	}
	return leaderIsShell
}

// foregroundName names what holds the pane: the first process that is not
// the shell when there is one (an agent behind a shell wrapper), else the
// group leader, else the first listed process.
func (p processInfo) foregroundName() string {
	for _, proc := range p.Processes {
		if proc.PID != p.ShellPID && proc.PID != p.ForegroundGroup {
			return proc.Name
		}
	}
	for _, proc := range p.Processes {
		if proc.PID == p.ForegroundGroup {
			return proc.Name
		}
	}
	if len(p.Processes) > 0 {
		return p.Processes[0].Name
	}
	return "an unknown process"
}

type processInfoReply struct {
	Result struct {
		ProcessInfo *processInfo `json:"process_info"`
	} `json:"result"`
}

// parseProcessInfo fails closed: a reply without a shell pid or a foreground
// group is one this adapter cannot judge, and an unjudged pane is not idle.
func parseProcessInfo(out exec.BoundedOutput) (processInfo, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return processInfo{}, errors.New("the herdr process-info reply was truncated")
	}
	var reply processInfoReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return processInfo{}, errors.New("the herdr process-info reply was not readable JSON")
	}
	info := reply.Result.ProcessInfo
	if info == nil {
		return processInfo{}, errors.New("the herdr process-info reply had no process_info")
	}
	// A zero shell pid or foreground group is a pane whose shell has not
	// started yet. It is not idle (idle refuses it) and it is re-read; only a
	// pane still without a shell when the reads run out is refused as
	// malformed.
	return *info, nil
}

// errNoShell reports a pane that never showed a shell within the re-reads.
var errNoShell = errors.New("the herdr process-info reply named no shell or foreground group")

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
		// The exact marker and pre-snapshot identify this attempt's workspace.
		// Revalidate the SAME named session and socket, then bind the reference
		// to that fresh incarnation. A moved endpoint is refused before a pinned
		// command can follow the roster to it.
		fresh, rcause := a.freshReadiness(ctx, server)
		if rcause != nil {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		ref, err := a.reference(createdWorkspace{WorkspaceID: matches[0]}, tag, fresh)
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
	// session is the immutable CLI pin whose roster row selected socket. It is
	// carried so reconciliation can refuse before issuing any command if that
	// pin somehow changes during a launch.
	session string

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
// The session roster is read FIRST and WITHOUT the pin. `session list` is global
// and read-only, so it answers "does this session exist, is it running, and
// which socket does it own" without naming a session to anything.
//
// The justification is narrower than it first appears, and worth writing down
// accurately because two earlier versions of this comment got it wrong in
// opposite directions. `herdr --session <name>` with NO SUBCOMMAND — the bare
// and attach forms — starts a server when that session is not running, and a
// stray herdr server silently captures later invocations, which is a real hazard
// in this estate. Read from herdr's source: its CLI SUBCOMMANDS, which are all
// this adapter ever issues, return a structured `server_not_running` error
// instead of spawning. So the ordering is not what stands between this package
// and an accidental server.
//
// What it does buy is still worth the call: the roster is the only place the
// socket comes from, and refusing a session that is not already running gives
// the operator a better answer than a `server_not_running` surfacing three
// commands deeper, after a pre-snapshot and a create have been attempted
// (forgectl#364).
func (a *Adapter) readiness(ctx context.Context) (serverInfo, *backend.StartCause) {
	return a.readinessAtPin(ctx, "", true)
}

// CheckReady runs the readiness check Start runs first, and nothing else: the
// pinned session is running, its socket is ours, and it speaks a protocol
// this build knows. `surface drain` asks it before claiming, so a herdr that
// is down pauses claiming instead of failing a launch after its worktree
// exists. The error is a backend.StartCause; FailureUnavailable means the
// session is not running or cannot be reached.
func (a *Adapter) CheckReady(ctx context.Context) error {
	if _, cause := a.readiness(ctx); cause != nil {
		return *cause
	}
	return nil
}

// freshReadiness resolves the original named session again but refuses a new
// socket before protocol() can follow the session pin to another endpoint. The
// warning is suppressed because the initial readiness already emitted it for
// this Start.
func (a *Adapter) freshReadiness(ctx context.Context, original serverInfo) (serverInfo, *backend.StartCause) {
	if original.session == "" || original.session != a.session || original.socket == "" {
		cause := backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr session pin changed during reconciliation"))
		return serverInfo{}, &cause
	}
	return a.readinessAtPin(ctx, original.socket, false)
}

func (a *Adapter) readinessAtPin(ctx context.Context, expectedSocket string, warn bool) (serverInfo, *backend.StartCause) {
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
	if expectedSocket != "" && row.SocketPath != expectedSocket {
		cause := backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr session resolved to a different endpoint during reconciliation"))
		return serverInfo{session: a.session, socket: row.SocketPath}, &cause
	}

	// From here the socket is KNOWN and shape-checked, so every later failure
	// carries it forward rather than returning a zero serverInfo.
	//
	// This is not tidiness. locate concludes absence from the failure class AND
	// a stat of this socket, and serverGone short-circuits on an empty path — so
	// while every failing path returned serverInfo{}, the stat never ran and
	// locateAbsent was unreachable. A stopped session then produced
	// CloseUnreadable forever: a rollback obligation for a surface that
	// provably no longer exists, which could never be discharged. The comment in
	// locate described a control that did not exist.
	//
	// The class gate still does the discriminating. checkSocketOwner returns
	// FailureUnavailable only for an unreachable path and a non-socket, and
	// FailurePermissionDenied for the ownership arms, so a socket we are refused
	// can never be read as a socket that is gone.
	endpoint := serverInfo{session: a.session, socket: row.SocketPath}

	info, cause := a.checkSocketOwner(row.SocketPath)
	if cause != nil {
		return endpoint, cause
	}
	if warn {
		a.warnUnsafeSocketDir(row.SocketPath)
	}

	version, cause := a.protocol(ctx)
	if cause != nil {
		return endpoint, cause
	}

	id, err := a.fingerprint(row.SocketPath, info, version)
	if err != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, err)
		return endpoint, &c
	}
	return serverInfo{session: a.session, socket: row.SocketPath, version: version, incarnation: id}, nil
}

// warnUnsafeSocketDir reports an unsafe herdr-selected location without
// rejecting it. The socket-owning application chooses its endpoint; forgectl
// honors that choice and makes the local disruption risk visible.
func (a *Adapter) warnUnsafeSocketDir(socket string) {
	dir := filepath.Dir(socket)
	info, err := a.statSocketDir(dir)
	if err != nil {
		return
	}
	reason := sockstat.UnsafeDirectoryReason(info, a.selfUID())
	if reason == "" {
		return
	}
	// Advisory by ruling: even a broken warning sink must not change readiness.
	_, _ = fmt.Fprintf(a.warnings,
		"warning: herdr socket directory %q is %s; forgectl will honor this location, but another local user may disrupt launches\n",
		dir, reason)
}

// checkSocketOwner refuses an endpoint this uid does not own.
//
// Deliberately not "privately own", which is what this said and what it does not
// check: the refusal is based on the socket type and owning uid, never the
// permission bits or parent directory. A socket under an unsafe directory is
// accepted because herdr owns its endpoint policy, and the advisory check
// immediately before this function's caller proceeds makes that concern visible
// without claiming it is a readiness failure.
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
// Pinned, and safe to pin — established by reading herdr's source rather than
// by reasoning about the argv, after two successive comments here got it wrong
// in opposite directions.
//
// herdr's CLI SUBCOMMANDS do not auto-start a server. A subcommand that cannot
// reach the socket returns a structured `server_not_running` error naming the
// command an operator would run; no spawn path is reachable from one. The
// auto-start that makes a stray server a hazard in this estate belongs to the
// BARE and ATTACH forms — `herdr --session <name>` with no subcommand — which
// this adapter never issues.
//
// So the roster-first ordering is not load-bearing against auto-start, and the
// earlier wording claiming a pinned call could create a server overstated it as
// badly as the wording before it understated it. What the ordering actually buys
// is worth keeping on its own: the roster is where the socket comes from, and
// refusing a session that is not running gives the operator a better answer than
// `server_not_running` would, before any pinned command is issued.
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
// herdr reports no server pid and no start time, so the fingerprint rests on
// the socket alone — its inode and its change time (forgectl#344), where tmux
// has the server's pid and start time as well. Taking the fingerprint again
// after the mutation catches both a restart and the weaker case where socket
// metadata changed while the create was in flight.
//
// The change time can move without a restart, so this local bind refuses
// slightly more often than it strictly must. Start then reconciles by marker
// and novelty; one exact match earns fresh readiness against the same named
// session and socket, while ambiguity still remains OutcomeUnknown.
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
		return backend.Ref{}, errors.New("the herdr server incarnation changed while the workspace was being created")
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

// createResult and listResult are the "result" members of herdr's
// {"id","result"} replies, which wire.DecodeResult unwraps; the envelope itself
// is decoded in one place for this adapter and internal/herdr (#722).
type createResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	Tab struct {
		TabID string `json:"tab_id"`
	} `json:"tab"`
	RootPane struct {
		PaneID string `json:"pane_id"`
	} `json:"root_pane"`
}

// listResult's list is a pointer so a missing or null member is told apart
// from an empty listing: absence is what a false empty reads as.
type listResult struct {
	Workspaces *[]struct {
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
	} `json:"workspaces"`
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
	reply, err := wire.DecodeResult[createResult](raw)
	switch {
	case errors.Is(err, wire.ErrNoResult):
		return createdWorkspace{}, errors.New("the herdr create reply had no result")
	case err != nil:
		return createdWorkspace{}, errors.New("the herdr create reply was not readable JSON")
	}
	out2 := createdWorkspace{
		WorkspaceID: reply.Workspace.WorkspaceID,
		TabID:       reply.Tab.TabID,
		PaneID:      reply.RootPane.PaneID,
	}
	if out2.WorkspaceID == "" || out2.TabID == "" || out2.PaneID == "" {
		return createdWorkspace{}, errors.New("the herdr create reply named no complete workspace identity")
	}
	return out2, nil
}

// parseWorkspaceList reads a listing, failing closed on anything unreadable —
// including a SINGLE unusable row, which is where this parser diverges from its
// siblings and the divergence is deliberate.
//
// A reply we could not read is not an empty reply; the completeness flag is
// checked as well as the JSON parse, because a document that happened to be
// valid at its truncation point would otherwise present as a SHORTER listing,
// which is exactly a false absence. For the same reason a reply with no
// "result", or a result with no "workspaces" member, is refused rather than
// read as an empty listing (#722): that is the shape of a renamed envelope.
//
// internal/tmux and the cmux adapter DROP an unusable row and refuse only when
// every row is unusable, and they are right to: a tmux session name is chosen by
// the operator, so erroring on one malformed row would let anyone who can name a
// session break every lookup on the server. That reasoning does not transfer.
// herdr workspace ids are SERVER-assigned and short; nothing an operator types
// reaches this field, so an unusable row is the server saying something this
// adapter does not understand rather than a hostile name.
//
// The cost of getting it wrong is asymmetric. A dropped row for OUR workspace
// reads as absence at both consumers: locate answers gone, so Close discharges a
// rollback for a live surface, and reconcile matches nothing, so a create that
// landed reports NotMutated and the workspace stays open with no reference to
// it. Refusing the whole listing instead costs a launch that fails loudly.
func parseWorkspaceList(out exec.BoundedOutput) (map[string]workspaceRow, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return nil, errors.New("the herdr workspace listing was truncated")
	}
	reply, err := wire.DecodeResult[listResult](raw)
	switch {
	case errors.Is(err, wire.ErrNoResult):
		return nil, errors.New("the herdr workspace listing had no result")
	case err != nil:
		return nil, errors.New("the herdr workspace listing was not readable JSON")
	case reply.Workspaces == nil:
		return nil, errors.New("the herdr workspace listing had no workspaces member")
	}
	rows := make(map[string]workspaceRow, len(*reply.Workspaces))
	for _, w := range *reply.Workspaces {
		if _, err := backend.NewHerdrIdentity(w.WorkspaceID, "", ""); err != nil {
			return nil, errors.New("a row of the herdr workspace listing was not usable")
		}
		rows[w.WorkspaceID] = workspaceRow{id: w.WorkspaceID, marker: w.Label}
	}
	return rows, nil
}

// errorCode reads herdr's structured refusal code through wire.DecodeError, or
// "" when the stream is not one. The CODE is the field worth reading — it is
// machine-readable and stable in a way the message is not, which is the thing
// the cmux adapter had to approximate with a substring match on prose; matching
// it is what makes a reworded diagnostic harmless.
func errorCode(out exec.BoundedOutput) string {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return ""
	}
	r, ok := wire.DecodeError(raw)
	if !ok {
		return ""
	}
	return r.Code
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

// interactiveArgv reports a shell started to read commands from its terminal:
// argv present, and every argument after the program name a flag that does not
// take a command string. `bash -lc '...; claude'` sits at its own setup with
// only the shell in the foreground, but it never reads the pane; a line typed
// there waits in the terminal buffer for the agent the string starts. A
// missing argv cannot be judged and is not interactive.
func interactiveArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	for _, arg := range argv[1:] {
		if !slices.Contains(interactiveShellFlags, arg) {
			return false
		}
	}
	return true
}

// interactiveShellFlags are the only arguments an idle shell may carry. Every
// other flag is refused, because shells disagree on which ones take a command
// string (bash -c, fish -C and --init-command, nu --commands, pwsh -Command)
// and a list of the dangerous ones is never finished.
var interactiveShellFlags = []string{"-l", "-i", "-il", "-li", "--login", "--interactive"}
