package tmuxadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// Output caps. A tmux -F reply is a handful of short fields per session, so
// these are generous rather than tight — but they are bounded, because the
// completeness flag that comes back with a truncated stream is what turns a
// half-read reply into OutcomeUnknown instead of a confident misparse.
const (
	stdoutCap = 1 << 16
	stderrCap = 1 << 14
)

// Start creates the surface session and returns what we know about whether the
// server changed state.
//
// The ownership name is derived BEFORE the create call, from the spec's tag.
// That ordering is the whole reason a failed create is recoverable: the name we
// would have used is known independently of any reply, so one bounded
// reconciliation can ask "does an object by that name exist?" — a question that
// has an answer even when the create call told us nothing.
func (a *Adapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	if err := spec.Validate(); err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	name := spec.OwnershipName()

	// Readiness first, and it is the one check permitted to conclude
	// NotMutated on its own: a tmux that cannot run, or that predates
	// #{start_time}, has certainly not created anything.
	version, cause := a.readiness(ctx)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}

	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindTmuxCreate,
		exec.MustFixed("new-session"),
		exec.MustFixed("-d"),
		exec.MustFixed("-P"),
		exec.MustFixed("-F"),
		exec.Opaque(identityFormat),
		exec.MustFixed("-s"),
		exec.Opaque(name),
		exec.MustFixed("-c"),
		spec.CWD(),
	))

	if runErr == nil {
		row, ok := soleRow(res.Stdout, name)
		if ok {
			ref, err := a.reference(row, spec.Tag(), version)
			if err == nil {
				return backend.NewRefKnown(ref)
			}
			// The session exists — tmux said so — but we cannot build a
			// reference to it. That is not "nothing happened".
			return a.reconcile(ctx, spec.Tag(), name, version,
				backend.NewStartCause(backend.FailureMalformedResponse, err))
		}
		// A create that exited zero and did not describe what it made is the
		// same ambiguity as one that failed: reconcile rather than guess.
		return a.reconcile(ctx, spec.Tag(), name, version,
			backend.NewStartCause(backend.FailureMalformedResponse,
				errors.New("create reported success without a parseable identity")))
	}

	// A duplicate-name refusal is the ONE definitive pre-mutation failure tmux
	// reports, and the only class the contract lets a caller retry. It is
	// matched on exact equality against a string built here from the name we
	// supplied — not Contains — so a session named after the diagnostic cannot
	// forge the verdict.
	if stderrEquals(res.Stderr, "duplicate session: "+name) {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureNameCollision, runErr))
	}
	return a.reconcile(ctx, spec.Tag(), name, version, classifyRunError(runErr))
}

// reconcile runs EXACTLY ONE listing to settle whether the create landed.
//
// One, not a retry loop: each attempt is another chance to match a session that
// appeared for some other reason, and the three-outcome contract would rather
// report an honest OutcomeUnknown than keep asking until it gets an answer it
// likes.
//
// The three ways out map onto the matrix directly. Present under our exact
// ownership name on the same incarnation is RefKnown-with-cause: something was
// created and the launch still failed, so the service closes it. Confirmed
// absent is NotMutated. Anything we could not read — an unreadable server, a
// truncated reply, a restarted incarnation — is OutcomeUnknown, which carries
// the tag so an operator can find the object by name and nothing else.
func (a *Adapter) reconcile(ctx context.Context, tag backend.RecoveryTag, name, version string, cause backend.StartCause) backend.StartResult {
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindTmuxReconcile,
		exec.MustFixed("list-sessions"),
		exec.MustFixed("-F"),
		exec.Opaque(identityFormat),
	))
	if runErr != nil {
		// "No server running" is the one listing failure that proves absence:
		// a server that does not exist is not hiding our session.
		if noServer(res.Stderr) {
			return backend.NewNotMutated(cause)
		}
		return backend.NewOutcomeUnknown(tag, cause)
	}
	rows, complete := parseRows(res.Stdout)
	if !complete {
		// A truncated listing cannot prove absence — the row we are looking for
		// may be in the part we did not get.
		return backend.NewOutcomeUnknown(tag, cause)
	}
	for _, row := range rows {
		if row.name != name {
			continue
		}
		ref, err := a.reference(row, tag, version)
		if err != nil {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		return backend.NewRefKnownWithCause(ref, cause)
	}
	return backend.NewNotMutated(cause)
}

// readiness proves tmux runs and is new enough to expand the identity format.
//
// The version floor is checked rather than inferred from a parse failure: below
// 2.2 tmux echoes `#{pid}` back literally, which would arrive as an
// unparseable row and be classified as a malformed response — sending an
// operator to look for a protocol bug instead of an old tmux.
func (a *Adapter) readiness(ctx context.Context) (string, *backend.StartCause) {
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindTmuxReadiness, exec.MustFixed("-V")))
	if err != nil {
		cause := classifyRunError(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", &cause
		}
		cause = backend.NewStartCause(backend.FailureUnavailable, err)
		return "", &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	version := strings.TrimSpace(string(raw))
	if !complete || version == "" {
		cause := backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("tmux -V produced no readable version"))
		return "", &cause
	}
	major, minor, ok := parseVersion(version)
	if !ok {
		cause := backend.NewStartCause(backend.FailureMalformedResponse,
			fmt.Errorf("tmux reported an unrecognized version"))
		return "", &cause
	}
	if major < minMajor || (major == minMajor && minor < minMinor) {
		cause := backend.NewStartCause(backend.FailureIncompatible,
			fmt.Errorf("tmux %d.%d predates the %d.%d floor for #{start_time}", major, minor, minMajor, minMinor))
		return "", &cause
	}
	return version, nil
}

