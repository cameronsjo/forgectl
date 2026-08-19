package tmuxadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Close removes exactly the session a reference names, after proving the server
// is still the incarnation the reference was bound to.
//
// The ordering is the guarantee. Re-resolve, re-fingerprint, compare, find the
// row by exact name, and only then kill — by NATIVE ID, never by name. A name
// handed to `-t` goes through tmux's target grammar, where a missing session
// falls through to a prefix sibling; that is forgectl#237, and it is the reason
// this package never reuses a name past the lookup that found it.
func (a *Adapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	row, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewCloseIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewCloseUnreadable(cause)
	case locateAbsent:
		return backend.NewCloseAlreadyGone()
	case locateFound:
	}

	res, err := a.run.RunSensitive(ctx, a.command(exec.KindTmuxCleanup,
		exec.MustFixed("kill-session"),
		exec.MustFixed("-t"),
		exec.Opaque(row.id),
	))
	if err != nil {
		// A kill that raced someone else's is a satisfied rollback, not a
		// failure: the obligation was that the object be gone, and it is.
		if noServer(res.Stderr) || sessionNotFound(res.Stderr) {
			return backend.NewCloseAlreadyGone()
		}
		return backend.NewCloseFailed(classifyRunError(err))
	}
	return backend.NewCloseClosed()
}

// Probe answers whether the referenced session still exists, under exactly the
// re-resolution and ownership rules Close uses. It runs no mutating command.
func (a *Adapter) Probe(ctx context.Context, ref backend.Ref) backend.ProbeResult {
	_, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewProbeIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewProbeUnreadable(cause)
	case locateAbsent:
		return backend.NewProbeGone()
	case locateFound:
		return backend.NewProbePresent()
	}
	return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureInternal,
		errors.New("unreachable locate state")))
}

// locateState is the shared outcome of "find this reference's session".
type locateState uint8

const (
	locateFound locateState = iota
	locateAbsent
	locateMismatch
	locateUnreadable
)

// locate is the single lookup Close and Probe share.
//
// Sharing it is not just deduplication: the contract requires a probe and a
// close to apply the SAME re-resolution, incarnation, and ownership-name rules,
// and two implementations of that would be two chances to let them drift — with
// the drift showing up as a probe that says present and a close that refuses,
// or worse, the reverse.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (identityRow, locateState, backend.StartCause) {
	if ref.Kind() != backend.KindTmux {
		return identityRow{}, locateUnreadable, backend.NewStartCause(backend.FailureInternal,
			backend.ErrRefKindMismatch)
	}
	// The selection CHAIN must match, not merely the resolved path. A reference
	// taken against the inherited $TMUX server must not be answered by an
	// adapter pinned to the default socket, even on a machine where the two
	// paths happen to coincide today.
	if ref.Source() != a.source {
		return identityRow{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the reference names a different server selection"))
	}

	version, cause := a.readiness(ctx)
	if cause != nil {
		return identityRow{}, locateUnreadable, *cause
	}

	res, err := a.run.RunSensitive(ctx, a.command(exec.KindTmuxProbe,
		exec.MustFixed("list-sessions"),
		exec.MustFixed("-F"),
		exec.Opaque(identityFormat),
	))
	if err != nil {
		if noServer(res.Stderr) {
			// No server at all means the session is certainly gone. The
			// incarnation question is moot: there is no incarnation.
			return identityRow{}, locateAbsent, backend.StartCause{}
		}
		return identityRow{}, locateUnreadable, classifyRunError(err)
	}
	rows, status := parseRows(res.Stdout)
	if status != parseOK {
		// Neither a truncated listing nor an unreadable one can prove absence,
		// and reporting gone here would discharge a rollback obligation that is
		// still outstanding.
		return identityRow{}, locateUnreadable, backend.NewStartCause(backend.FailureMalformedResponse,
			listingUnusable(status))
	}

	want := ref.OwnershipName()
	for _, row := range rows {
		if row.name != want {
			continue
		}
		// The row exists under our name — now prove it is on the incarnation
		// the reference was bound to. A restarted server reuses native ids, so
		// closing without this check is closing a stranger's session that
		// happens to hold $1 today.
		server, ferr := a.fingerprint(row, version)
		if ferr != nil {
			return identityRow{}, locateUnreadable, backend.NewStartCause(backend.FailureMalformedResponse, ferr)
		}
		if !ref.Server().Matches(server) {
			return identityRow{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
				errors.New("the tmux server restarted since this reference was taken"))
		}
		return row, locateFound, backend.StartCause{}
	}

	// Absent by name. Deliberately NOT checked against the incarnation: if no
	// session carries our ownership name, there is nothing to close on any
	// incarnation, and demanding a fingerprint match first would turn a
	// satisfied rollback into an unreadable one.
	return identityRow{}, locateAbsent, backend.StartCause{}
}

// sessionNotFound reports tmux's exact refusal for a target that has since
// disappeared.
func sessionNotFound(out exec.BoundedOutput) bool {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return false
	}
	return hasPrefixTrimmed(string(raw), "can't find session")
}

// hasPrefixTrimmed reports whether s, with surrounding whitespace removed,
// begins with prefix. tmux diagnostics arrive with a trailing newline and
// occasionally a leading one.
func hasPrefixTrimmed(s, prefix string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), prefix)
}
