package backend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// backendChatter stands in for what a manager CLI actually puts on stderr: the
// socket it was pinned to, the directory it was asked to open, and an echo of
// the command line — which for a surface launch is the bootstrap, nonce
// included.
const backendChatter = "tmux: failed to connect to /private/tmp/fc-a1b2/sock " +
	"while opening /Users/someone/Projects/secret-thing: " +
	"forgectl surface _exec --nonce deadbeefcafe0123"

// TestStartCause_RendersOnlyItsClass is the containment assertion on the type
// that wraps backend errors.
//
// Every one of these verbs is a way a cause reaches a terminal or a log file,
// and the wrapped original is a string a manager chose. The test asserts each
// rendering is non-empty first, because a type that renders nothing would pass
// every "does not contain" check while telling an operator nothing.
func TestStartCause_RendersOnlyItsClass(t *testing.T) {
	cause := backend.NewStartCause(backend.FailureUnavailable, errors.New(backendChatter))

	buf := &bytes.Buffer{}
	slog.New(slog.NewTextHandler(buf, nil)).Error("start failed", "cause", cause)

	marshaled, err := json.Marshal(cause)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	text, err := cause.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}

	renders := map[string]string{
		"Error()": cause.Error(),
		"%v":      fmt.Sprintf("%v", cause),
		"%+v":     fmt.Sprintf("%+v", cause),
		"%#v":     fmt.Sprintf("%#v", cause),
		// The %s and %q rows are the point of this test, not accidental
		// Sprintfs: a String method is what makes them "redundant", and the
		// assertion is that every verb redacts, including those.
		"%s":            fmt.Sprintf("%s", cause), //nolint:gosimple,staticcheck // exercising the verb is the assertion
		"%q":            fmt.Sprintf("%q", cause),
		"wrapped %v":    fmt.Sprintf("%v", fmt.Errorf("launch: %w", cause)),
		"slog":          buf.String(),
		"json":          string(marshaled),
		"MarshalText()": string(text),
	}

	for verb, rendered := range renders {
		if rendered == "" {
			t.Errorf("%s rendered nothing; this test would pass vacuously", verb)
			continue
		}
		if !strings.Contains(rendered, "backend-unavailable") {
			t.Errorf("%s = %q, which does not name the failure class", verb, rendered)
		}
		for _, leak := range []string{"/private/tmp/fc-a1b2/sock", "secret-thing", "deadbeefcafe0123", "_exec"} {
			if strings.Contains(rendered, leak) {
				t.Errorf("%s leaked %q: %s", verb, leak, rendered)
			}
		}
	}
}

// TestStartCause_ClassifiesWithoutSurrenderingTheOriginal covers the narrow
// seam the type deliberately leaves open. errors.Is must be able to ask what
// the underlying failure was; nothing may be able to obtain the value in order
// to print it.
func TestStartCause_ClassifiesWithoutSurrenderingTheOriginal(t *testing.T) {
	original := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	cause := backend.NewStartCause(backend.FailureTimeout, original)

	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Error("errors.Is cannot classify the wrapped cause")
	}
	if errors.Is(cause, context.Canceled) {
		t.Error("errors.Is matched a cause that is not there")
	}

	// Unwrap must not hand the original over: a caller that obtains it can
	// print the manager's stderr, which is exactly what the class-only
	// rendering exists to prevent.
	if unwrapped := errors.Unwrap(cause); unwrapped != nil {
		t.Errorf("errors.Unwrap returned %v; the chain must stop at the class", unwrapped)
	}

	// errors.As must not reach it either. It ignores the custom Is method and
	// walks Unwrap, so the absence of Unwrap is what closes this route.
	var target *bespokeError
	if errors.As(cause, &target) {
		t.Error("errors.As walked past the cause to a renderable value")
	}

	// The control: errors.As does find this type when it is not behind a
	// StartCause, so the negative result above is the barrier working rather
	// than the test asking an unanswerable question.
	if !errors.As(fmt.Errorf("wrapped: %w", &bespokeError{}), &target) {
		t.Error("errors.As cannot find the target type at all; the negative case proves nothing")
	}
}

type bespokeError struct{}

func (*bespokeError) Error() string { return "bespoke" }

// TestStartCause_UnsetIsNotAClassification keeps a cause nobody filled in from
// passing for a real one.
func TestStartCause_UnsetIsNotAClassification(t *testing.T) {
	var zero backend.StartCause
	if zero.Valid() {
		t.Error("the zero cause reports itself valid")
	}
	if errors.Is(zero, context.Canceled) {
		t.Error("the zero cause matched a target")
	}
}

