package backend_test

import (
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func cause() backend.StartCause {
	return backend.NewStartCause(backend.FailureUnavailable, errors.New("tmux: no server running"))
}

// TestStartResult_ZeroValueIsNotARefusal is the single most important
// assertion in this package.
//
// NotMutated is the zero value of MutationOutcome, so a StartResult nobody
// filled in reads as "definitively nothing was created" — the most dangerous
// default available, and the one an adapter produces by returning early or by
// hitting a nil-function path in a fake. Requiring a classified cause is what
// makes the zero value fail instead.
func TestStartResult_ZeroValueIsNotARefusal(t *testing.T) {
	var zero backend.StartResult

	if zero.Outcome() != backend.NotMutated {
		t.Fatal("the zero result no longer reads as not-mutated; this test is testing the wrong thing")
	}
	if err := zero.Validate(); !errors.Is(err, backend.ErrInvalidResult) {
		t.Errorf("the zero start result validated: err = %v", err)
	}
}

// TestStartResult_ValidMatrix pins the four legal shapes. Each row is a state
// an adapter is entitled to report, and each carries exactly the fields that
// state can justify.
func TestStartResult_ValidMatrix(t *testing.T) {
	ref, _ := tmuxRef(t)
	tag := recoveryTag(t)

	tests := []struct {
		name    string
		result  backend.StartResult
		outcome backend.MutationOutcome
		wantRef bool
		wantTag bool
		failed  bool
	}{
		{
			name:    "definitive pre-mutation refusal",
			result:  backend.NewNotMutated(cause()),
			outcome: backend.NotMutated,
			failed:  true,
		},
		{
			name:    "successful start",
			result:  backend.NewRefKnown(ref),
			outcome: backend.RefKnown,
			wantRef: true,
			wantTag: true,
		},
		{
			name:    "created, then failed later",
			result:  backend.NewRefKnownWithCause(ref, cause()),
			outcome: backend.RefKnown,
			wantRef: true,
			wantTag: true,
			failed:  true,
		},
		{
			name:    "ambiguous daemon outcome",
			result:  backend.NewOutcomeUnknown(tag, cause()),
			outcome: backend.OutcomeUnknown,
			wantTag: true,
			failed:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.result.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := tc.result.Outcome(); got != tc.outcome {
				t.Errorf("Outcome() = %v, want %v", got, tc.outcome)
			}
			if _, ok := tc.result.Ref(); ok != tc.wantRef {
				t.Errorf("Ref() present = %v, want %v", ok, tc.wantRef)
			}
			if _, ok := tc.result.RecoveryTag(); ok != tc.wantTag {
				t.Errorf("RecoveryTag() present = %v, want %v", ok, tc.wantTag)
			}
			if got := tc.result.Failed(); got != tc.failed {
				t.Errorf("Failed() = %v, want %v", got, tc.failed)
			}
		})
	}
}

// TestStartResult_RefKnownTagComesFromTheRef proves the tag is read back off
// the reference rather than stored beside it. Two independently-held copies of
// one value are two values that can disagree, and the disagreement would show
// up as a reconciliation matching an object the reference does not name.
func TestStartResult_RefKnownTagComesFromTheRef(t *testing.T) {
	ref, tag := tmuxRef(t)

	got, ok := backend.NewRefKnown(ref).RecoveryTag()
	if !ok {
		t.Fatal("a ref-known result reports no recovery tag")
	}
	if got != tag {
		t.Errorf("recovery tag = %q, want the reference's own tag %q", got, tag)
	}
}

// TestStartResult_InvalidCombinationsFailValidation is the refusal table. Each
// row is a claim an adapter must not be able to make stick: a refusal with no
// reason, an ambiguity with a reference attached, a success with nothing in it.
func TestStartResult_InvalidCombinationsFailValidation(t *testing.T) {
	ref, _ := tmuxRef(t)
	var noRef backend.Ref
	var noTag backend.RecoveryTag

	tests := map[string]backend.StartResult{
		"refusal with no cause":              backend.NewNotMutated(backend.StartCause{}),
		"refusal with an unclassified cause": backend.NewNotMutated(backend.NewStartCause(backend.FailureUnspecified, errors.New("boom"))),
		"success with no reference":          backend.NewRefKnown(noRef),
		"success with an invalid reference":  backend.NewRefKnown(backend.Ref{}),
		"post-create failure, no cause":      backend.NewRefKnownWithCause(ref, backend.StartCause{}),
		"post-create failure, bad class":     backend.NewRefKnownWithCause(ref, backend.NewStartCause(backend.StartFailureClass(200), nil)),
		"ambiguity with no tag":              backend.NewOutcomeUnknown(noTag, cause()),
		"ambiguity with no cause":            backend.NewOutcomeUnknown(recoveryTag(t), backend.StartCause{}),
	}

	for name, res := range tests {
		t.Run(name, func(t *testing.T) {
			if err := res.Validate(); !errors.Is(err, backend.ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}
		})
	}
}

