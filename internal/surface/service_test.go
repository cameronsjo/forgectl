package surface_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/fake"
)

// The service is the half that can leave something behind, so these tests are
// mostly about rollback: which failures close the surface, which must not, and
// which are honest about not knowing.
//
// The commit path needs the socket and the nonce, which nothing outside this
// package can reach — that is the point of the design — so it lives in the
// in-package test file beside this one.

func serviceFixture(t *testing.T) (*fake.Adapter, *surface.Service) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("the surface service is Unix-only; this is %s", runtime.GOOS)
	}

	// macOS caps a socket path near 104 bytes, and the default temp base plus a
	// run directory is most of that already.
	base, err := os.MkdirTemp("", "sv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	adapter := fake.New(backend.KindTmux)
	return adapter, surface.NewService(adapter, surface.Policy{}, base)
}

// launchRequest builds a request whose harness passes the admission policy.
func launchRequest(t *testing.T) surface.LaunchRequest {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	req := surface.NewLaunchRequest("thing", launch.Invocation{
		Harness: "claude",
		Binary: launch.ResolvedBinary{
			Path:   "/bin/sh",
			Source: launch.BinaryClaudeConfig,
		},
		Args: []string{"--model", "opus"},
		Env:  []string{"PATH=/usr/bin"},
		CWD:  t.TempDir(),
	})
	req.Self = self
	return req
}

// withInvocation rebuilds a request with a modified invocation, since the
// invocation is not an exported field.
func withInvocation(req surface.LaunchRequest, mutate func(*launch.Invocation)) surface.LaunchRequest {
	inv, _ := req.Invocation()
	mutate(&inv)
	next := surface.NewLaunchRequest(req.Name, inv)
	next.Self = req.Self
	return next
}

