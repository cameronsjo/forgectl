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
	return surface.LaunchRequest{
		Name: "thing",
		Invocation: launch.Invocation{
			Harness: "claude",
			Binary: launch.ResolvedBinary{
				Path:   "/bin/sh",
				Source: launch.BinaryClaudeConfig,
			},
			Args: []string{"--model", "opus"},
			Env:  []string{"PATH=/usr/bin"},
			CWD:  t.TempDir(),
		},
		Self: self,
	}
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

	req := launchRequest(t)
	req.Invocation.Binary.Source = launch.BinaryPATH // refused without the opt-in

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
	req.Invocation.Args = []string{"--prompt", "SENTINEL-launch-arg"}
	req.Invocation.Env = []string{"TOKEN=SENTINEL-launch-env"}

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
