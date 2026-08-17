package backend

import (
	"errors"
	"strings"
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

	tests := map[string]struct {
		result StartResult
		// wantClose is true only where the result claims RefKnown and carries
		// a reference that validates on its own. Those name a real object we
		// created, so refusing to plan their close would orphan it over a
		// bookkeeping mistake; every other shape is asserting that we do not
		// know we created anything, and gets no close.
		wantClose bool
	}{
		"outcome-unknown with a reference": {result: StartResult{
			outcome: OutcomeUnknown, ref: ref, hasRef: true,
			tag: tag, hasTag: true, cause: internalCause(), hasCause: true,
		}},
		"not-mutated with a reference": {result: StartResult{
			outcome: NotMutated, ref: ref, hasRef: true,
			cause: internalCause(), hasCause: true,
		}},
		"not-mutated with a recovery tag": {result: StartResult{
			outcome: NotMutated, tag: tag, hasTag: true,
			cause: internalCause(), hasCause: true,
		}},
		"outcome-unknown with an invalid tag": {result: StartResult{
			outcome: OutcomeUnknown, hasTag: true,
			cause: internalCause(), hasCause: true,
		}},
		"an outcome outside the enum": {result: StartResult{
			outcome: MutationOutcome(200), cause: internalCause(), hasCause: true,
		}},
		// A cause present in the field but never declared by a constructor.
		// Cause() reports absent for these, so validating them would mean a
		// result whose failure nothing downstream can see.
		"not-mutated with an undeclared cause": {result: StartResult{
			outcome: NotMutated, cause: internalCause(),
		}},
		"outcome-unknown with an undeclared cause": {result: StartResult{
			outcome: OutcomeUnknown, tag: tag, hasTag: true, cause: internalCause(),
		}},
		"ref-known with a separate tag": {
			result: StartResult{
				outcome: RefKnown, ref: ref, hasRef: true, tag: tag, hasTag: true,
			},
			wantClose: true,
		},
		"ref-known with an unclassified cause": {
			result:    NewRefKnownWithCause(ref, StartCause{}),
			wantClose: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if err := tc.result.Validate(); !errors.Is(err, ErrInvalidResult) {
				t.Errorf("Validate() = %v, want ErrInvalidResult", err)
			}

			plan, err := PlanCleanup(tc.result, false)
			if err == nil {
				t.Error("PlanCleanup accepted a contradictory result")
			}
			gotRef, gotCall := plan.Close()
			if gotCall != tc.wantClose {
				t.Fatalf("PlanCleanup close = %v, want %v", gotCall, tc.wantClose)
			}
			if tc.wantClose && gotRef != ref {
				t.Errorf("PlanCleanup planned a close against %v, want the result's own reference", gotRef)
			}

			// A committed launch never closes, whatever the result claims.
			committed, _ := PlanCleanup(tc.result, true)
			if _, call := committed.Close(); call {
				t.Error("PlanCleanup asked for a close on a committed launch")
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

// TestClosedEnums_EveryConstantIsNamed walks each enum to its unexported count
// sentinel.
//
// The external version of this test cannot: it can only loop to the last
// *named* constant it can see, so appending a constant and forgetting its
// name-table entry leaves the loop stopping short — the array grows with an
// empty default, String returns "", and the test written to catch exactly that
// passes. Only an in-package test can reach the sentinel, which is where the
// real bound lives.
func TestClosedEnums_EveryConstantIsNamed(t *testing.T) {
	enums := map[string]struct {
		count int
		name  func(int) string
	}{
		"Kind":              {int(kindCount), func(i int) string { return Kind(i).String() }},
		"StartFailureClass": {int(failureClassCount), func(i int) string { return StartFailureClass(i).String() }},
		"Phase":             {int(phaseCount), func(i int) string { return Phase(i).String() }},
		"MutationOutcome":   {int(mutationOutcomeCount), func(i int) string { return MutationOutcome(i).String() }},
		"CloseState":        {int(closeStateCount), func(i int) string { return CloseState(i).String() }},
		"ProbeState":        {int(probeStateCount), func(i int) string { return ProbeState(i).String() }},
		"CleanupOutcome":    {int(cleanupOutcomeCount), func(i int) string { return CleanupOutcome(i).String() }},
	}

	for enum, spec := range enums {
		t.Run(enum, func(t *testing.T) {
			if spec.count < 2 {
				t.Fatalf("count sentinel is %d; the enum is not being walked", spec.count)
			}
			seen := make(map[string]bool, spec.count)
			for i := range spec.count {
				got := spec.name(i)
				switch {
				case got == "":
					t.Errorf("value %d has no entry in the name table", i)
				case strings.HasPrefix(got, "invalid("):
					t.Errorf("value %d renders as %q; it is inside the enum but unnamed", i, got)
				case seen[got]:
					t.Errorf("value %d reuses the name %q", i, got)
				}
				seen[got] = true
			}
			// One past the sentinel must render as an invalid marker, which is
			// what proves the walk stopped at the real end rather than short of
			// it.
			if got := spec.name(spec.count); !strings.HasPrefix(got, "invalid(") {
				t.Errorf("the value past the sentinel renders as %q, not an invalid marker", got)
			}
		})
	}
}

// TestStartSpec_RefusesAStructLiteralBuild covers the shape the unexported
// fields make unconstructible outside this package: a spec whose two
// string-valued fields were assigned directly rather than sealed by
// NewStartSpec.
//
// The point is not that an external caller could do this — they cannot — but
// that a future in-package one could, and such a spec would be *valid enough*
// to reach an adapter while carrying values that render in the clear and that
// never passed the text checks. Validate asks whether the seal is present
// rather than whether the fields look plausible.
func TestStartSpec_RefusesAStructLiteralBuild(t *testing.T) {
	tag, err := NewRecoveryTag()
	if err != nil {
		t.Fatalf("NewRecoveryTag: %v", err)
	}
	boot, err := NewBootstrapCommand(exec.Opaque("forgectl surface _exec --nonce beefcafe"))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}

	// Everything a hand-built spec would plausibly set, except the seal.
	unsealed := StartSpec{tag: tag, bootstrap: boot}
	if err := unsealed.Validate(); !errors.Is(err, ErrInvalidStartSpec) {
		t.Errorf("an unsealed spec validated: err = %v", err)
	}

	// Half-sealed is still unsealed: both values must have gone through the
	// constructor, or one of them skipped the text checks.
	halfSealed := StartSpec{cwd: exec.Opaque("/tmp/x"), tag: tag, bootstrap: boot}
	if err := halfSealed.Validate(); !errors.Is(err, ErrInvalidStartSpec) {
		t.Errorf("a half-sealed spec validated: err = %v", err)
	}

	// The control: the real constructor produces a spec that validates, so the
	// refusals above are the seal check firing rather than Validate refusing
	// everything.
	sealed, err := NewStartSpec("/tmp/x", "x", tag, boot)
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}
	if err := sealed.Validate(); err != nil {
		t.Errorf("a sealed spec was refused: %v", err)
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
