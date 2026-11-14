// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import "github.com/cameronsjo/forgectl/internal/progstatus"

// reportOnSignal is a no-op off unix: the re-raise needs syscall.Kill.
func reportOnSignal(*progstatus.Reporter, string) (stop func()) { return func() {} }

// reportBlocked reports blocked, runs fn, and clears the row on every return path.
func reportBlocked(r *progstatus.Reporter, kind, msg string, fn func() error) error {
	r.Blocked(kind, msg)
	defer r.Clear()
	return fn()
}
