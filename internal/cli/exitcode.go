// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package cli

import "errors"

// ExitCoder is satisfied by an error that wants to drive main's os.Exit with
// something other than the universal 1 — the same interface shape
// internal/bless's exitCodeOf reads back off a helper subprocess's
// *exec.ExitError, generalized here to the CLI's own RunE errors. A command
// wires a failure class to a code with WithExitCode; main resolves the
// final code with ExitCode. Errors that never opt in (the vast majority)
// keep exiting 1, unchanged.
type ExitCoder interface {
	ExitCode() int
}

// codedError pairs err with the process exit code it should produce.
// Unwrap keeps errors.Is/As working against the wrapped error, so wrapping
// a sentinel for its exit code never hides it from other error-chain checks.
type codedError struct {
	err  error
	code int
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

// WithExitCode wraps err so ExitCode(err) reports code instead of the
// default 1. A nil err stays nil, so it composes with a bare `return
// WithExitCode(err, N)` after an `if err != nil` guard.
func WithExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return &codedError{err: err, code: code}
}

// ExitCode walks err's chain for an ExitCoder (see WithExitCode) and returns
// its code, or 1 when err carries none — the default every command got
// before typed exit codes existed. main calls this once, on whatever
// Execute returns.
func ExitCode(err error) int {
	var coded ExitCoder
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}