// TestPlanCleanup_Matrix is the rollback decision, made once from typed state.
//
// The committed row is the one that matters most: after the acknowledgement,
// ownership has moved to the surface and a late caller cancellation must not
// reach in and close a running harness. Deciding that here — from a value —
// rather than in a defer is what keeps it from racing the commit.
func TestPlanCleanup_Matrix(t *testing.T) {
	ref, _ := tmuxRef(t)
	tag := recoveryTag(t)

	tests := []struct {
		name      string
		result    backend.StartResult
		committed bool
		wantCall  bool
		wantRef   backend.Ref
		want      backend.CleanupOutcome
	}{
		{
			name:   "nothing was created",
			result: backend.NewNotMutated(cause()),
			want:   backend.CleanupNotApplicable,
		},
		{
			name:   "the daemon may have created something",
			result: backend.NewOutcomeUnknown(tag, cause()),
			want:   backend.CleanupUnavailableUnknown,
		},
		{
			name:     "created and uncommitted",
			result:   backend.NewRefKnown(ref),
			wantCall: true,
			wantRef:  ref,
		},
		{
			name:     "created, later failure, uncommitted",
			result:   backend.NewRefKnownWithCause(ref, cause()),
			wantCall: true,
			wantRef:  ref,
		},
		{
			name:      "created and committed",
			result:    backend.NewRefKnown(ref),
			committed: true,
			want:      backend.CleanupSkippedCommitted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := backend.PlanCleanup(tc.result, tc.committed)
			if err != nil {
				t.Fatalf("PlanCleanup: %v", err)
			}

			gotRef, gotCall := plan.Close()
			if gotCall != tc.wantCall {
				t.Fatalf("Close() call = %v, want %v", gotCall, tc.wantCall)
			}
			if tc.wantCall {
				if gotRef != tc.wantRef {
					t.Errorf("Close() ref = %v, want %v", gotRef, tc.wantRef)
				}
				return
			}
			if got := plan.Outcome(); got != tc.want {
				t.Errorf("Outcome() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanCleanup_RefusesAnInvalidResult stops a contradictory adapter answer
// from steering rollback. An adapter that cannot describe what it did has not
// earned a close call against a reference it may have invented.
func TestPlanCleanup_RefusesAnInvalidResult(t *testing.T) {
	plan, err := backend.PlanCleanup(backend.StartResult{}, false)
	if !errors.Is(err, backend.ErrInvalidResult) {
		t.Errorf("PlanCleanup err = %v, want ErrInvalidResult", err)
	}
	if _, call := plan.Close(); call {
		t.Error("a refused plan still asked for a close")
	}
}

// TestCleanupOutcome_OnlyConfirmedAbsenceSatisfiesRollback is the honesty
// assertion. An unreadable server or a refused close leaves an object behind,
// and reporting either as a completed rollback is exactly the lie this whole
// typed matrix exists to make unwriteable.
func TestCleanupOutcome_OnlyConfirmedAbsenceSatisfiesRollback(t *testing.T) {
	satisfied := map[backend.CleanupOutcome]bool{
		backend.CleanupNotApplicable:      true,
		backend.CleanupSkippedCommitted:   true,
		backend.CleanupClosed:             true,
		backend.CleanupAlreadyGone:        true,
		backend.CleanupUnavailableUnknown: false,
		backend.CleanupIdentityMismatch:   false,
		backend.CleanupUnreadable:         false,
		backend.CleanupFailed:             false,
		backend.CleanupUnspecified:        false,
	}

	for outcome, want := range satisfied {
		if got := outcome.Satisfied(); got != want {
			t.Errorf("%v.Satisfied() = %v, want %v", outcome, got, want)
		}
	}
}

// TestCleanupOutcomeFor_MapsEveryCloseState covers the conversion from an
// attempted close to its record, including the case an adapter should not be
// able to produce: an invalid close result records a failure, because a close
// that cannot say what it did has not demonstrated a cleanup.
func TestCleanupOutcomeFor_MapsEveryCloseState(t *testing.T) {
	tests := []struct {
		name   string
		result backend.CloseResult
		want   backend.CleanupOutcome
	}{
		{"closed", backend.NewCloseClosed(), backend.CleanupClosed},
		{"already gone", backend.NewCloseAlreadyGone(), backend.CleanupAlreadyGone},
		{"identity mismatch", backend.NewCloseIdentityMismatch(cause()), backend.CleanupIdentityMismatch},
		{"unreadable", backend.NewCloseUnreadable(cause()), backend.CleanupUnreadable},
		{"failed", backend.NewCloseFailed(cause()), backend.CleanupFailed},
		// An adapter that returns a result it never constructed has not
		// demonstrated a cleanup, so the record is a failure rather than the
		// zero state's benefit of the doubt.
		{"unset", backend.CloseResult{}, backend.CleanupFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := backend.CleanupOutcomeFor(tc.result); got != tc.want {
				t.Errorf("CleanupOutcomeFor(%v) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
}

// TestCloseResult_Matrix pins which close states may carry a cause. The two
// success states must not: a close that succeeded and also carries a failure
// is a value nobody can act on.
func TestCloseResult_Matrix(t *testing.T) {
	valid := []struct {
		name     string
		result   backend.CloseResult
		state    backend.CloseState
		rollback bool
	}{
		{"closed", backend.NewCloseClosed(), backend.CloseClosed, true},
		{"already gone", backend.NewCloseAlreadyGone(), backend.CloseAlreadyGone, true},
		{"identity mismatch", backend.NewCloseIdentityMismatch(cause()), backend.CloseIdentityMismatch, false},
		{"unreadable", backend.NewCloseUnreadable(cause()), backend.CloseUnreadable, false},
		{"failed", backend.NewCloseFailed(cause()), backend.CloseFailed, false},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.result.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := tc.result.State(); got != tc.state {
				t.Errorf("State() = %v, want %v", got, tc.state)
			}
			if got := tc.result.State().SatisfiesRollback(); got != tc.rollback {
				t.Errorf("SatisfiesRollback() = %v, want %v", got, tc.rollback)
			}
		})
	}

	invalid := map[string]backend.CloseResult{
		"unset":                       {},
		"mismatch with no cause":      backend.NewCloseIdentityMismatch(backend.StartCause{}),
		"unreadable with no cause":    backend.NewCloseUnreadable(backend.StartCause{}),
		"failed with an unclassified": backend.NewCloseFailed(backend.NewStartCause(backend.FailureUnspecified, errors.New("x"))),
	}
	for name, res := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := res.Validate(); !errors.Is(err, backend.ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}
		})
	}
}

// TestProbeResult_Matrix mirrors the close matrix, plus the distinction that
// makes a probe usable: only present and gone answer the existence question. A
// mismatched or unreadable server is a probe that ran and learned nothing, and
// reading that as absence is how a reconciliation concludes "nothing was
// created" about a workspace that exists.
func TestProbeResult_Matrix(t *testing.T) {
	valid := []struct {
		name       string
		result     backend.ProbeResult
		state      backend.ProbeState
		conclusive bool
	}{
		{"present", backend.NewProbePresent(), backend.ProbePresent, true},
		{"gone", backend.NewProbeGone(), backend.ProbeGone, true},
		{"identity mismatch", backend.NewProbeIdentityMismatch(cause()), backend.ProbeIdentityMismatch, false},
		{"unreadable", backend.NewProbeUnreadable(cause()), backend.ProbeUnreadable, false},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.result.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := tc.result.State(); got != tc.state {
				t.Errorf("State() = %v, want %v", got, tc.state)
			}
			if got := tc.result.State().Conclusive(); got != tc.conclusive {
				t.Errorf("Conclusive() = %v, want %v", got, tc.conclusive)
			}
		})
	}

	// A ProbePresent carrying a cause is unconstructible from out here — the
	// success constructors take no cause — so that shape is covered in-package
	// instead, in validate_internal_test.go.
	invalid := map[string]backend.ProbeResult{
		"unset":                    {},
		"mismatch with no cause":   backend.NewProbeIdentityMismatch(backend.StartCause{}),
		"unreadable with no cause": backend.NewProbeUnreadable(backend.StartCause{}),
	}
	for name, res := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := res.Validate(); !errors.Is(err, backend.ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}
		})
	}
}

