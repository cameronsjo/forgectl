package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// exitFailure is the status herdr exits with when it refuses a request
// (measured). A stream with the error envelope from any other status, such as
// -1 for a killed or cancelled child, is not herdr's refusal.
const exitFailure = 1

// Error is herdr's structured refusal: {"error":{"code","message"}} on stderr
// with exit 1. Code is herdr's own vocabulary (workspace_not_found,
// server_not_running, ...); match on it, not on Message. It unwraps to the
// *[exec.CommandError] it came from.
type Error struct {
	Code    string
	Message string
	cause   error
}

func (e *Error) Error() string {
	return "herdr: " + printable(e.Code) + ": " + printable(e.Message)
}

// printable drops control characters. herdr's text can echo pane-controlled
// values (labels, titles), and a decoded \u001b would otherwise reach a
// terminal that prints the error.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Unwrap returns the *[exec.CommandError] behind the refusal.
func (e *Error) Unwrap() error { return e.cause }

// classify turns a runner failure into an *Error when herdr exited with its
// failure status and stderr carries its envelope, and otherwise wraps the
// original error with the command that ran. Truncated stderr is never parsed:
// a cut tail can look like valid JSON with the wrong code. Nor is a stream
// from a child that was killed or timed out.
func classify(args []string, err error) error {
	var ce *exec.CommandError
	if errors.As(err, &ce) && ce.ExitCode == exitFailure && ce.StderrDropped == 0 {
		if e := parseEnvelope(ce.Stderr); e != nil {
			e.cause = ce
			return e
		}
	}
	return fmt.Errorf("herdr %s: %w", strings.Join(args, " "), err)
}

// parseEnvelope returns the *Error in a stderr stream that is exactly one
// herdr error object, or nil. Log lines before the JSON, a second object, or
// an envelope without a code all return nil.
func parseEnvelope(stderr string) *Error {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &env); err != nil {
		return nil
	}
	if env.Error == nil || env.Error.Code == "" {
		return nil
	}
	return &Error{Code: env.Error.Code, Message: env.Error.Message}
}