// tmuxRef builds a ref the fake adapter can return.
func tmuxRef(t *testing.T, tag backend.RecoveryTag) backend.Ref {
	t.Helper()
	source := backend.TmuxDefaultServer()
	server, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/tmp/tmux-501/default",
		Version:  "3.5a",
		Inode:    42,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := backend.NewTmuxIdentity(tag.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := backend.NewTmuxRef(source, server, tag, identity)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestService_ClosesWhatItCreatedWhenTheHandshakeFails is the rollback path.
// A surface that exists but never received an invocation is ours to remove.
func TestService_ClosesWhatItCreatedWhenTheHandshakeFails(t *testing.T) {
	adapter, service := serviceFixture(t)

	var ref backend.Ref
	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		ref = tmuxRef(t, spec.Tag())
		// No trampoline: the manager was asked and nothing ever connected.
		return backend.NewRefKnown(ref)
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		return backend.NewCloseClosed()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := service.Launch(ctx, launchRequest(t))
	if err == nil {
		t.Fatal("a launch whose trampoline never appeared reported success")
	}
	if !errors.Is(err, surface.ErrLaunch) {
		t.Errorf("err = %v, want ErrLaunch", err)
	}

	closes := adapter.Closes()
	if len(closes) != 1 {
		t.Fatalf("the surface was closed %d time(s), want exactly 1", len(closes))
	}
	if closes[0] != ref {
		t.Error("cleanup closed a different surface than the one that was created")
	}
}

// TestService_NeverGuessesAtAnUnknownOutcome is the ambiguity rule. An adapter
// that cannot say whether it created anything must not be asked to close
// something, because the close would be a guess about which container.
func TestService_NeverGuessesAtAnUnknownOutcome(t *testing.T) {
	adapter, service := serviceFixture(t)

	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		return backend.NewOutcomeUnknown(spec.Tag(),
			backend.NewStartCause(backend.FailureTimeout, errors.New("daemon stopped answering")))
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		t.Error("cleanup ran against an outcome the adapter could not determine")
		return backend.NewCloseClosed()
	}

	_, err := service.Launch(context.Background(), launchRequest(t))
	if err == nil {
		t.Fatal("an unknown outcome reported success")
	}

	var launchErr *surface.LaunchError
	if !errors.As(err, &launchErr) {
		t.Fatalf("err = %T, want *surface.LaunchError", err)
	}
	if launchErr.Outcome != backend.OutcomeUnknown {
		t.Errorf("outcome = %v, want OutcomeUnknown", launchErr.Outcome)
	}
	// The operator needs the tag: it is the only handle on a container that may
	// or may not exist.
	if launchErr.Recovery == "" {
		t.Error("an unknown outcome reported no recovery tag; the operator has nothing to search for")
	}
	if len(adapter.Closes()) != 0 {
		t.Error("cleanup was attempted against an unknown outcome")
	}
}

// TestService_DoesNotCloseWhatItNeverCreated keeps a definitive failure from
// producing a cleanup call against a surface that does not exist.
func TestService_DoesNotCloseWhatItNeverCreated(t *testing.T) {
	adapter, service := serviceFixture(t)

	adapter.StartFunc = func(context.Context, backend.StartSpec) backend.StartResult {
		return backend.NewNotMutated(
			backend.NewStartCause(backend.FailureUnavailable, errors.New("no tmux server")))
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		t.Error("cleanup ran for a launch that never mutated anything")
		return backend.NewCloseClosed()
	}

	_, err := service.Launch(context.Background(), launchRequest(t))
	if err == nil {
		t.Fatal("a failed create reported success")
	}
	var launchErr *surface.LaunchError
	if !errors.As(err, &launchErr) {
		t.Fatalf("err = %T, want *surface.LaunchError", err)
	}
	if launchErr.Outcome != backend.NotMutated {
		t.Errorf("outcome = %v, want NotMutated", launchErr.Outcome)
	}
	if len(adapter.Closes()) != 0 {
		t.Error("cleanup was attempted for a launch that created nothing")
	}
	// NotMutated is the zero value, so this test passes against a Launch that
	// never consulted the adapter at all. The subject is the adapter's report,
	// so the adapter has to have reported.
	if len(adapter.Starts()) != 1 {
		t.Fatalf("the adapter was called %d time(s), want exactly 1", len(adapter.Starts()))
	}
}

// TestService_TreatsAnUnvalidatableResultAsUnknown is the one that must not be
// got wrong.
//
// NotMutated is the zero MutationOutcome, so reading Outcome() off a result
// that failed Validate turns a malformed adapter reply into the *positive*
// claim that nothing was created — and an operator told "not-mutated" has no
// reason to look for a container. There is no "an error came back so presumably
// nothing happened".
func TestService_TreatsAnUnvalidatableResultAsUnknown(t *testing.T) {
	adapter, service := serviceFixture(t)

	adapter.StartFunc = func(context.Context, backend.StartSpec) backend.StartResult {
		// The zero value: no outcome, no ref, no cause. What a half-built
		// adapter returns.
		return backend.StartResult{}
	}

	_, err := service.Launch(context.Background(), launchRequest(t))
	if err == nil {
		t.Fatal("an unvalidatable result reported success")
	}

	var launchErr *surface.LaunchError
	if !errors.As(err, &launchErr) {
		t.Fatalf("err = %T, want *surface.LaunchError", err)
	}
	if launchErr.Outcome == backend.NotMutated {
		t.Error("an unvalidatable result was reported as not-mutated — a positive claim " +
			"that nothing was created, from a reply that said nothing at all")
	}
	if launchErr.Outcome != backend.OutcomeUnknown {
		t.Errorf("outcome = %v, want OutcomeUnknown", launchErr.Outcome)
	}
}

// TestLaunchError_RendersItsCause keeps the operator-facing string useful.
//
// Without the cause every setup failure is byte-identical, and the CLI prints
// this string — so a socket path over the platform limit, which carries an
// actionable remedy, would arrive as the same nine words as a refused binary.
func TestLaunchError_RendersItsCause(t *testing.T) {
	adapter, service := serviceFixture(t)
	adapter.StartFunc = func(context.Context, backend.StartSpec) backend.StartResult {
		t.Error("the adapter ran for a request that should have failed validation")
		return backend.StartResult{}
	}

	req := withInvocation(launchRequest(t), func(inv *launch.Invocation) { inv.CWD = "relative/path" })

	_, err := service.Launch(context.Background(), req)
	if err == nil {
		t.Fatal("a relative working directory was accepted")
	}

	// Two different setup failures must not render identically.
	other := withInvocation(launchRequest(t), func(inv *launch.Invocation) { inv.Binary.Source = launch.BinaryPATH })
	_, otherErr := service.Launch(context.Background(), other)
	if otherErr == nil {
		t.Fatal("a PATH binary was accepted")
	}
	if err.Error() == otherErr.Error() {
		t.Errorf("two unrelated setup failures render identically: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the error does not name its cause: %q", err.Error())
	}
}

// TestLaunchRequest_NeverRendersTheInvocation guards the type the CLI holds.
//
// The downstream StartSpec is already opaque and tested; this is the upstream
// half. %+v on a plain struct walks into launch.Invocation and prints the argv
// and the environment, which is the copy this package exists to prevent.
func TestLaunchRequest_NeverRendersTheInvocation(t *testing.T) {
	req := withInvocation(launchRequest(t), func(inv *launch.Invocation) {
		inv.Args = []string{"--prompt", "SENTINEL-request-arg"}
		inv.Env = []string{"TOKEN=SENTINEL-request-env"}
	})

	rendered := []string{
		req.String(),
		req.GoString(),
		fmt.Sprintf("%v", req),
		fmt.Sprintf("%+v", req),
		fmt.Sprintf("%#v", req),
		fmt.Sprintf("%q", req),
		fmt.Sprintf("%v", &req),
		fmt.Sprintf("%v", struct{ R surface.LaunchRequest }{req}),
		fmt.Sprintf("%+v", struct{ r surface.LaunchRequest }{req}), //nolint:govet // reaching an unexported field is the case under test
	}
	if payload, err := req.MarshalJSON(); err == nil {
		rendered = append(rendered, string(payload))
	}
	if payload, err := req.MarshalText(); err == nil {
		rendered = append(rendered, string(payload))
	}

	for _, s := range rendered {
		for _, sentinel := range []string{"SENTINEL-request-arg", "SENTINEL-request-env"} {
			if strings.Contains(s, sentinel) {
				t.Errorf("a rendered LaunchRequest leaked the invocation: %s", s)
			}
		}
	}
}

// TestService_ReportsAFailedCloseAsFailed is lens-3's question: can a rollback
// that did not work read as one that did?
//
// Reporting CleanupClosed unconditionally instead of mapping the adapter's
// answer survives every other test here — and it is the difference between an
// operator who knows a container is still out there and one who does not.
func TestService_ReportsAFailedCloseAsFailed(t *testing.T) {
	cases := map[string]struct {
		close     func() backend.CloseResult
		satisfied bool
	}{
		"closed": {
			close:     backend.NewCloseClosed,
			satisfied: true,
		},
		"already gone": {
			close:     backend.NewCloseAlreadyGone,
			satisfied: true,
		},
		"failed": {
			close: func() backend.CloseResult {
				return backend.NewCloseFailed(
					backend.NewStartCause(backend.FailureTimeout, errors.New("daemon hung")))
			},
			satisfied: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			adapter, service := serviceFixture(t)
			adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
				return backend.NewRefKnown(tmuxRef(t, spec.Tag()))
			}
			adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
				return tc.close()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := service.Launch(ctx, launchRequest(t))
			var launchErr *surface.LaunchError
			if !errors.As(err, &launchErr) {
				t.Fatalf("err = %T, want *surface.LaunchError", err)
			}
			if got := launchErr.Cleanup.Satisfied(); got != tc.satisfied {
				t.Errorf("cleanup %v reports Satisfied()=%v, want %v",
					launchErr.Cleanup, got, tc.satisfied)
			}
			// A rollback that left something behind must name it; one that
			// succeeded must not send the operator hunting.
			if tc.satisfied && launchErr.Recovery != "" {
				t.Error("a clean rollback still reported a recovery tag")
			}
			if !tc.satisfied && launchErr.Recovery == "" {
				t.Error("a failed rollback reported no recovery tag; the operator has " +
					"nothing to search for")
			}
		})
	}
}

