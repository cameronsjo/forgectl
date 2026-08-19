package tmuxadapter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/sockstat"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// Output caps. A tmux -F reply is a handful of short fields per session, so
// these are generous — but they are bounded, because the completeness flag that
// comes back with a truncated stream is what turns a half-read reply into
// OutcomeUnknown instead of a confident misparse.
//
// stdoutCap is written as the seam's ceiling rather than the literal that
// happens to equal it: validate() REFUSES a cap above MaxOutputBytes, so a
// hardcoded 1<<16 would make every command in this package fail validation at
// once the day that ceiling is lowered.
const (
	stdoutCap = exec.MaxOutputBytes
	stderrCap = 1 << 14
)

// maxStartTimeSeconds is the largest #{start_time} that still fits in int64
// once multiplied into nanoseconds — roughly the year 2262.
const maxStartTimeSeconds int64 = math.MaxInt64 / int64(time.Second)

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
		// Everything past here is the shell-command operand, and it is what
		// makes the created session the SURFACE rather than a login shell: the
		// bootstrap re-enters forgectl carrying the socket path and the one-shot
		// nonce the handshake authenticates. Without it tmux starts the
		// operator's default shell, nothing dials the socket, and every launch
		// dies in the handshake after creating a session it then rolls back.
		//
		// The separator is load-bearing and its correctness is OUR assertion,
		// not the seam's: exec.Opaque refuses a leading dash without one, and
		// tmux honours `--` at new-session (probed on 3.7b — the operand landed
		// in #{pane_start_command} intact).
		exec.EndOfOptions(),
		spec.Bootstrap().SensitiveArg(),
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
		if a.serverAbsent(res.Stderr) {
			return backend.NewNotMutated(cause)
		}
		return backend.NewOutcomeUnknown(tag, cause)
	}
	rows, status := parseRows(res.Stdout)
	if status != parseOK {
		// Neither a truncated listing nor an unreadable one can prove absence:
		// the row may be in the part we did not get, or in a rendering we could
		// not read. Only parseOK is entitled to conclude NotMutated below.
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
			errors.New("tmux reported an unrecognized version"))
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
		StartedAtUnixNano: row.startTime * int64(time.Second),
	}
	sockstat.Fill(&in, info)
	return backend.Fingerprint(in)
}

// identityRow is one parsed -F line.
type identityRow struct {
	pid       int
	startTime int64
	id        string
	name      string
}

// parseStatus says whether a listing may be read as authoritative.
//
// Three states rather than a bool because "we got nothing" and "there is
// nothing" are different answers, and only one of them may be reported as
// absence.
type parseStatus uint8

const (
	// parseOK — the stream arrived whole and yielded usable rows (or was
	// legitimately empty). Only this status may be read as absence.
	parseOK parseStatus = iota
	// parseTruncated — the stream was cut short by the output cap, so the row
	// we are looking for may be in the part we never got.
	parseTruncated
	// parseUnreadable — output was present and NOT ONE line of it was usable.
	//
	// Deliberately named for the property it establishes and not for the cause
	// it usually has. A lost field separator is the reason that matters and the
	// one internal/tmux measured, but a listing can also drop every row because
	// each carried a non-numeric pid, an out-of-range start time, or an id that
	// is not $N. Claiming the separator in the status would send an operator
	// hitting a forged row off to fix their locale.
	parseUnreadable
)

// parseRows splits a listing into rows, dropping any line whose field count is
// not exact, and refuses to report an unreadable listing as an empty one.
//
// Exact, not a minimum: a session NAME may legally contain the field
// separator, and under a `len(f) < N` check such a row would parse with every
// later field shifted one right — offering a well-formed-looking native id
// that names a different session. A separator inside a name can only push the
// count ABOVE the expected one, so requiring equality drops the forged row
// instead of misreading it.
//
// TOTAL parse failure is the fail-closed half, and it is not hypothetical:
// internal/tmux measured tmux 3.7b under a non-UTF-8 locale SUBSTITUTING `_`
// for the separator, lossily, so every row collapses to one field and every
// exact-count check drops it (see internal/tmux/format.go, which takes the same
// contract at parsedRows and explains why there is no rendering-side fix).
// Without this, an operator's LANG turns a live session into a confident "no
// such session" — reported as NotMutated by Start and AlreadyGone by Close,
// orphaning the surface with the harness inside it.
//
// PARTIAL loss stays a silent drop, deliberately and for the same reason
// internal/tmux gives: one row can legitimately fail its count because a name
// carries the separator, and erroring on that would let anyone who can name a
// session break every lookup on the server. Total loss cannot be caused that
// way — every row failing at once means the separator itself is gone.
func parseRows(out exec.BoundedOutput) ([]identityRow, parseStatus) {
	raw, complete := out.CopyBytesForParse()
	var rows []identityRow
	lines := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lines++
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
		// Bounded before it is multiplied into nanos below: ParseInt accepts
		// anything up to int64, and start*1e9 overflows silently past ~9.2e9.
		// No tmux emits a start time beyond this, which is exactly why an
		// unbounded value would be a forged row rather than an old one.
		if start < 0 || start > maxStartTimeSeconds {
			continue
		}
		if tmux.ValidateSessionID(fields[2]) != nil {
			continue
		}
		rows = append(rows, identityRow{pid: pid, startTime: start, id: fields[2], name: fields[3]})
	}
	// Unreadable is checked first because it is the stronger claim: a stream
	// whose separator is gone tells us nothing regardless of whether we got all
	// of it. Both statuses reach the same consumer outcome either way.
	if lines > 0 && len(rows) == 0 {
		return nil, parseUnreadable
	}
	if !complete {
		return rows, parseTruncated
	}
	return rows, parseOK
}

