// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package docscli

import (
	"errors"
	"fmt"
	"log/slog"
)

type codedError struct {
	err  error
	code int
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

// WithExitCode wraps err so ExitCode(err) reports code instead of 1.
func WithExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	return &codedError{err: err, code: code}
}

type silentCodedError struct {
	code int
}

func (e *silentCodedError) Error() string { return "" }
func (e *silentCodedError) ExitCode() int { return e.code }

func newSilentCodedError(code int) error {
	return &silentCodedError{code: code}
}

// ExitCode walks err's chain for a codedError or silentCodedError and returns
// its code, or 1 when err carries none.
func ExitCode(err error) int {
	var coded *codedError
	if chainAs(err, &coded) {
		return coded.ExitCode()
	}
	var silent *silentCodedError
	if chainAs(err, &silent) {
		return silent.ExitCode()
	}
	return 1
}

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
