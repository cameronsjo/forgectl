// Package surface is the coordinator that launches a harness inside a terminal
// manager without the manager ever seeing the harness invocation.
//
// A manager necessarily learns the target directory and the bootstrap command
// it is asked to type, because it creates the workspace. What it must never
// learn is the resolved harness path, argv, environment, or prompt. Those are
// delivered to a private trampoline over a local socket after the workspace
// exists, so this package — not the adapter — owns the invocation from
// resolution through commit.
//
// The adapter-facing contract lives in the backend subpackage, which does not
// depend on internal/launch. That split is what makes the privacy boundary a
// property of the dependency graph rather than of review discipline; the
// admission policy below is the piece that needs launch, and it is the reason
// the split had to exist.
//
// Phase 4a ships the typed core and this policy. The protocol, trampoline, and
// Service.Launch state machine are forgectl#331 Phase 4b; the real backends
// are forgectl#332 Phase 5.
package surface

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

var (
	// ErrBinaryProvenance reports a harness binary whose provenance the
	// surface will not accept without an explicit opt-in.
	ErrBinaryProvenance = errors.New("surface: harness binary was found on $PATH")
	// ErrBinaryUnusable reports a harness path that is not an absolute,
	// executable, regular file.
	ErrBinaryUnusable = errors.New("surface: harness binary is not usable")
	// ErrBinarySelfLoop reports a harness path that is this forgectl binary.
	ErrBinarySelfLoop = errors.New("surface: harness binary is forgectl itself")
)

// Policy is the surface's admission decision about a resolved harness binary.
//
// It is separate from launch's resolution because the two answer different
// questions. Resolution asks "which binary did the operator's configuration
// name"; this asks "is that provenance good enough to hand to a terminal
// manager we are about to hand a rendezvous nonce". An ordinary local launch
// is the operator running a command in their own shell; a surface launch puts
// the same binary behind a bootstrap that a manager types, which is one more
// step removed from anyone watching.
type Policy struct {
	// AllowPATHBinary opts in to a harness discovered by searching $PATH.
	//
	// The distinction it turns on is intent, not authenticity. An env variable
	// or a config file naming a binary is an operator asserting *this one*;
	// $PATH is whatever the ambient environment happened to resolve, which is
	// the case where a shim earlier in the path silently becomes the harness.
	// The flag acknowledges that risk. It waives nothing else: the path checks
	// below still run.
	AllowPATHBinary bool
}

// AcceptBinary decides whether a resolved binary may back a surface launch.
//
// self is the current forgectl executable, resolved once by the caller. The
// checks run provenance first, then shape, then the self-loop — cheapest and
// most decisive first, and so that a refusal names the reason an operator can
// act on rather than the first stat that happened to fail.
func (p Policy) AcceptBinary(b launch.ResolvedBinary, self string) error {
	switch b.Source {
	case launch.BinaryClaudeEnv, launch.BinaryClaudeConfig,
		launch.BinaryCodexEnv, launch.BinaryCodexConfig:
		// An explicit selection is operator intent. It is deliberately not a
		// claim that the binary is an official harness: an intentional wrapper
		// is a legitimate thing to point forgectl at, and same-UID replacement
		// after this check is outside the threat model either way.
		//
		// Recorded residual, since "other local UIDs" *is* in the threat
		// model: nothing here rejects a binary sitting under a world- or
		// group-writable directory, where another UID could swap it — before
		// this check or between it and the eventual exec. A permission check
		// on the file and its parent would close most of that, and is not
		// done because a group-writable shared bin is a legitimate layout on
		// some machines and refusing it would break real setups over a risk
		// the operator already accepted by naming that path. Revisit if the
		// threat model ever promotes other-UID tampering above operator
		// intent.
	case launch.BinaryPATH:
		if !p.AllowPATHBinary {
			return fmt.Errorf("%w: %s; pass --allow-path-binary to accept it",
				ErrBinaryProvenance, termsafe.QuotePath(b.Path))
		}
	default:
		return fmt.Errorf("%w: unknown provenance %q", ErrBinaryUnusable, b.Source)
	}

	if b.Path == "" || !filepath.IsAbs(b.Path) {
		return fmt.Errorf("%w: %s is not an absolute path",
			ErrBinaryUnusable, termsafe.QuotePath(b.Path))
	}

	info, err := os.Stat(b.Path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrBinaryUnusable, termsafe.QuotePath(b.Path), termsafe.Error(err))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file",
			ErrBinaryUnusable, termsafe.QuotePath(b.Path))
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable",
			ErrBinaryUnusable, termsafe.QuotePath(b.Path))
	}

	// The self-loop check is by file identity, not by string comparison.
	// A symlink, a hard link, or a differently-spelled absolute path all name
	// the same inode, and any of them would produce a forgectl that re-execs
	// itself as its own harness — a fork bomb wearing a launch command.
	//
	// An empty self is a caller that never resolved one, which would disable
	// the check silently, so it is refused rather than skipped. A stat that
	// fails is the guard's own failure rather than a bad input, so it warns
	// and admits: a guard must not block the operation it protects because it
	// could not run, but it must say that it did not run.
	if self == "" {
		return fmt.Errorf("%w: cannot prove %s is not forgectl itself without the running executable",
			ErrBinaryUnusable, termsafe.QuotePath(b.Path))
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		slog.Warn("Self-loop check did not run; could not stat the running executable.",
			"binary_source", string(b.Source), "error", termsafe.Error(err))
		return nil
	}
	if os.SameFile(info, selfInfo) {
		return fmt.Errorf("%w: %s", ErrBinarySelfLoop, termsafe.QuotePath(b.Path))
	}

	return nil
}

// SelfPath resolves the running forgectl executable for the self-loop check.
func SelfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("surface: resolve forgectl executable: %w", err)
	}
	return self, nil
}
