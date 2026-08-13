package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// xdgStateHomeEnv names the base directory spec's state root. Launch usage
// statistics live under state, not config: they are machine-local, disposable
// activity records, and nothing in them belongs in a config backup.
const xdgStateHomeEnv = "XDG_STATE_HOME"

// ErrUsageBaseUnsafe reports a state base forgectl refuses to derive a usage
// path from. It is deliberately one error for every refusal reason: the caller
// (a silent recorder on the launch hot path) has exactly one disposition —
// collect nothing — and a taxonomy here would only invite a caller to try
// harder against a base it was told not to trust.
var ErrUsageBaseUnsafe = errors.New("launch usage state base is unsafe")

// LaunchUsageBase resolves the absolute directory that holds forgectl's
// machine-local state, honoring XDG_STATE_HOME and falling back to
// $HOME/.local/state.
//
// It is deliberately strict about what it will accept, because everything
// downstream is opened relative to a descriptor pinned on this path:
//
//   - a relative (or "~"-prefixed) XDG_STATE_HOME is refused rather than
//     resolved against the process cwd — the cwd during a launch is the
//     operator's project, and a relative state root would scatter usage rows
//     into repositories;
//   - a base that is itself a symlink is refused rather than followed, so a
//     substituted state root cannot redirect the leaf created under it. The
//     store still opens every component no-follow; this is the cheap first
//     refusal, not the only one.
//
// The returned path is cleaned but never resolved through symlinks — callers
// pin it with an O_NOFOLLOW open, which is the check that actually holds.
func LaunchUsageBase() (string, error) {
	base := ""
	switch xdg := os.Getenv(xdgStateHomeEnv); {
	case xdg != "":
		base = xdg
	default:
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("%w: no home directory to resolve %s against", ErrUsageBaseUnsafe, xdgStateHomeEnv)
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("%w: home directory is not absolute", ErrUsageBaseUnsafe)
		}
		base = filepath.Join(home, ".local", "state")
	}

	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("%w: %s must be an absolute path, got %s",
			ErrUsageBaseUnsafe, xdgStateHomeEnv, termsafe.QuotePath(base))
	}
	base = filepath.Clean(base)

	switch info, err := os.Lstat(base); {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return "", fmt.Errorf("%w: %s is a symlink", ErrUsageBaseUnsafe, termsafe.QuotePath(base))
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("%w: %s is not a directory", ErrUsageBaseUnsafe, termsafe.QuotePath(base))
	}
	// An absent base is fine — the writer creates it; the reader and doctor
	// treat it as an empty store.
	return base, nil
}
