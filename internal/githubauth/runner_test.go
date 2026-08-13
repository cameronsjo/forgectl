package githubauth

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// helperProcessEnv gates the re-exec helper below: the test binary re-runs
// itself with this set, and the helper test body exits nonzero only then.
const helperProcessEnv = "FORGECTL_GITHUBAUTH_HELPER_PROCESS"

// TestHelperProcessExitsNonzero is not a test. It is the child half of
// realExitError: re-executed with helperProcessEnv set, it exits 3 so the
// parent gets a genuine *os/exec.ExitError to feed through the wrapper.
func TestHelperProcessExitsNonzero(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		t.Skip("helper process; runs only under realExitError")
	}
	os.Exit(3)
}

// realExitError produces a genuine *os/exec.ExitError deterministically, by
// running the test binary in helper-process mode. Its point is what it does
// NOT carry: no context identity at all. Feeding it through the wrapper is
// what proves the ctx.Err()-first ordering, since errors.Is on this error can
// never match a context sentinel by accident.
func realExitError(t *testing.T) error {
	t.Helper()
	cmd := osexec.Command(os.Args[0], "-test.run=TestHelperProcessExitsNonzero")
	cmd.Env = append(os.Environ(), helperProcessEnv+"=1")
	err := cmd.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper process: want *exec.ExitError, got %T (%v)", err, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper exit error unexpectedly carries context identity: %v", err)
	}
	return err
}

// hookFake is a Runner double that runs a hook just before returning, so a
// test can cancel or expire the caller's context at the exact moment the
// subprocess "fails". exec.FakeRunner cannot do this: it ignores ctx entirely.
type hookFake struct {
	hook   func(context.Context)
	out    string
	err    error
	gotEnv map[string]string
	calls  int
}

func (f *hookFake) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	f.calls++
	if f.hook != nil {
		f.hook(ctx)
	}
	return f.out, f.err
}

func (f *hookFake) RunWithEnv(ctx context.Context, env map[string]string, _ string, _ ...string) (string, error) {
	f.calls++
	f.gotEnv = env
	if f.hook != nil {
		f.hook(ctx)
	}
	return f.out, f.err
}

func (f *hookFake) RunWithInput(ctx context.Context, _ string, name string, args ...string) (string, error) {
	return f.Run(ctx, name, args...)
}

func (f *hookFake) RunInteractive(_ context.Context, _ string, _ ...string) error { return nil }

func TestRunner_PinsGhToGitHubComDespiteAmbientHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{}

	if _, err := Runner(fake).Run(t.Context(), "gh", "api", "user"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	last := fake.Last()
	if last.Name != "gh" {
		t.Fatalf("command name = %q, want gh", last.Name)
	}
	if got := last.Env["GH_HOST"]; got != Host {
		t.Fatalf("GH_HOST = %q, want %q", got, Host)
	}
}

func TestRunner_DelegatesNonGhUntouched(t *testing.T) {
	fake := &exec.FakeRunner{}

	if _, err := Runner(fake).Run(t.Context(), "git", "status"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	last := fake.Last()
	if last.Name != "git" {
		t.Fatalf("command name = %q, want git", last.Name)
	}
	// FakeRunner records Env only on RunWithEnv, so a nil Env proves the call
	// took the plain Run delegation path rather than the pinned one.
	if last.Env != nil {
		t.Fatalf("non-gh call carried env %v, want plain Run delegation", last.Env)
	}
}

func TestRunner_DoubleWrapIsHarmless(t *testing.T) {
	fake := &exec.FakeRunner{}

	if _, err := Runner(Runner(fake)).Run(t.Context(), "gh", "api", "user"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("underlying calls = %d, want 1", len(fake.Calls))
	}
	if got := fake.Last().Env["GH_HOST"]; got != Host {
		t.Fatalf("GH_HOST = %q, want %q", got, Host)
	}
}

func TestRunner_CancelledContextBeatsDisagreeingRawError(t *testing.T) {
	raw := realExitError(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &hookFake{err: raw, out: "partial", hook: func(context.Context) { cancel() }}

	out, err := Runner(fake).Run(ctx, "gh", "api", "user")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("err %q leaked the raw exit text", err)
	}
	if out != "partial" {
		t.Fatalf("stdout = %q, want it retained across classification", out)
	}
}

func TestRunner_ExpiredDeadlineBeatsDisagreeingRawError(t *testing.T) {
	raw := realExitError(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	fake := &hookFake{err: raw, hook: func(ctx context.Context) { <-ctx.Done() }}

	_, err := Runner(fake).Run(ctx, "gh", "api", "user")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("err %q leaked the raw exit text", err)
	}
}

func TestRunner_LiveContextNormalizesRawContextCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  error
		want error
	}{
		{"wrapped deadline", errors.Join(errors.New("gh failed"), context.DeadlineExceeded), context.DeadlineExceeded},
		{"wrapped canceled", errors.Join(errors.New("gh failed"), context.Canceled), context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &hookFake{err: tc.raw}

			_, err := Runner(fake).Run(t.Context(), "gh", "api", "user")

			if err != tc.want {
				t.Fatalf("err = %v (%T), want the bare sentinel %v", err, err, tc.want)
			}
		})
	}
}

func TestRunner_OrdinaryFailurePassesThroughUnchanged(t *testing.T) {
	raw := errors.New("gh: HTTP 401")
	fake := &hookFake{err: raw}

	_, err := Runner(fake).Run(t.Context(), "gh", "api", "user")

	if !errors.Is(err, raw) {
		t.Fatalf("err = %v, want the raw error preserved for categorical classification", err)
	}
}

func TestSafeContextSentinel(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"canceled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},
		{"ordinary", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeContextSentinel(tc.err); got != tc.want {
				t.Fatalf("SafeContextSentinel(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
