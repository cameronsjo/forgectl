package workflow

import "log/slog"

// Verifier gates a workflow file before it is planned (ADR-0002's "verify"
// stage runs before any resolved step is built). The interface is
// scheme-agnostic by design — #10 drops in an Ed25519/minisign implementation
// without touching the executor.
type Verifier interface {
	Verify(path string) error
}

// AllowAllVerifier is the spike's no-op Verifier: every file passes. #10
// replaces this with a real signature check.
type AllowAllVerifier struct{}

// Verify always succeeds.
func (AllowAllVerifier) Verify(path string) error {
	slog.Debug("Verifier passed (no-op AllowAllVerifier).", "path", path)
	return nil
}
