package cli

import (
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// topLevelCandidate is an unknown, extension-eligible top-level verb and the
// exact argv suffix that followed it. Host-owned leading --no-icons flags are
// deliberately absent from suffix.
type topLevelCandidate struct {
	verb   string
	suffix []string
}

// externalCommandRuntime is the process boundary for Rung B. Execute supplies
// the OS functions; tests inject inert functions so syscall.Exec never replaces
// the test process.
type externalCommandRuntime struct {
	lookPath func(string) (string, error)
	exec     func(string, []string, []string) error
	environ  func() []string
	stderr   io.Writer
}

func defaultExternalCommandRuntime() externalCommandRuntime {
	return externalCommandRuntime{
		lookPath: osexec.LookPath,
		exec:     syscall.Exec,
		environ:  os.Environ,
		stderr:   os.Stderr,
	}
}

// tryExtensionRungs applies top-level dispatch precedence once: registered
// commands, aliases, and lazy Cobra builtins win before any extension rung;
// future exact workflow-name dispatch (Rung A) belongs next; external commands
// are Rung B. Keeping the shared candidate gate outside either rung prevents a
// future Rung A from accidentally jumping known command precedence.
func tryExtensionRungs(root *cobra.Command, args []string, runtime externalCommandRuntime) (bool, error) {
	candidate, ok := unknownTopLevelCandidate(root, args)
	if !ok {
		return false, nil
	}

	// Rung A (future): exact workflow-name dispatch belongs here, before Rung B.

	return runtime.runExternalCommand(candidate)
}

// unknownTopLevelCandidate accepts only an unknown top-level verb, optionally
// preceded by the host's inert --no-icons flag. All other leading flags, the
// -- sentinel, path-shaped tokens, empty input, and bare "-" retain the
// existing Cobra/TUI behavior without probing PATH.
func unknownTopLevelCandidate(root *cobra.Command, args []string) (topLevelCandidate, bool) {
	for i, token := range args {
		switch {
		case token == "--":
			return topLevelCandidate{}, false
		case token == "--no-icons" || strings.HasPrefix(token, "--no-icons="):
			continue
		case token == "-" || token == "" || isFlag(token):
			return topLevelCandidate{}, false
		}

		if strings.ContainsAny(token, `/\`) {
			return topLevelCandidate{}, false
		}
		if builtinVerbs[token] || findChild(root, token) != nil {
			return topLevelCandidate{}, false
		}

		return topLevelCandidate{
			verb:   token,
			suffix: append([]string(nil), args[i+1:]...),
		}, true
	}

	return topLevelCandidate{}, false
}

// runExternalCommand implements Rung B. Lookup errors (including exec.ErrDot)
// and relative results are ordinary misses. Once an absolute executable has
// been resolved, an exec return is terminal: report it once and fail closed.
func (runtime externalCommandRuntime) runExternalCommand(candidate topLevelCandidate) (bool, error) {
	path, err := runtime.lookPath(meta.AppName + "-" + candidate.verb)
	if err != nil || !filepath.IsAbs(path) {
		return false, nil
	}

	argv := make([]string, 1, len(candidate.suffix)+1)
	argv[0] = path
	argv = append(argv, candidate.suffix...)
	environ := append([]string(nil), runtime.environ()...)

	err = runtime.exec(path, argv, environ)
	if err != nil {
		// The exec error is the caller's identity-bearing result; an unavailable
		// diagnostic writer must not replace or wrap it.
		// SafeLine, so the diagnostic is one inert physical line: the text comes
		// from a third-party extension binary, which is exactly the source with
		// no reason to be trusted with the operator's cursor.
		_, _ = fmt.Fprintln(runtime.stderr, termsafe.SafeLine(meta.AppName+": "+err.Error()))
	}
	return true, err
}
