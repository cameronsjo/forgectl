//go:build unix && !linux

package tui

import "github.com/cameronsjo/forgectl/internal/desk"

// ttyArgv is the foreground command for a TTY item on macOS and the BSDs.
// BSD script(1) takes the command as its trailing operands and hands its
// child every descriptor it inherited, so fd 3 (the verified bytes) reaches
// bash, and it writes the typescript to fd 4 (the log the desk created);
// TestTTYRunPassesFD3 proves both on the platform under test.
func ttyArgv(rcPath string) []string {
	return []string{
		"/usr/bin/script", "-q", ttyLogFD,
		"/bin/bash", "-c", ttyWrapper, "desk-tty", "/bin/bash", desk.ScriptFDPath, rcPath,
	}
}

// ttyEnv is the environment for the script(1) process. BSD script(1) runs
// its trailing operands directly, not through $SHELL, so env is used as is.
func ttyEnv(env []string) []string { return env }