// TestRetryable_OnlyPreMutationCollision keeps a retry from following a
// failure that may already have mutated the daemon. Every class but a
// definitive pre-creation name collision leaves that possibility open, and a
// retry there creates a second workspace nobody owns.
//
// This list is what the exported surface can name, so it cannot see a class
// appended later. TestRetryable_WalksEveryFailureClass holds the closed set
// closed, in-package, where the count sentinel is reachable.
func TestRetryable_OnlyPreMutationCollision(t *testing.T) {
	all := []backend.StartFailureClass{
		backend.FailureUnspecified,
		backend.FailureUnavailable,
		backend.FailureIncompatible,
		backend.FailurePermissionDenied,
		backend.FailureAuthentication,
		backend.FailureNameCollision,
		backend.FailureMalformedResponse,
		backend.FailureTimeout,
		backend.FailureCanceled,
		backend.FailureIdentityMismatch,
		backend.FailureInternal,
	}

	retryable := 0
	for _, class := range all {
		if class.Retryable() {
			retryable++
			if class != backend.FailureNameCollision {
				t.Errorf("%v is retryable; only a pre-mutation name collision may be", class)
			}
		}
	}
	if retryable != 1 {
		t.Errorf("%d classes are retryable, want exactly 1", retryable)
	}
}
