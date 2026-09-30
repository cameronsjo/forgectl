package herdr

import (
	"context"
	"errors"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"

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

func TestErrorTextCarriesNoControlCharacters(t *testing.T) {
	// herdr can echo a pane-controlled value in a message; a decoded \u001b must not
	// reach a terminal that prints the error.
	ce := &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: `{"error":{"code":"x\u001by","message":"a\u001b[31mred\u0007\nb"}}`}
	_, err := New(runnerFor("", ce)).Workspaces(context.Background())
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *Error", err)
	}
	for _, s := range []string{he.Error(), (&Declined{TabID: "w1:t\u001b1", Reason: "r\u001b[0m\n"}).Error()} {
		for _, r := range s {
			if unicode.IsControl(r) {
				t.Errorf("%q contains control character %U", s, r)
			}
		}
	}
	if he.Code != "x\x1by" {
		t.Errorf("Code = %q; the raw field must stay as herdr sent it", he.Code)
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

// TestEnvelopeFromAKilledChildIsNotHerdrsRefusal uses a REAL child. exec.OSRunner
// reports a context kill as an *os/exec.ExitError ("signal: killed") with
// ExitCode -1; it does not wrap context.DeadlineExceeded, so a hand-built
// CommandError would have hidden that. Callers tell a timeout by ctx.Err().
func TestEnvelopeFromAKilledChildIsNotHerdrsRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	// One second, so a loaded machine cannot kill sh before its echo lands.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	const envelope = `{"error":{"code":"workspace_not_found","message":"m"}}`
	// exec replaces sh with sleep so the kill lands on the process holding stderr.
	_, runErr := exec.OSRunner{}.Run(ctx, "sh", "-c", "echo '"+envelope+"' >&2; exec sleep 5")

	var ce *exec.CommandError
	if !errors.As(runErr, &ce) || ce.ExitCode != -1 || !strings.Contains(ce.Stderr, "workspace_not_found") {
		t.Fatalf("precondition: want a killed child with the envelope on stderr, got %v (%+v)", runErr, ce)
	}
	if ctx.Err() == nil {
		t.Fatal("precondition: the context should have expired")
	}

	err := classify([]string{"workspace", "list"}, runErr)
	var he *Error
	if errors.As(err, &he) {
		t.Fatalf("got *Error %+v for a killed child; that is a timeout, not a herdr refusal", he)
	}
	var got *exec.CommandError
	if !errors.As(err, &got) || got.ExitCode != -1 {
		t.Fatalf("err = %v, want the wrapped *exec.CommandError with exit -1", err)
	}
}

func TestRunnerErrorWithoutCommandErrorIsWrappedNotSwallowed(t *testing.T) {
	sentinel := errors.New("herdr not found on PATH")
	_, err := New(runnerFor("", sentinel)).Workspaces(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the runner error", err)
	}
}

// herdr error text renders its argv through redact.Args and CheckFork's
// stderr through redact.Text (#782): forgectl builds every herdr argv today,
// so this pins the path a future user-supplied id or label would take.
//
// Mutation that turns it red: make argvText a raw strings.Join (the classify
// and decode rows show the token), or drop redact.Text from CheckFork's exit
// arm (the probe row shows it).
func TestHerdrErrorTextRedactsArgvAndStderr(t *testing.T) {
	const secret = "SEKRIT-herdr-782" //nolint:gosec // G101: a fake credential the test plants
	args := []string{"tab", "move", "--token", secret}

	if err := classify(args, errors.New("boom")); strings.Contains(err.Error(), secret) {
		t.Errorf("classify: %q carries the credential", err)
	}
	if _, err := read[struct{}](context.Background(), New(runnerFor("not json", nil)), args...); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("read decode failure: %v (want an error without the credential)", err)
	}
	if _, err := read[struct{}](context.Background(), New(runnerFor(`{"id":1}`, nil)), args...); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("read missing result: %v (want an error without the credential)", err)
	}

	stderr := "error: Authorization: Bearer " + secret
	probe := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 3, Stderr: stderr})
	err := CheckFork(context.Background(), probe)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("CheckFork: %v (want an error without the credential)", err)
	}
}

// TestErrorMessageIsRedacted is #816: an envelope's Message rendered through
// printable only, so a credential herdr echoed in it reached the error text.
// The line without a credential shape survives.
//
// Mutation: drop redact.Text from (*Error).Error and the secret shows; apply
// it after printable and the whole message collapses to one withheld line,
// so "kept line" disappears.
func TestErrorMessageIsRedacted(t *testing.T) {
	const secret = "SEKRIT-herdr-816" //nolint:gosec // G101: a fake credential the test plants
	env := `{"error":{"code":"bad_request","message":"kept line\nAuthorization: Bearer ` + secret + `"}}`
	ce := &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: env}
	_, err := New(runnerFor("", ce)).Workspaces(context.Background())
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if got := he.Error(); strings.Contains(got, secret) || !strings.Contains(got, "kept line") {
		t.Errorf("Error() = %q; want the credential line withheld and the other kept", got)
	}
}
