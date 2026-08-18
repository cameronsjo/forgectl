package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"

	"github.com/spf13/cobra"
)

// This file holds the entry-ordering proof: `surface _exec` is claimed before
// ANY of forgectl's normal startup runs.
//
// Ordering is the whole security property. Normal startup captures the
// environment, prepares the legacy migration boundary, sets up the process
// logger, and builds the module registry — and, before this change, logged raw
// argv. For a bootstrap invocation, raw argv is a socket path and a 256-bit
// rendezvous nonce. A classifier that ran one statement too late would leak
// them into the log and still pass every test that only checks its parsing.
//
// So the assertion is not "the classifier is first in the source"; it is that
// each startup dependency, replaced with a sentinel that fails the test when
// called, is never called. Reading the source proves the order today. This
// proves it after the next edit.

// swapStartupSentinels replaces every startup seam with a fail-if-called stub
// and restores the originals when the test ends. It returns nothing: any call
// is a test failure, so there is no counter worth reading.
func swapStartupSentinels(t *testing.T) {
	t.Helper()

	prevEnv, prevBoundary, prevLogger, prevRoot := captureEnvSnapshot, prepareLegacyBoundary, setupLogger, buildRoot
	t.Cleanup(func() {
		captureEnvSnapshot, prepareLegacyBoundary, setupLogger, buildRoot = prevEnv, prevBoundary, prevLogger, prevRoot
	})

	captureEnvSnapshot = func() (config.EnvSnapshot, error) {
		t.Error("config.CaptureEnvSnapshot ran during a bootstrap invocation")
		return config.EnvSnapshot{}, nil
	}
	prepareLegacyBoundary = func(config.EnvSnapshot, config.MigrationFS) (*config.LegacyMigrationBoundary, error) {
		t.Error("config.PrepareLegacyMigrationBoundary ran during a bootstrap invocation")
		return nil, errors.New("sentinel")
	}
	setupLogger = func(config.Config) io.Closer {
		t.Error("config.SetupLogger ran during a bootstrap invocation")
		return io.NopCloser(nil)
	}
	buildRoot = func(module.Deps) *cobra.Command {
		t.Error("the module registry was built during a bootstrap invocation")
		return &cobra.Command{}
	}
}

// withArgs points os.Args at argv for the duration of the test. Execute reads
// process argv directly, which is the thing under test — a helper that fed
// argv in by parameter would be testing a different function.
func withArgs(t *testing.T, argv ...string) {
	t.Helper()
	prev := osArgs
	t.Cleanup(func() { osArgs = prev })
	osArgs = append([]string{"forgectl"}, argv...)
}

// TestExecute_ValidBootstrapTouchesNoStartupDependency is the plan's RED case
// for a well-formed candidate.
func TestExecute_ValidBootstrapTouchesNoStartupDependency(t *testing.T) {
	swapStartupSentinels(t)
	withArgs(t, validBootstrapArgs()...)

	err := Execute(context.Background())

	// The fixture's socket path does not exist, so the trampoline refuses at its
	// first check. That specific error is what proves the invocation reached the
	// runtime rather than stopping in the parser — the sentinels are the real
	// assertion, and this confirms the handoff happened at all.
	if !errors.Is(err, errSocketUnsafe) {
		t.Fatalf("Execute = %v, want errSocketUnsafe", err)
	}
}

// TestExecute_MalformedBootstrapTouchesNoStartupDependency is the half that
// actually matters. A well-formed bootstrap is forgectl talking to itself; a
// MALFORMED one is the hand-crafted case, and it is the one that must not fall
// through into startup and have its argv logged.
func TestExecute_MalformedBootstrapTouchesNoStartupDependency(t *testing.T) {
	malformed := [][]string{
		{"surface", "_exec"},
		{"surface", "_exec", "--protocol", "99", "--socket", validSocket, "--nonce", validNonce},
		{"surface", "--no-icons", "_exec", "--socket", validSocket, "--nonce", validNonce},
		append(validBootstrapArgs(), "extra"),
	}

	for _, argv := range malformed {
		t.Run(argv[len(argv)-1], func(t *testing.T) {
			swapStartupSentinels(t)
			withArgs(t, argv...)

			if err := Execute(context.Background()); err == nil {
				t.Fatal("Execute returned nil for a malformed bootstrap; it must refuse")
			}
		})
	}
}

// TestExecute_SentinelsFireForAnOrdinaryInvocation is the anti-vacuity control.
// Without it, the two tests above would pass identically if Execute returned
// early for every input, or if the sentinels were never wired to anything.
func TestExecute_SentinelsFireForAnOrdinaryInvocation(t *testing.T) {
	var envCalled, rootCalled bool

	prevEnv, prevRoot := captureEnvSnapshot, buildRoot
	t.Cleanup(func() { captureEnvSnapshot, buildRoot = prevEnv, prevRoot })

	captureEnvSnapshot = func() (config.EnvSnapshot, error) {
		envCalled = true
		return config.EnvSnapshot{}, errors.New("stop here")
	}
	buildRoot = func(module.Deps) *cobra.Command {
		rootCalled = true
		return &cobra.Command{}
	}

	withArgs(t, "--version")

	_ = Execute(context.Background())

	if !envCalled {
		t.Error("an ordinary invocation did not reach config.CaptureEnvSnapshot; the seam is not wired")
	}
	if rootCalled {
		t.Error("buildRoot ran after CaptureEnvSnapshot failed; startup should have stopped")
	}
}
