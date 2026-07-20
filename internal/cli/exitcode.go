// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package cli

import "errors"

// codedError pairs err with the process exit code it should produce. A command
// wires a failure class to a code with WithExitCode; main resolves the final
// code with ExitCode. Errors that never opt in (the vast majority) keep
// exiting 1, unchanged.
//
// Unwrap keeps errors.Is/As working against the wrapped error, so wrapping a
// sentinel for its exit code never hides it from other error-chain checks.
//
// ExitCode matches this CONCRETE type, deliberately not a bare
// `interface{ ExitCode() int }`: stdlib's *exec.ExitError satisfies that shape,
// so an interface match would leak a subprocess's exit code (an editor, a
// `docker build` child) as forgectl's own for any command that wraps such an
// error with %w — commands that never opted in. Only WithExitCode mints a
// codedError, so gating on it keeps the opt-in explicit.
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

// ExitCode walks err's chain for a codedError (see WithExitCode) and returns
// its code, or 1 when err carries none — the default every command got before
// typed exit codes existed, and what every command that never opts in still
// gets. main calls this once, on whatever Execute returns.
func ExitCode(err error) int {
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}
