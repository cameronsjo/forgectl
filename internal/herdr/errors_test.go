package herdr

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// liveShapes are id shapes a live herdr session produces and a sanitized
// fixture must not carry: raw terminal ids, uuids, and base32-style
// workspace ids (w1, w2, ... are the sanitized form).
var liveShapes = []*regexp.Regexp{
	regexp.MustCompile(`term_[0-9a-f]{8,}`),
	regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-`),
	regexp.MustCompile(`"w[0-9]+[A-Za-z][0-9A-Za-z]*"`),
}

func TestErrorEnvelopeBecomesTypedError(t *testing.T) {
	stderr, err := os.ReadFile("testdata/err_workspace_not_found.json")
	if err != nil {
		t.Fatal(err)
	}
	ce := &exec.CommandError{Name: Binary, Args: []string{"tab", "list", "--workspace", "wNOPE"}, ExitCode: 1, Stderr: string(stderr)}
	_, gotErr := New(runnerFor("", ce)).Tabs(context.Background(), "wNOPE")
	var he *Error
	if !errors.As(gotErr, &he) {
		t.Fatalf("err = %v, want *Error", gotErr)
	}
	if he.Code != "workspace_not_found" || he.Message != "workspace wNOPE not found" {
		t.Errorf("error = %+v", he)
	}
}

func TestServerNotRunningIsATypedError(t *testing.T) {
	ce := &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: `{"id":"cli:workspace:list","error":{"code":"server_not_running","message":"no herdr server is running at /nonexistent/herdr.sock"}}`}
	_, err := New(runnerFor("", ce)).Workspaces(context.Background())
	var he *Error
	if !errors.As(err, &he) || he.Code != "server_not_running" {
		t.Fatalf("err = %v, want *Error server_not_running", err)
	}
}

func TestUnparsableStderrStaysTheWrappedCommandError(t *testing.T) {
	good := `{"error":{"code":"workspace_not_found","message":"m"}}`
	for _, tt := range []struct {
		name string
		ce   *exec.CommandError
	}{
		{"truncated stderr", &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: good, StderrDropped: 40}},
		{"log line before the json", &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: "warn: retrying\n" + good}},
		{"not json", &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: "boom"}},
		{"empty stderr", &exec.CommandError{Name: Binary, ExitCode: 1}},
		{"envelope without a code", &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: `{"error":{"message":"m"}}`}},
		{"two objects", &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: good + "\n" + good}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(runnerFor("", tt.ce)).Workspaces(context.Background())
			var he *Error
			if errors.As(err, &he) {
				t.Fatalf("got *Error %+v, want the wrapped *exec.CommandError", he)
			}
			var ce *exec.CommandError
			if !errors.As(err, &ce) || ce != tt.ce {
				t.Fatalf("err = %v, want the original *exec.CommandError", err)
			}
		})
	}
}

func TestErrorUnwrapsToTheCommandError(t *testing.T) {
	ce := &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: `{"error":{"code":"tab_not_found","message":"m"}}`}
	_, err := New(runnerFor("", ce)).Workspaces(context.Background())
	var got *exec.CommandError
	if !errors.As(err, &got) || got != ce {
		t.Fatalf("errors.As(*exec.CommandError) = %v, want the original", got)
	}
}

func TestEnvelopeFromAKilledChildIsNotHerdrsRefusal(t *testing.T) {
	// The context deadline killed herdr after it wrote a complete error object.
	// That is a timeout, not a herdr refusal, and callers must be able to tell.
	ce := &exec.CommandError{
		Name: Binary, ExitCode: -1, Err: context.DeadlineExceeded,
		Stderr: `{"error":{"code":"workspace_not_found","message":"m"}}`,
	}
	_, err := New(runnerFor("", ce)).Workspaces(context.Background())
	var he *Error
	if errors.As(err, &he) {
		t.Fatalf("got *Error %+v for a killed child", he)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to still match context.DeadlineExceeded", err)
	}
}

func TestRunnerErrorWithoutCommandErrorIsWrappedNotSwallowed(t *testing.T) {
	sentinel := errors.New("herdr not found on PATH")
	_, err := New(runnerFor("", sentinel)).Workspaces(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the runner error", err)
	}
}
