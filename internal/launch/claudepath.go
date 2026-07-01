package launch

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/cameronsjo/forgectl/internal/config"
)

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
func ClaudePath(defaults config.LaunchDefaults) (string, error) {
	home, _ := os.UserHomeDir()
	if env := os.Getenv("FORGECTL_CLAUDE_BIN"); env != "" {
		return validateClaudeBin(expandTilde(env, home), "FORGECTL_CLAUDE_BIN")
	}
	if defaults.BinaryPath != "" {
		return validateClaudeBin(expandTilde(defaults.BinaryPath, home), "[launch.defaults] binary_path")
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude not found on PATH: %w", err)
	}
	return p, nil
}

// validateClaudeBin confirms an explicit claude path exists and is an executable
// regular file, attributing failures to their source (env var or config key).
func validateClaudeBin(path, source string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("claude binary from %s is unusable: %w", source, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("claude binary from %s is a directory: %s", source, path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("claude binary from %s is not executable: %s", source, path)
	}
	return path, nil
}
