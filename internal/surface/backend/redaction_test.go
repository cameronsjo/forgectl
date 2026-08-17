package backend_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	fcexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// chattyError is a value-typed error with an exported field, which is the
// shape that defeats a naive containment.
//
// fmt reaches a value held in an *unexported* struct field by reflection,
// where it cannot satisfy the Formatter or Stringer interfaces, so it walks the
// fields instead. A cause holding one of these directly — rather than behind a
// pointer — prints its contents in full at any nesting depth, and the contents
// are a manager CLI's stderr.
type chattyError struct{ Stderr string }

func (e chattyError) Error() string { return e.Stderr }

// sliceError is uncomparable: errors.Is compares a comparable target with ==,
// and two causes wrapping the same uncomparable dynamic type would panic on
// that comparison if the cause held the error directly.
type sliceError struct{ Frames []string }

func (e sliceError) Error() string { return strings.Join(e.Frames, "/") }

const managerChatter = "tmux: /private/tmp/fc-a1b2/sock: connect failed; " +
	"cwd=/Users/someone/Projects/secret-thing"

// TestStartCause_ContainsAValueTypedErrorUnderReflection is the containment
// test that matters, because it covers the route the direct renderings do not.
//
// Every String/Format/LogValue path on a StartCause is only consulted when fmt
// can see the value as an interface. Nested in an unexported field it cannot,
// and a wrapped error stored inline would then be printed field by field.
func TestStartCause_ContainsAValueTypedErrorUnderReflection(t *testing.T) {
	cause := backend.NewStartCause(backend.FailureUnavailable, chattyError{Stderr: managerChatter})

	// Both wrappers hold the value in an unexported field, which is the shape
	// an adapter or a result type naturally has.
	type holdsCause struct{ cause backend.StartCause }
	type holdsResult struct{ res backend.CloseResult }

	renders := map[string]string{
		"cause in a struct, %v":  fmt.Sprintf("%v", holdsCause{cause: cause}),
		"cause in a struct, %+v": fmt.Sprintf("%+v", holdsCause{cause: cause}),
		"cause in a struct, %#v": fmt.Sprintf("%#v", holdsCause{cause: cause}),
		"result in a struct, %+v": fmt.Sprintf("%+v",
			holdsResult{res: backend.NewCloseFailed(cause)}),
		"result in a struct, %#v": fmt.Sprintf("%#v",
			holdsResult{res: backend.NewCloseFailed(cause)}),
	}

	for name, rendered := range renders {
		if rendered == "" {
			t.Errorf("%s rendered nothing; this test would pass vacuously", name)
			continue
		}
		for _, leak := range []string{"/private/tmp/fc-a1b2/sock", "secret-thing", "connect failed"} {
			if strings.Contains(rendered, leak) {
				t.Errorf("%s leaked %q: %s", name, leak, rendered)
			}
		}
	}

	// The control: the same error prints in full when it is not behind a
	// cause, so the absences above are the containment working rather than the
	// fixture being empty.
	if !strings.Contains(fmt.Sprintf("%+v", struct{ e error }{e: chattyError{Stderr: managerChatter}}), "secret-thing") {
		t.Fatal("the fixture error does not leak on its own; this test cannot prove containment")
	}
}

// TestStartCause_ComparingUncomparableCausesDoesNotPanic covers the other half
// of why the original is boxed. errors.Is compares a comparable target with
// ==; two causes wrapping the same uncomparable dynamic error type would panic
// there, and a panic on an error path is the worst place for one.
func TestStartCause_ComparingUncomparableCausesDoesNotPanic(t *testing.T) {
	inner := sliceError{Frames: []string{"a", "b"}}
	left := backend.NewStartCause(backend.FailureInternal, inner)
	right := backend.NewStartCause(backend.FailureInternal, sliceError{Frames: []string{"a", "b"}})

	// Distinct causes: must be unequal and must not panic.
	if errors.Is(&backend.LaunchError{Cause: left}, right) {
		t.Error("two independently constructed causes compared equal")
	}

	// The same cause: identity comparison must still work, or errors.Is
	// against a specific cause becomes useless.
	if !errors.Is(&backend.LaunchError{Cause: left}, left) {
		t.Error("a launch error does not match its own cause")
	}

	// And classification still reaches the original.
	sentinel := errors.New("sentinel")
	wrapped := backend.NewStartCause(backend.FailureTimeout, fmt.Errorf("dial: %w", sentinel))
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is cannot classify through the boxed cause")
	}
}

// TestLaunchError_OmitsAnUnrecordedCleanup keeps a dangling clause out of the
// operator's message. Before rollback runs, Cleanup is the zero value, and
// appending its name produced "; unspecified" — which reads as a cleanup
// verdict rather than the absence of one.
func TestLaunchError_OmitsAnUnrecordedCleanup(t *testing.T) {
	err := &backend.LaunchError{
		Phase:    backend.PhaseCreate,
		Backend:  backend.KindTmux,
		Mutation: backend.RefKnown,
		Cause:    backend.NewStartCause(backend.FailureTimeout, nil),
	}

	msg := err.Error()
	if strings.Contains(msg, "unspecified") {
		t.Errorf("message carries an unrecorded cleanup: %q", msg)
	}
	if !strings.Contains(msg, "timeout") {
		t.Errorf("message dropped the cause: %q", msg)
	}

	// The control: a recorded outcome still appears, so the check above is not
	// simply suppressing the clause entirely.
	err.Cleanup = backend.CleanupClosed
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("a recorded cleanup outcome is missing: %q", err.Error())
	}
}