// TestLaunchError_KeepsTheFourFactsApart is the reason the type has four
// fields instead of a message. A failed close after a failed create is two
// facts, and an operator who only hears the first leaves a workspace behind.
func TestLaunchError_KeepsTheFourFactsApart(t *testing.T) {
	tag := recoveryTag(t)

	tests := []struct {
		name     string
		err      *backend.LaunchError
		contains []string
	}{
		{
			name: "nothing was created",
			err: &backend.LaunchError{
				Phase:    backend.PhasePreflight,
				Backend:  backend.KindTmux,
				Mutation: backend.NotMutated,
				Cause:    backend.NewStartCause(backend.FailureUnavailable, errors.New(backendChatter)),
				Cleanup:  backend.CleanupNotApplicable,
			},
			contains: []string{"tmux", "preflight", "backend-unavailable"},
		},
		{
			name: "created and cleaned up",
			err: &backend.LaunchError{
				Phase:    backend.PhaseHandshake,
				Backend:  backend.KindCmux,
				Mutation: backend.RefKnown,
				Cause:    backend.NewStartCause(backend.FailureTimeout, nil),
				Cleanup:  backend.CleanupClosed,
			},
			contains: []string{"cmux", "handshake", "timeout", "closed"},
		},
		{
			name: "created, and cleanup failed too",
			err: &backend.LaunchError{
				Phase:    backend.PhaseCommit,
				Backend:  backend.KindCmux,
				Mutation: backend.RefKnown,
				Cause:    backend.NewStartCause(backend.FailureTimeout, nil),
				Cleanup:  backend.CleanupFailed,
			},
			contains: []string{"timeout", "cleanup failed"},
		},
		{
			name: "the daemon left it ambiguous",
			err: &backend.LaunchError{
				Phase:    backend.PhaseCreate,
				Backend:  backend.KindHerdr,
				Mutation: backend.OutcomeUnknown,
				Cause:    backend.NewStartCause(backend.FailureMalformedResponse, errors.New(backendChatter)),
				Cleanup:  backend.CleanupUnavailableUnknown,
				Recovery: tag,
			},
			// An ambiguous outcome has to hand the operator something to look
			// for, or "we may have created a workspace" is unactionable.
			contains: []string{"herdr", "malformed-response", tag.OwnershipName()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			if msg == "" {
				t.Fatal("rendered nothing")
			}
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q omits %q", msg, want)
				}
			}
			for _, leak := range []string{"/private/tmp/fc-a1b2/sock", "secret-thing", "deadbeefcafe0123"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message leaked %q: %s", leak, msg)
				}
			}
			if !errors.Is(tc.err, tc.err.Cause) {
				t.Error("errors.Is cannot reach the class-only cause")
			}
		})
	}
}

// TestClosedEnums_NameEveryValue catches an enum that gained a constant
// without gaining a name — which renders as "invalid(7)" in a log and reads as
// a corruption rather than as a missing table entry.
func TestClosedEnums_NameEveryValue(t *testing.T) {
	stringers := map[string][]fmt.Stringer{}

	for k := backend.KindTmux; k <= backend.KindHerdr; k++ {
		stringers["Kind"] = append(stringers["Kind"], k)
	}
	for c := backend.FailureUnavailable; c <= backend.FailureInternal; c++ {
		stringers["StartFailureClass"] = append(stringers["StartFailureClass"], c)
	}
	for p := backend.PhaseResolve; p <= backend.PhaseCommit; p++ {
		stringers["Phase"] = append(stringers["Phase"], p)
	}
	for m := backend.NotMutated; m <= backend.OutcomeUnknown; m++ {
		stringers["MutationOutcome"] = append(stringers["MutationOutcome"], m)
	}
	for s := backend.CloseClosed; s <= backend.CloseFailed; s++ {
		stringers["CloseState"] = append(stringers["CloseState"], s)
	}
	for s := backend.ProbePresent; s <= backend.ProbeUnreadable; s++ {
		stringers["ProbeState"] = append(stringers["ProbeState"], s)
	}
	for o := backend.CleanupNotApplicable; o <= backend.CleanupFailed; o++ {
		stringers["CleanupOutcome"] = append(stringers["CleanupOutcome"], o)
	}

	for name, values := range stringers {
		if len(values) < 2 {
			t.Errorf("%s: only %d values enumerated; the loop bounds are wrong", name, len(values))
		}
		seen := make(map[string]bool, len(values))
		for _, v := range values {
			s := v.String()
			switch {
			case s == "":
				t.Errorf("%s: a value renders as the empty string", name)
			case strings.HasPrefix(s, "invalid("):
				t.Errorf("%s: %s has no name in its table", name, s)
			case seen[s]:
				t.Errorf("%s: %q is used by two values", name, s)
			}
			seen[s] = true
		}
	}
}

// TestClosedEnums_RenderOutOfRangeValuesSafely covers the other side: a value
// cast in from outside must render as an obvious invalid marker rather than
// panicking on a table index.
func TestClosedEnums_RenderOutOfRangeValuesSafely(t *testing.T) {
	renders := []string{
		backend.Kind(200).String(),
		backend.StartFailureClass(200).String(),
		backend.Phase(200).String(),
		backend.MutationOutcome(200).String(),
		backend.CloseState(200).String(),
		backend.ProbeState(200).String(),
		backend.CleanupOutcome(200).String(),
	}
	for _, s := range renders {
		if !strings.HasPrefix(s, "invalid(") {
			t.Errorf("an out-of-range value rendered as %q, not an invalid marker", s)
		}
	}
}