// listingUnusable names why a listing could not be read, so an operator sees
// "fix your locale" rather than "tmux said something odd". Both arms are bare
// sentences with no operand: the diagnostic can reach a manager's pane.
func listingUnusable(status parseStatus) error {
	if status == parseUnreadable {
		return errUnusableListing
	}
	return errors.New("session listing was truncated")
}

// errUnusableListing states only what parseUnreadable establishes. It is
// deliberately NOT tmux.ErrUnreadableFields, whose text names the field
// separator specifically: that is the likeliest cause and the one
// internal/tmux measured, but not the only way every row is dropped, and an
// operator told to fix a locale that is fine has been sent to the wrong place.
var errUnusableListing = errors.New("no line of the tmux session listing was usable")

// soleRow reads a create reply, requiring exactly one row and requiring it to
// name the session we asked for.
//
// Both requirements matter. `new-session -P` describes the session it just
// made, so more than one row means we are not reading what we think we are —
// and a row naming something else is a reply about a different object, which
// must never become a reference we would later close.
func soleRow(out exec.BoundedOutput, want string) (identityRow, bool) {
	rows, status := parseRows(out)
	if status != parseOK || len(rows) != 1 || rows[0].name != want {
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

// noServer reports tmux's one listing failure that proves absence rather than
// ambiguity: there is no server, so no server is hiding our session.
//
// It matches THAT diagnostic and no other. "error connecting to <path> (…)"
// looks like a sibling and is not — tmux formats it as
// `error connecting to %s (%s)` with strerror(errno), so a RUNNING server
// whose socket the caller cannot open answers with it. Measured on 3.7b:
// chmod 000 on a live server's socket yields
// "error connecting to /tmp/…/s (Permission denied)" with the server still up
// and its sessions intact. Reading that as absence made reconcile report
// NotMutated for a session that exists and made Close discharge a rollback
// obligation that was still outstanding — both fail-open, in the direction
// that leaks a live surface.
func noServer(out exec.BoundedOutput) bool {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return false
	}
	return hasPrefixTrimmed(string(raw), "no server running on ")
}

// serverAbsent decides whether a failed listing PROVES there is no server,
// which is the one listing failure the contract lets us read as absence.
//
// It asks the filesystem rather than trusting the diagnostic, because the
// diagnostics do not partition the way they look like they do. Measured on
// 3.7b: "no server running on <path>" appears only when the socket FILE exists
// and the server behind it is dead; when the socket is simply absent — a clean
// machine, or after a /tmp sweep — tmux says
// "error connecting to <path> (No such file or directory)", which shares its
// prefix with the permission case that proves nothing.
//
// So the text is a fast path and the stat is the proof. The stat is also
// locale-proof, which the text is not: the parenthesised half is strerror(3)
// and translates, and this package has already been bitten once by tmux
// rendering differently under a non-UTF-8 locale.
func (a *Adapter) serverAbsent(out exec.BoundedOutput) bool {
	if noServer(out) {
		return true
	}
	_, err := a.lstat(a.socket)
	return errors.Is(err, os.ErrNotExist)
}

// classifyRunError maps a runner error onto the closed failure vocabulary.
//
// It matches the SEAM's sentinels, not context's or os's. The sensitive runner
// returns only *exec.SensitiveError, and that type unwraps to its outcome's
// package sentinel and deliberately never to the underlying error — whose text
// can carry the path that failed to start. So `errors.Is(err, context.Canceled)`
// is structurally false even for a command the runner killed on cancellation,
// and classifying on it would leave FailureCanceled and FailureTimeout
// unreachable while every failure reported FailureUnavailable.
//
// FailurePermissionDenied is deliberately NOT in that list, though an earlier
// version of this comment claimed it: the seam publishes no permission
// sentinel, so that class is unreachable from here under either scheme. It is
// produced where a permission question is actually asked — the socket-ownership
// checks — never by classifying a runner error.
//
// Everything it cannot recognize becomes FailureUnavailable rather than
// FailureInternal: an unrecognized tmux failure is a statement about tmux, and
// calling it an internal defect would send the reader to the wrong code.
func classifyRunError(err error) backend.StartCause {
	switch {
	case errors.Is(err, exec.ErrCanceled):
		return backend.NewStartCause(backend.FailureCanceled, err)
	case errors.Is(err, exec.ErrTimeout):
		return backend.NewStartCause(backend.FailureTimeout, err)
	case errors.Is(err, exec.ErrInvalidCommand):
		// Refused before start: the argv this package built is wrong, which is
		// our defect and not the operator's tmux.
		return backend.NewStartCause(backend.FailureInternal, err)
	default:
		return backend.NewStartCause(backend.FailureUnavailable, err)
	}
}
