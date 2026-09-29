package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Error is herdr's structured refusal: {"error":{"code","message"}} on stderr
// with exit 1. Code is herdr's own vocabulary (workspace_not_found,
// server_not_running, ...); match on it, not on Message.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	return "herdr: " + e.Code + ": " + e.Message
}

// classify turns a runner failure into an *Error when stderr carries herdr's
// envelope, and otherwise wraps the original error with the command that ran.
// Truncated stderr is never parsed: a cut tail can look like valid JSON with
// the wrong code.
func classify(args []string, err error) error {
	var ce *exec.CommandError
	if errors.As(err, &ce) && ce.StderrDropped == 0 {
		if e := parseEnvelope(ce.Stderr); e != nil {
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