// reference fingerprints the live server and binds the row's native id to it.
func (a *Adapter) reference(row identityRow, tag backend.RecoveryTag, version string) (backend.Ref, error) {
	if err := tmux.ValidateSessionID(row.id); err != nil {
		return backend.Ref{}, err
	}
	server, err := a.fingerprint(row, version)
	if err != nil {
		return backend.Ref{}, err
	}
	id, err := backend.NewTmuxIdentity(row.name)
	if err != nil {
		return backend.Ref{}, err
	}
	return backend.NewTmuxRef(a.source, server, tag, id)
}

// fingerprint hashes the incarnation this row came from.
//
// The socket's inode is required by Fingerprint and is the field that turns
// over when a server restarts on the same path. tmux also reports its own pid
// and start time, so a tmux fingerprint carries three independent witnesses to
// a restart — which is why the incarnation guarantee holds here and needs
// separate work for herdr (forgectl#344).
func (a *Adapter) fingerprint(row identityRow, version string) (backend.ServerID, error) {
	info, err := a.lstat(a.socket)
	if err != nil {
		return backend.ServerID{}, fmt.Errorf("stat tmux socket: %w", err)
	}
	in := backend.IncarnationInput{
		Endpoint: a.socket,
		Version:  version,
		PID:      row.pid,
		// tmux reports start_time in whole seconds; the field wants nanos.
		StartedAtUnixNano: row.startTime * 1e9,
	}
	fillStat(&in, info)
	return backend.Fingerprint(in)
}

// identityRow is one parsed -F line.
type identityRow struct {
	pid       int
	startTime int64
	id        string
	name      string
}

// parseRows splits a listing into rows, dropping any line whose field count is
// not exact.
//
// Exact, not a minimum: a session NAME may legally contain the field
// separator, and under a `len(f) < N` check such a row would parse with every
// later field shifted one right — offering a well-formed-looking native id
// that names a different session. A separator inside a name can only push the
// count ABOVE the expected one, so requiring equality drops the forged row
// instead of misreading it.
func parseRows(out exec.BoundedOutput) ([]identityRow, bool) {
	raw, complete := out.CopyBytesForParse()
	var rows []identityRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := tmux.SplitFields(line)
		if len(fields) != identityFieldCount {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		start, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if tmux.ValidateSessionID(fields[2]) != nil {
			continue
		}
		rows = append(rows, identityRow{pid: pid, startTime: start, id: fields[2], name: fields[3]})
	}
	return rows, complete
}

// soleRow reads a create reply, requiring exactly one row and requiring it to
// name the session we asked for.
//
// Both requirements matter. `new-session -P` describes the session it just
// made, so more than one row means we are not reading what we think we are —
// and a row naming something else is a reply about a different object, which
// must never become a reference we would later close.
func soleRow(out exec.BoundedOutput, want string) (identityRow, bool) {
	rows, complete := parseRows(out)
	if !complete || len(rows) != 1 || rows[0].name != want {
		return identityRow{}, false
	}
	return rows[0], true
}

// parseVersion reads tmux's "tmux 3.7b" / "tmux next-3.4" banner. Only the
// leading major.minor is needed; the suffix letter carries no ordering this
// check depends on.
func parseVersion(s string) (major, minor int, ok bool) {
	digits := strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
	if digits < 0 {
		return 0, 0, false
	}
	rest := s[digits:]
	dot := strings.IndexByte(rest, '.')
	if dot < 0 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(rest[:dot])
	if err != nil {
		return 0, 0, false
	}
	tail := rest[dot+1:]
	end := 0
	for end < len(tail) && tail[end] >= '0' && tail[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(tail[:end])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// stderrEquals compares a bounded stderr against an exact expected line. A
// truncated stream never matches: the comparison is equality, and a prefix is
// not equal.
func stderrEquals(out exec.BoundedOutput, want string) bool {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return false
	}
	return strings.TrimRight(string(raw), "\n") == want
}

// noServer reports tmux's "no server running" family, the one listing failure
// that proves absence rather than ambiguity.
func noServer(out exec.BoundedOutput) bool {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return false
	}
	text := strings.TrimSpace(string(raw))
	return strings.HasPrefix(text, "no server running on ") ||
		strings.HasPrefix(text, "error connecting to ")
}

// classifyRunError maps a runner error onto the closed failure vocabulary.
//
// Everything it cannot recognize becomes FailureUnavailable rather than
// FailureInternal: an unrecognized tmux failure is a statement about tmux, and
// calling it an internal defect would send the reader to the wrong code.
func classifyRunError(err error) backend.StartCause {
	switch {
	case errors.Is(err, context.Canceled):
		return backend.NewStartCause(backend.FailureCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return backend.NewStartCause(backend.FailureTimeout, err)
	case errors.Is(err, os.ErrPermission):
		return backend.NewStartCause(backend.FailurePermissionDenied, err)
	default:
		return backend.NewStartCause(backend.FailureUnavailable, err)
	}
}
