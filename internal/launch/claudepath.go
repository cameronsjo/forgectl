package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// layered is one harness's resolution ladder: the env override, the config key,
// and the PATH name to fall back on. Both harnesses differ only in these
// values, so the ladder is described once and walked once (resolveLayered)
// rather than written twice.
type layered struct {
	envKey      string
	envSource   BinarySource
	configPath  string
	configLabel string
	configSrc   BinarySource
	name        string
}

// resolveLayered walks env → config → PATH, validating each explicit selection
// against the layer that supplied it. An explicit path that does not point at
// an existing executable file is an error, never a silent fall-through to PATH:
// an operator who set the knob wants that binary, and quietly running a
// different one is the failure mode worth refusing.
//
// Every layer's result is made absolute before it is returned. `forgectl launch`
// execs in place, so a relative path resolved fine there; a second consumer that
// honors Invocation.CWD does not, and a name validated against one directory
// would exec whatever sits at that name in another. Absolute by construction is
// the version of that guarantee a caller cannot forget.
func resolveLayered(l layered) (ResolvedBinary, error) {
	// A missing home directory is only fatal for a path that needs one. Ignoring
	// the error here and refusing at the point of use (homeless, below) keeps an
	// absolute or PATH-based resolution working on a machine with no $HOME.
	home, homeErr := os.UserHomeDir()
	// A failed layer returns the zero ResolvedBinary, never a source label over
	// an empty path: a caller gating on Source must not see a category for a
	// resolution that did not happen.
	if env := os.Getenv(l.envKey); env != "" {
		if err := homeless(env, home, homeErr, l.envKey); err != nil {
			return ResolvedBinary{}, err
		}
		path, err := validateBinary(expandTilde(env, home), l.envKey, l.name)
		if err != nil {
			return ResolvedBinary{}, err
		}
		return absolute(path, l.envSource, l.envKey)
	}
	if l.configPath != "" {
		if err := homeless(l.configPath, home, homeErr, l.configLabel); err != nil {
			return ResolvedBinary{}, err
		}
		path, err := validateBinary(expandTilde(l.configPath, home), l.configLabel, l.name)
		if err != nil {
			return ResolvedBinary{}, err
		}
		return absolute(path, l.configSrc, l.configLabel)
	}
	// The PATH layer needs no absolutising of its own: exec.LookPath refuses a
	// hit it resolved relative to the current directory (ErrDot, "cannot run
	// executable found relative to current directory"), which covers both an
	// empty $PATH component and a relative one. absolute() runs anyway so the
	// guarantee holds at this function's boundary rather than depending on a
	// standard-library behavior that a future caller would have to know about.
	p, err := exec.LookPath(l.name)
	if err != nil {
		return ResolvedBinary{}, fmt.Errorf("%s not found on PATH: %w", l.name, err)
	}
	return absolute(p, BinaryPATH, "$PATH")
}

// homeless refuses a tilde path when no home directory could be determined,
// rather than letting expandTilde quietly drop the prefix.
//
// The quiet version is the hazard: expandTilde renders "~/bin/claude" through
// filepath.Join("", "bin/claude"), which yields "bin/claude" — a path relative
// to whatever the process cwd happens to be, not the literal the operator
// wrote. It then either fails validation naming a path with no tilde in it (a
// confusing error), or, if a file of that name happens to sit in the cwd,
// silently resolves to a completely different binary. Refusing names the real
// problem instead.
func homeless(path, home string, homeErr error, source string) error {
	if home != "" || !strings.HasPrefix(path, "~") {
		return nil
	}
	return fmt.Errorf(
		"cannot expand %q from %s: no home directory (%w); use an absolute path instead",
		path, source, homeErr,
	)
}

// absolute pins a validated path to the directory it was validated against.
// filepath.Abs joins against the process cwd — the same cwd validateBinary's
// stat resolved through — so it names the file that was actually checked, and
// keeps naming it after a consumer changes directory.
func absolute(path string, source BinarySource, label string) (ResolvedBinary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ResolvedBinary{}, fmt.Errorf("resolve absolute path for binary from %s: %w", label, err)
	}
	return ResolvedBinary{Path: abs, Source: source}, nil
}

// ClaudePath resolves the claude binary to exec, in precedence order:
//
//  1. $FORGECTL_CLAUDE_BIN — an explicit override (env wins over config)
//  2. [launch.defaults] binary_path in config.toml
//  3. `claude` on $PATH
//
// An explicit path (1 or 2) must point at an existing, executable file; if it
// does not, a clear error is returned rather than silently falling back to
// PATH. Folds in forgectl#289 so a wrapped or non-PATH claude (e.g. a cmux
// launcher loop) is reachable.
//
// A compatibility wrapper over ResolveBinary, kept because ten call sites want
// only the path. Callers that need to know which layer chose it — the surface
// policy — call ResolveBinary instead.
func ClaudePath(defaults config.LaunchDefaults) (string, error) {
	resolved, err := ResolveBinary("claude", defaults)
	return resolved.Path, err
}

// CodexPath resolves the Codex CLI binary in env-over-config-over-PATH order.
// A compatibility wrapper over ResolveBinary, on the same terms as ClaudePath.
func CodexPath(defaults config.LaunchDefaults) (string, error) {
	resolved, err := ResolveBinary("codex", defaults)
	return resolved.Path, err
}

func validateBinary(path, source, name string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s binary from %s is unusable: %w", name, source, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s binary from %s is a directory: %s", name, source, path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s binary from %s is not executable: %s", name, source, path)
	}
	return path, nil
}
