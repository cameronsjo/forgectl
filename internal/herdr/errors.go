package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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

// Error renders Message through redact.Text before printable (#816), like
// every other child-stderr text forgectl renders: herdr can echo a value it
// was handed, and a line holding a credential shape reads as [redacted].
// redact.Text works per line, so it runs first, while the line breaks that
// printable escapes still mark its boundaries.
func (e *Error) Error() string {
	return "herdr: " + printable(e.Code) + ": " + printable(redact.Text(e.Message))
}

// printable renders herdr text as one inert terminal line through
// termsafe.SafeLine, as forgectl's other child-stderr echoes are. herdr's
// text can echo pane-controlled values (labels, titles): a decoded \u001b
// would drive a terminal that prints the error, and a bidi override or other
// format character (Cf, e.g. U+202E) would reorder what the operator reads
// (#825). SafeLine shows each such rune as its escape rather than dropping
// it, so the operator can see something was there.
func printable(s string) string {
	return termsafe.SafeLine(s)
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
	return fmt.Errorf("herdr %s: %w", argvText(args), err)
}

// argvText renders a herdr argv for error text through redact.Args (#782).
// forgectl builds every herdr argv itself today, so this closes a latent
// path: an id or label a user supplies later cannot carry a credential into
// an error message verbatim.
func argvText(args []string) string {
	return strings.Join(redact.Args(args), " ")
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
