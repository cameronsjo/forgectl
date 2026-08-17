package backend

import (
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// The constructors in this package are the only way an adapter can build a
// result, and they never produce a contradictory one. That makes the
// validators' forbidden-combination branches unreachable from outside — which
// is the point, and also the reason they need testing from inside.
//
// A validator branch nobody can reach is a branch nobody notices rotting. The
// next constructor added here, or the next field, is written against a
// validator whose guarantees are only as good as what still holds. These tests
// construct the illegal values directly, so the guard is pinned by what it
// forbids rather than by which callers happen not to trip it today.

func internalCause() StartCause {
	return NewStartCause(FailureUnavailable, errors.New("backend chatter"))
}

func internalRef(t *testing.T) Ref {
	t.Helper()
	tag, err := NewRecoveryTag()
	if err != nil {
		t.Fatalf("NewRecoveryTag: %v", err)
	}
	id, err := NewTmuxIdentity(tag.OwnershipName())
	if err != nil {
		t.Fatalf("NewTmuxIdentity: %v", err)
	}
	server, err := Fingerprint(IncarnationInput{Endpoint: "/tmp/s", Version: "3.7", Inode: 1})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	ref, err := NewTmuxRef(TmuxDefaultServer(), server, tag, id)
	if err != nil {
		t.Fatalf("NewTmuxRef: %v", err)
	}
	return ref
}

// TestStartResult_ForbiddenFieldCombinations covers the shapes no constructor
// produces. The outcome-unknown-with-a-reference row is the one that matters:
// an ambiguous outcome means there is nothing we are entitled to close, so a
// value carrying both would hand PlanCleanup a target it must not have.
func TestStartResult_ForbiddenFieldCombinations(t *testing.T) {
	ref := internalRef(t)
	tag := ref.Tag()

	tests := map[string]StartResult{
		"outcome-unknown with a reference": {
			outcome: OutcomeUnknown, ref: ref, hasRef: true,
			tag: tag, hasTag: true, cause: internalCause(), hasCause: true,
		},
		"not-mutated with a reference": {
			outcome: NotMutated, ref: ref, hasRef: true,
			cause: internalCause(), hasCause: true,
		},
		"not-mutated with a recovery tag": {
			outcome: NotMutated, tag: tag, hasTag: true,
			cause: internalCause(), hasCause: true,
		},
		"ref-known with a separate tag": {
			outcome: RefKnown, ref: ref, hasRef: true, tag: tag, hasTag: true,
		},
		"outcome-unknown with an invalid tag": {
			outcome: OutcomeUnknown, hasTag: true,
			cause: internalCause(), hasCause: true,
		},
		"an outcome outside the enum": {
			outcome: MutationOutcome(200), cause: internalCause(), hasCause: true,
		},
	}

	for name, res := range tests {
		t.Run(name, func(t *testing.T) {
			if err := res.Validate(); !errors.Is(err, ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}
			// And the decision that consumes it must refuse rather than act on
			// a value it could not validate.
			plan, err := PlanCleanup(res, false)
			if err == nil {
				t.Error("PlanCleanup accepted a contradictory result")
			}
			if _, call := plan.Close(); call {
				t.Error("PlanCleanup asked for a close from a contradictory result")
			}
		})
	}
}

// TestRef_ForbiddenIdentityCombinations pins the exactly-one-identity rule.
// A reference carrying two is a value whose meaning nobody can state: the
// kind-checked accessor would hand out one of them, and a close would target
// whichever the switch happened to reach.
func TestRef_ForbiddenIdentityCombinations(t *testing.T) {
	ref := internalRef(t)

	cmux, err := NewCMuxIdentity("3f2504e0-4f89-41d3-9a0c-0305e82c3301")
	if err != nil {
		t.Fatalf("NewCMuxIdentity: %v", err)
	}
	herdr, err := NewHerdrIdentity("ws-1", "", "")
	if err != nil {
		t.Fatalf("NewHerdrIdentity: %v", err)
	}

	twoIdentities := ref
	twoIdentities.cmux = cmux

	threeIdentities := twoIdentities
	threeIdentities.herdr = herdr

	noIdentity := ref
	noIdentity.tmux = TmuxIdentity{}

	wrongIdentity := ref
	wrongIdentity.tmux = TmuxIdentity{}
	wrongIdentity.cmux = cmux

	tests := map[string]Ref{
		"two identities":               twoIdentities,
		"three identities":             threeIdentities,
		"no identity":                  noIdentity,
		"the other backend's identity": wrongIdentity,
	}

	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			if err := r.Validate(); !errors.Is(err, ErrInvalidRef) {
				t.Errorf("Validate() = %v, want ErrInvalidRef", err)
			}
			if _, err := r.MarshalJSON(); err == nil {
				t.Error("a contradictory reference encoded successfully")
			}
		})
	}

	// The control: the untouched fixture validates, so the refusals above are
	// the identity rule firing rather than the helper building a broken ref.
	if err := ref.Validate(); err != nil {
		t.Fatalf("the fixture reference does not validate: %v", err)
	}
}

