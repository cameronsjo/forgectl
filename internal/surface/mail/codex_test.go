package mail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// The adapter takes forgectl's own runner, so the repo's fake stands in for it.
var _ Runner = (fexec.Runner)(nil)

func TestCodexDeliver(t *testing.T) {
	var runErr error
	r := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", runErr }}
	a := CodexAdapter{Runner: r}
	ctx := context.Background()

	_, err := a.Deliver(ctx, Worker{Name: "codex-1"}, Message{}, "hi")
	if !IsRetryable(err) {
		t.Fatalf("no thread id: err = %v, want retryable", err)
	}

	_, err = a.Deliver(ctx, Worker{Name: "codex-1", ThreadID: "-x"}, Message{}, "hi")
	if err == nil || IsRetryable(err) {
		t.Fatalf("bad thread id: err = %v, want a permanent error", err)
	}
	if len(r.Calls) != 0 {
		t.Fatalf("ran codex %d times before a usable thread id", len(r.Calls))
	}

	detail, err := a.Deliver(ctx, Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, "-rf hello")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"queue", "--thread=th_1", "--message=-rf hello"}
	if last := r.Last(); last.Name != "codex" || !reflect.DeepEqual(last.Args, want) {
		t.Fatalf("ran %s %q, want codex %q", last.Name, last.Args, want)
	}
	if !strings.Contains(detail, "th_1") {
		t.Fatalf("detail %q", detail)
	}

	runErr = errors.New("no such thread\x1b[0m\n")
	_, err = a.Deliver(ctx, Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, "hi")
	if !IsRetryable(err) || !strings.Contains(err.Error(), "no such thread") || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("runner failure: err = %v", err)
	}
}

// A failed codex queue must not copy the message body into the status detail:
// the production runner's error text renders argv, so the body is masked.
func TestCodexDeliverMasksBodyInFailure(t *testing.T) {
	body := "a private plan the log should not keep"
	r := &fexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", &fexec.CommandError{Name: name, Args: args, Stderr: "thread not loaded"}
	}}
	a := CodexAdapter{Runner: r}
	_, err := a.Deliver(context.Background(), Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, body)
	if !IsRetryable(err) || !strings.Contains(err.Error(), "thread not loaded") {
		t.Fatalf("err = %v, want retryable with codex's stderr", err)
	}
	// The fake records the real argv; the mask applies where the production
	// runner renders it, which the next assertion drives through OSRunner.
	if got := r.Last().Args[2]; got != "--message="+body {
		t.Fatalf("codex got %q, want the unmasked body on argv", got)
	}
	_, err = CodexAdapter{Runner: fexec.OSRunner{}, Bin: "/nonexistent/codex"}.Deliver(context.Background(), Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, body)
	if err == nil || strings.Contains(err.Error(), body) {
		t.Fatalf("err = %v, want a failure that does not quote the body", err)
	}
}