// TestService_RollsBackEvenWhenTheCallerCancelled is why cleanup gets its own
// context.
//
// The caller's may be the reason we are rolling back at all, so inheriting the
// cancellation would skip exactly the work the cancellation created — leaving a
// container behind every time someone pressed Ctrl-C.
func TestService_RollsBackEvenWhenTheCallerCancelled(t *testing.T) {
	adapter, service := serviceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var created backend.Ref
	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		created = tmuxRef(t, spec.Tag())
		// The operator gives up while the manager is being asked.
		cancel()
		return backend.NewRefKnown(created)
	}

	closed := make(chan backend.Ref, 1)
	adapter.CloseFunc = func(cleanupCtx context.Context, ref backend.Ref) backend.CloseResult {
		if err := cleanupCtx.Err(); err != nil {
			t.Errorf("cleanup ran under a cancelled context (%v); it would be skipped "+
				"exactly when it is needed", err)
		}
		closed <- ref
		return backend.NewCloseClosed()
	}

	if _, err := service.Launch(ctx, launchRequest(t)); err == nil {
		t.Fatal("a cancelled launch reported success")
	}

	select {
	case ref := <-closed:
		if ref != created {
			t.Error("cleanup closed a different surface than the one created")
		}
	default:
		t.Fatal("a cancelled launch left its surface behind")
	}
}

// TestService_RollsBackACreatedThenFailedSurface covers RefKnownWithCause: the
// adapter created something and then failed. The surface exists, so it is ours
// to remove, and the failure belongs to the start phase rather than to the
// handshake that never happened.
func TestService_RollsBackACreatedThenFailedSurface(t *testing.T) {
	adapter, service := serviceFixture(t)

	var created backend.Ref
	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		created = tmuxRef(t, spec.Tag())
		return backend.NewRefKnownWithCause(created,
			backend.NewStartCause(backend.FailureTimeout, errors.New("created, then lost the daemon")))
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		return backend.NewCloseClosed()
	}

	_, err := service.Launch(context.Background(), launchRequest(t))
	var launchErr *surface.LaunchError
	if !errors.As(err, &launchErr) {
		t.Fatalf("err = %T, want *surface.LaunchError", err)
	}
	if launchErr.Phase != surface.PhaseStart {
		t.Errorf("phase = %v, want PhaseStart — the failure was the adapter's, not the "+
			"handshake's", launchErr.Phase)
	}
	closes := adapter.Closes()
	if len(closes) != 1 || closes[0] != created {
		t.Errorf("a created-then-failed surface was not rolled back: %d close(s)", len(closes))
	}
}