// TestCloseResult_SuccessMayNotCarryACause covers the guard CleanupOutcomeFor
// depends on. A close that reports success *and* a failure cannot be acted on,
// and recording it as a satisfied rollback would be the exact lie the typed
// matrix exists to prevent — so it records a failure instead.
func TestCloseResult_SuccessMayNotCarryACause(t *testing.T) {
	tests := map[string]CloseResult{
		"closed with a cause":       {state: CloseClosed, cause: internalCause()},
		"already gone with a cause": {state: CloseAlreadyGone, cause: internalCause()},
		"a state outside the enum":  {state: CloseState(200), cause: internalCause()},
	}

	for name, res := range tests {
		t.Run(name, func(t *testing.T) {
			if err := res.Validate(); !errors.Is(err, ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}
			if got := CleanupOutcomeFor(res); got != CleanupFailed {
				t.Errorf("CleanupOutcomeFor = %v, want CleanupFailed", got)
			}
			if CleanupOutcomeFor(res).Satisfied() {
				t.Error("a contradictory close satisfied the rollback obligation")
			}
		})
	}

	// The control: a well-formed close still records its own outcome, so the
	// rows above are the validation firing rather than the mapping collapsing
	// everything to a failure.
	if got := CleanupOutcomeFor(NewCloseClosed()); got != CleanupClosed {
		t.Errorf("a valid close recorded %v, want CleanupClosed", got)
	}
}

// TestProbeResult_SuccessMayNotCarryACause mirrors the close guard.
func TestProbeResult_SuccessMayNotCarryACause(t *testing.T) {
	tests := map[string]ProbeResult{
		"present with a cause":     {state: ProbePresent, cause: internalCause()},
		"gone with a cause":        {state: ProbeGone, cause: internalCause()},
		"a state outside the enum": {state: ProbeState(200), cause: internalCause()},
	}

	for name, res := range tests {
		t.Run(name, func(t *testing.T) {
			if err := res.Validate(); !errors.Is(err, ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}
		})
	}
}

// TestBootstrapCommand_HasNoRevealEvenInPackage records why one mutation probe
// could not be written: there is no expression inside this package that turns
// a bootstrap back into its payload.
//
// exec.Arg holds its value in a closure with no exported accessor, so the
// containment is not a rendering choice any method here could weaken — it is
// the absence of a route. This test asserts the shape that makes that true, so
// a future field added to BootstrapCommand does not quietly reintroduce one.
func TestBootstrapCommand_HasNoRevealEvenInPackage(t *testing.T) {
	const payload = "forgectl surface _exec --nonce beefcafe"

	b, err := NewBootstrapCommand(exec.Opaque(payload))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}

	// Every rendering reachable from inside, including straight off the field.
	renders := []string{
		b.String(),
		b.GoString(),
		b.arg.String(),
		b.arg.GoString(),
	}
	for _, rendered := range renders {
		if rendered == "" {
			t.Error("a rendering produced nothing; this test would pass vacuously")
		}
		if rendered != "[redacted]" {
			t.Errorf("a rendering produced %q, not the redaction marker", rendered)
		}
	}

	// Equality is the only thing the payload supports, and it is a
	// confirmation oracle rather than a read.
	if !b.arg.Equal(exec.Opaque(payload)) {
		t.Error("the stored argument does not equal the value it was built from")
	}
}
