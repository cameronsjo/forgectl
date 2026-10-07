// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"log/slog"
)

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
	if chainAs(err, &coded) {
		return coded.ExitCode()
	}
	// silentCodedError (execute.go) opts in to a typed exit code the same
	// way, but is a distinct concrete type so termsafeErrorHandler can
	// pattern-match it separately to render nothing.
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return silent.ExitCode()
	}
	return 1
}

// chainAs is errors.As that treats a panic during the walk as "not found".
// A chain can hold a typed-nil error whose Unwrap dereferences its receiver —
// fmt.Errorf("…: %w", (*os.PathError)(nil)) formats fine (fmt recovers inside
// Error) but errors.As then calls (*os.PathError)(nil).Unwrap and panics.
// Resolving an exit code must never crash the process on its way out.
//
// The recovery leaves a Debug trace naming the error's and the panic value's
// Go types, as termsafe's errorText does, so a real bug in an As or Unwrap
// method stays visible. The panic value itself is never logged: it may carry
// text from the error.
func chainAs[T any](err error, target *T) (found bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Error chain walk panicked; treating the target as absent.",
				"error_type", fmt.Sprintf("%T", err), "target_type", fmt.Sprintf("%T", target), "panic_type", fmt.Sprintf("%T", r))
			found = false
		}
	}()
	return errors.As(err, target)
}

// exitClass is one of the classes in the exit-code table (ADR-0014,
// forgectl#1085). A verb names a class, never a number, so the table lives in
// one place.
type exitClass int

const (
	// classOK is a verb that did what its name says.
	classOK exitClass = iota
	// classFailed is the default for a verb that ran and whose result did not
	// hold.
	classFailed
	// classUsage is a call or setup the caller can fix; nothing was attempted.
	classUsage
	// classUnauthorized is a credential that is missing, rejected, or too weak.
	classUnauthorized
	// classRefused is a safety rule that said no and will say it again.
	classRefused
)

// The numbers behind the classes. 5 (unreachable) and 6 (not_found) are
// reserved by ADR-0014 and deliberately absent: no verb emits them, and a new
// verb must not reuse them. Outside the table: 75, 130 and 141 (desk watch,
// interrupts) and the exit codes of pass-through verbs.
const (
	exitOK           = 0
	exitFailed       = 1
	exitUsage        = 2
	exitUnauthorized = 3
	exitRefused      = 4
)

// classExit returns the process exit code for a class. It is the only
// resolver and has one mode; a later phase that adds codes 5 and 6 would add
// its second mode here.
func classExit(c exitClass) int {
	switch c {
	case classOK:
		return exitOK
	case classUsage:
		return exitUsage
	case classUnauthorized:
		return exitUnauthorized
	case classRefused:
		return exitRefused
	default:
		return exitFailed
	}
}
