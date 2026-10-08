//go:build linux

package tui

import (
	"strings"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// ttyArgv is the foreground command for a TTY item on Linux. util-linux
// script(1) takes the command as one string (-c) that it runs with $SHELL
// -c, so the wrapper and the rc path are single-quoted into it; the log path
// stays a separate operand. -e returns the child's status, but the rc file is
// still what the desk reads. The typescript goes to fd 4, the log the desk
// created. TestTTYRunPassesFD3 proves fd 3 reaches bash and fd 4 the log.
func ttyArgv(rcPath string) []string {
	command := strings.Join([]string{
		"/bin/bash", "-c", shellQuote(ttyWrapper), "desk-tty", "/bin/bash", desk.ScriptFDPath, shellQuote(rcPath),
	}, " ")
	return []string{"/usr/bin/script", "-q", "-e", "-c", command, ttyLogFD}
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ttyEnv is the environment for the script(1) process on Linux. util-linux
// script(1) runs its command with $SHELL -c, and a shell other than bash may
// read startup files there (zsh reads .zshenv) and run code, or change
// directory, before the item. SHELL is set to /bin/bash, which reads only
// BASH_ENV when non-interactive, and ChildEnv has removed that. The item
// sees SHELL=/bin/bash.
func ttyEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "SHELL=") {
			out = append(out, kv)
		}
	}
	return append(out, "SHELL=/bin/bash")
}
