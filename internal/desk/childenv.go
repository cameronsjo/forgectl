package desk

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// childEnvDrop names the variables [ChildEnv] removes. Each makes bash run
// code other than the approved bytes (BASH_ENV and ENV name a file it runs
// first; PS4 is expanded, command substitutions included, before each traced
// command), or changes how the script's own lines behave.
var childEnvDrop = map[string]bool{
	"BASH_ENV": true, "ENV": true, "SHELLOPTS": true, "BASHOPTS": true,
	"CDPATH": true, "GLOBIGNORE": true, "PS4": true,
}

// ChildEnv is the environment an approved item runs with: the operator's own,
// minus bash's startup hooks. That is os.Environ() without BASH_ENV, ENV,
// SHELLOPTS, BASHOPTS, CDPATH, GLOBIGNORE, PS4 and every exported function
// (BASH_FUNC_*), which would replace a command the script calls. Everything
// else, credentials included, passes through: the item is the operator's own
// command.
func ChildEnv() []string { return scrubEnv(os.Environ()) }

func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if childEnvDrop[key] || strings.HasPrefix(key, "BASH_FUNC_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// HomeDir is the working directory an approved item starts in: the
// operator's home, so an item never inherits the desk's own cwd.
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("desk: an item starts in the home directory, and there is none: %w", err)
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("desk: an item starts in the home directory, and $HOME (%s) is not absolute", describe(home))
	}
	return home, nil
}
