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

// CodexPath resolves the Codex CLI binary in env-over-config-over-PATH order.
func CodexPath(defaults config.LaunchDefaults) (string, error) {
	home, _ := os.UserHomeDir()
	if env := os.Getenv("FORGECTL_CODEX_BIN"); env != "" {
		return validateBinary(expandTilde(env, home), "FORGECTL_CODEX_BIN", "codex")
	}
	if defaults.CodexBinaryPath != "" {
		return validateBinary(
			expandTilde(defaults.CodexBinaryPath, home),
			"[launch.defaults] codex_binary_path",
			"codex",
		)
	}
	p, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("codex not found on PATH: %w", err)
	}
	return p, nil
}

// validateClaudeBin confirms an explicit claude path exists and is an executable
// regular file, attributing failures to their source (env var or config key).
func validateClaudeBin(path, source string) (string, error) {
	return validateBinary(path, source, "claude")
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
