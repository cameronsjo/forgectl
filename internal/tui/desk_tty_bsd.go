//go:build unix && !linux

package tui

import "github.com/cameronsjo/forgectl/internal/desk"

// ttyArgv is the foreground command for a TTY item on macOS and the BSDs.
// BSD script(1) takes the command as its trailing operands and hands its
// child every descriptor it inherited, so fd 3 (the verified bytes) reaches
// bash; TestTTYRunPassesFD3 proves it on the platform under test.
func ttyArgv(logPath, rcPath string) []string {
	return []string{
		"/usr/bin/script", "-q", logPath,
		"/bin/bash", "-c", ttyWrapper, "desk-tty", "/bin/bash", desk.ScriptFDPath, rcPath,
	}
}