// TestService_RefusesARelativeSelf keeps the re-entry path absolute.
//
// This word becomes argv[0] of a command a terminal manager types into a shell,
// so a relative value resolves against that shell rather than this process —
// and it silently disarms the self-loop guard, which stats the path and, by
// design, warns-and-admits when it cannot.
func TestService_RefusesARelativeSelf(t *testing.T) {
	adapter, service := serviceFixture(t)
	adapter.StartFunc = func(context.Context, backend.StartSpec) backend.StartResult {
		t.Error("the adapter ran for a request with a relative self path")
		return backend.StartResult{}
	}

	req := launchRequest(t)
	req.Self = "forgectl"

	if _, err := service.Launch(context.Background(), req); err == nil {
		t.Fatal("a relative forgectl path was accepted")
	}
	if len(adapter.Starts()) != 0 {
		t.Error("the adapter created a surface before the path was validated")
	}
}

// TestService_RefusesAnAdapterThatCannotRollBack is the preflight. Discovering
// after the create that nothing can undo it is discovering it too late.
func TestService_RefusesAnAdapterThatCannotRollBack(t *testing.T) {
	base, err := os.MkdirTemp("", "sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	service := surface.NewService(fake.NewStartOnly(backend.KindTmux), surface.Policy{}, base)

	_, err = service.Launch(context.Background(), launchRequest(t))
	if !errors.Is(err, surface.ErrCapabilities) {
		t.Errorf("err = %v, want ErrCapabilities", err)
	}
}

// TestService_RefusesABinaryThePolicyRejects proves the admission check runs
// before anything is created, not after.
func TestService_RefusesABinaryThePolicyRejects(t *testing.T) {
	adapter, service := serviceFixture(t)
	adapter.StartFunc = func(context.Context, backend.StartSpec) backend.StartResult {
		t.Error("the adapter was called for a binary the policy refuses")
		return backend.StartResult{}
	}

	req := withInvocation(launchRequest(t), func(inv *launch.Invocation) { inv.Binary.Source = launch.BinaryPATH })

	if _, err := service.Launch(context.Background(), req); err == nil {
		t.Fatal("a PATH-resolved binary was accepted without --allow-path-binary")
	}
	if len(adapter.Starts()) != 0 {
		t.Error("the adapter created a surface before the policy check")
	}
}

// TestService_TheStartSpecNeverRendersTheInvocation is the privacy property at
// the seam where it is easiest to lose.
//
// An adapter is the component that talks to a terminal manager, and the design
// assumes it cannot learn the invocation. That assumption holds only if the
// spec it is handed refuses to render it under every verb a logger might use —
// and %+v on a struct reaches unexported fields by reflection.
func TestService_TheStartSpecNeverRendersTheInvocation(t *testing.T) {
	adapter, service := serviceFixture(t)

	req := launchRequest(t)
	req = withInvocation(req, func(inv *launch.Invocation) {
		inv.Args = []string{"--prompt", "SENTINEL-launch-arg"}
		inv.Env = []string{"TOKEN=SENTINEL-launch-env"}
	})

	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		for _, rendered := range []string{
			spec.String(),
			spec.GoString(),
			fmt.Sprintf("%v", spec),
			fmt.Sprintf("%+v", spec),
			fmt.Sprintf("%#v", spec),
			fmt.Sprintf("%v", spec.Bootstrap()),
			fmt.Sprintf("%+v", spec.Bootstrap()),
			fmt.Sprintf("%v", struct{ S backend.StartSpec }{spec}),
			fmt.Sprintf("%+v", struct{ s backend.StartSpec }{spec}), //nolint:govet // reaching an unexported field is the case under test
		} {
			for _, sentinel := range []string{"SENTINEL-launch-arg", "SENTINEL-launch-env"} {
				if strings.Contains(rendered, sentinel) {
					t.Errorf("the start spec rendered the invocation: %s", rendered)
				}
			}
		}
		return backend.NewNotMutated(
			backend.NewStartCause(backend.FailureUnavailable, errors.New("stop here")))
	}

	_, _ = service.Launch(context.Background(), req)
	if len(adapter.Starts()) != 1 {
		t.Fatal("the adapter was never called, so nothing was inspected")
	}
}
