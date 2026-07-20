// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package cli

// Test plan for exitcode.go
//
// WithExitCode / ExitCode (Classification: reusable helper)
//   [x] Happy: an error with no opted-in code reports the default (1)
//   [x] Happy: WithExitCode's code round-trips through ExitCode
//   [x] Happy: WithExitCode(nil, N) stays nil (composes with a bare return)
//   [x] Happy: errors.Is/As still sees the wrapped sentinel through Unwrap
//   [x] Guard: a foreign error that satisfies interface{ ExitCode() int } but
//       was never opted in (e.g. a wrapped *exec.ExitError) stays at 1

import (
	"errors"
	"fmt"
	"testing"
)

// fakeExitError mimics *exec.ExitError: it satisfies interface{ ExitCode() int }
// but is not a codedError. A subprocess error like this must never leak its code
// as forgectl's own — only WithExitCode opts a command in.
type fakeExitError struct{ code int }

func (e *fakeExitError) Error() string { return "subprocess failed" }
func (e *fakeExitError) ExitCode() int { return e.code }

func TestExitCode_Default(t *testing.T) {
	if got := ExitCode(errors.New("plain failure")); got != 1 {
		t.Errorf("ExitCode(plain error) = %d, want 1 (default)", got)
	}
}

func TestExitCode_RoundTripsWithExitCode(t *testing.T) {
	err := WithExitCode(errors.New("drift"), 1)
	if got := ExitCode(err); got != 1 {
		t.Errorf("ExitCode(WithExitCode(_, 1)) = %d, want 1", got)
	}

	err2 := WithExitCode(errors.New("absent file"), 2)
	if got := ExitCode(err2); got != 2 {
		t.Errorf("ExitCode(WithExitCode(_, 2)) = %d, want 2", got)
	}
}

func TestWithExitCode_NilStaysNil(t *testing.T) {
	if err := WithExitCode(nil, 2); err != nil {
		t.Errorf("WithExitCode(nil, 2) = %v, want nil", err)
	}
}

func TestWithExitCode_UnwrapsToOriginal(t *testing.T) {
	sentinel := errors.New("sentinel")
	wrapped := WithExitCode(sentinel, 2)
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is(wrapped, sentinel) = false, want true — WithExitCode must not hide the original error from the chain")
	}
}

func TestExitCode_IgnoresForeignExitCoder(t *testing.T) {
	// A wrapped subprocess error (e.g. an editor exiting 7 under `launch edit`,
	// or a `docker build` child) satisfies interface{ ExitCode() int } but was
	// never opted in via WithExitCode — it must fall back to the default 1, not
	// leak the child's code as forgectl's own.
	wrapped := fmt.Errorf("launch edit: %w", &fakeExitError{code: 7})
	if got := ExitCode(wrapped); got != 1 {
		t.Errorf("ExitCode(wrapped foreign ExitCoder) = %d, want 1 (a non-opted-in subprocess code must not leak)", got)
	}
}