// TestStartSpec_RendersRedactedUnderEveryVerb closes the leak a LogValue alone
// leaves open. slog consults LogValue; fmt does not, so an adapter writing
// fmt.Errorf("start %v: %w", spec, err) — the shape anyone reaches for — would
// print the target directory and the repository-derived label in the clear.
func TestStartSpec_RendersRedactedUnderEveryVerb(t *testing.T) {
	tag := recoveryTag(t)
	spec, err := backend.NewStartSpec(
		"/Users/someone/Projects/secret-thing", "secret-thing (main)", tag, bootstrap(t))
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}

	buf := &bytes.Buffer{}
	slog.New(slog.NewTextHandler(buf, nil)).Info("starting", "spec", spec)

	marshaled, marshalErr := json.Marshal(spec)
	if marshalErr != nil {
		t.Fatalf("Marshal: %v", marshalErr)
	}

	type holder struct{ spec backend.StartSpec }

	renders := map[string]string{
		"String()":            spec.String(),
		"%v":                  fmt.Sprintf("%v", spec),
		"%+v":                 fmt.Sprintf("%+v", spec),
		"%#v":                 fmt.Sprintf("%#v", spec),
		"%s":                  fmt.Sprintf("%s", spec), //nolint:staticcheck // exercising the verb is the assertion
		"%q":                  fmt.Sprintf("%q", spec),
		"wrapped in an error": fmt.Errorf("start %v: boom", spec).Error(),
		"in a struct, %+v":    fmt.Sprintf("%+v", holder{spec: spec}),
		"slog":                buf.String(),
		"json":                string(marshaled),
	}

	for name, rendered := range renders {
		if rendered == "" {
			t.Errorf("%s rendered nothing; this test would pass vacuously", name)
			continue
		}
		if !strings.Contains(rendered, tag.String()) {
			t.Errorf("%s = %q, which omits the recovery tag an operator needs", name, rendered)
		}
		for _, leak := range []string{"secret-thing", "/Users/someone", "beefcafe"} {
			if strings.Contains(rendered, leak) {
				t.Errorf("%s leaked %q: %s", name, leak, rendered)
			}
		}
	}
}

// TestNewBootstrapCommand_RefusesAnEmptyPayload closes the hole where
// exec.Opaque("") satisfies every gate. exec.Arg reports Secret from its kind
// alone, so an empty opaque argument is opaque — and would produce a workspace
// the manager is asked to type nothing into, with Set reporting a bootstrap
// was supplied.
func TestNewBootstrapCommand_RefusesAnEmptyPayload(t *testing.T) {
	if _, err := backend.NewBootstrapCommand(fcexec.Opaque("")); !errors.Is(err, backend.ErrBootstrapEmpty) {
		t.Errorf("an empty bootstrap was accepted: err = %v", err)
	}

	// A single character is enough to be a payload, so the check is emptiness
	// rather than a length heuristic.
	if _, err := backend.NewBootstrapCommand(fcexec.Opaque("x")); err != nil {
		t.Errorf("a non-empty bootstrap was refused: %v", err)
	}

	// And an empty one must not reach a spec.
	_, err := backend.NewStartSpec(
		"/Users/someone/Projects/thing", "thing", recoveryTag(t), backend.BootstrapCommand{})
	if !errors.Is(err, backend.ErrInvalidStartSpec) {
		t.Errorf("a spec with no bootstrap was built: %v", err)
	}
}

// TestRef_OwnershipNameIsTheCloseTimeComparand pins the accessor the Closer
// contract names. For cmux and herdr it is the only thing standing between a
// reference and somebody else's workspace, because their IDs are
// server-assigned and cannot carry our tag.
func TestRef_OwnershipNameIsTheCloseTimeComparand(t *testing.T) {
	tmux, tag := tmuxRef(t)
	if got := tmux.OwnershipName(); got != tag.OwnershipName() {
		t.Errorf("OwnershipName() = %q, want %q", got, tag.OwnershipName())
	}

	// The same accessor exists on the two backends whose identities cannot
	// carry the tag — which is where it actually earns its place.
	for _, ref := range []backend.Ref{cmuxRef(t), herdrRef(t)} {
		if got := ref.OwnershipName(); got != ref.Tag().OwnershipName() {
			t.Errorf("%s OwnershipName() = %q, want %q", ref.Kind(), got, ref.Tag().OwnershipName())
		}
		if !strings.HasPrefix(ref.OwnershipName(), "fc-surface-") {
			t.Errorf("%s ownership name %q is outside forgectl's namespace", ref.Kind(), ref.OwnershipName())
		}
	}
}

// TestFingerprint_IncludesTheServerReportedToken covers the input that exists
// for backends whose endpoint is a config path rather than a socket. For those
// the filesystem evidence does not turn over on a restart, so this token is
// the only thing that can.
func TestFingerprint_IncludesTheServerReportedToken(t *testing.T) {
	base := backend.IncarnationInput{
		Endpoint: "/Users/someone/.config/herdr/config.toml",
		Version:  "herdr 0.8.0 protocol 19",
		Device:   16777232,
		Inode:    4242,
	}

	before, err := backend.Fingerprint(base)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	restarted := base
	restarted.ServerReported = "boot-2"
	after, err := backend.Fingerprint(restarted)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if before.Matches(after) {
		t.Error("the server-reported token does not affect the fingerprint")
	}

	// Two different tokens on otherwise identical evidence must also differ,
	// or the field is only distinguishing present-from-absent.
	other := restarted
	other.ServerReported = "boot-3"
	third, err := backend.Fingerprint(other)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if after.Matches(third) {
		t.Error("two different server-reported tokens fingerprinted identically")
	}
}
