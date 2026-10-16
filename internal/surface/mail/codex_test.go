package mail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if !IsUnconfirmed(err) || !strings.Contains(err.Error(), "no such thread") || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("runner failure: err = %v, want unconfirmed without control characters", err)
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
	if !IsUnconfirmed(err) || !strings.Contains(err.Error(), "thread not loaded") {
		t.Fatalf("err = %v, want unconfirmed with codex's stderr", err)
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

// A hung codex queue must not hold the mailbox lock: the adapter bounds it.
func TestCodexDeliverTimesOut(t *testing.T) {
	r := blockingRunner{}
	a := CodexAdapter{Runner: r, Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := a.Deliver(context.Background(), Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, "hi")
	if !IsUnconfirmed(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %s, want an unconfirmed timeout", err, time.Since(start))
	}
}

func TestCodexDeliverUsesValidatedRemoteEndpoint(t *testing.T) {
	runner := &fexec.FakeRunner{}
	adapter := CodexAdapter{Runner: runner}
	_, err := adapter.Deliver(context.Background(), Worker{Name: "codex-1", ThreadID: "th_1", Socket: "/tmp/codex.sock"}, Message{}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"queue", "--remote=unix:///tmp/codex.sock", "--thread=th_1", "--message=hi"}
	if !reflect.DeepEqual(runner.Last().Args, want) {
		t.Fatalf("args = %q, want %q", runner.Last().Args, want)
	}
}

func TestCodexDeliverKeepsRemoteEndpointsIsolated(t *testing.T) {
	runner := &fexec.FakeRunner{}
	adapter := CodexAdapter{Runner: runner}
	endpoints := []string{"/tmp/codex-a.sock", "/tmp/codex-b.sock"}
	for _, endpoint := range endpoints {
		if _, err := adapter.Deliver(context.Background(), Worker{Name: "codex-1", ThreadID: "th_1", Socket: endpoint}, Message{}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.Calls) != len(endpoints) {
		t.Fatalf("runner calls = %d, want %d", len(runner.Calls), len(endpoints))
	}
	for index, endpoint := range endpoints {
		want := "--remote=unix://" + endpoint
		if got := runner.Calls[index].Args[1]; got != want {
			t.Errorf("call %d remote = %q, want %q", index, got, want)
		}
	}
}

func TestCodexDeliverRejectsUnsafeRemoteEndpoint(t *testing.T) {
	adapter := CodexAdapter{Runner: &fexec.FakeRunner{}}
	for _, socket := range []string{"relative.sock", "/tmp/../codex.sock", "/tmp/codex\n.sock"} {
		if _, err := adapter.Deliver(context.Background(), Worker{Name: "codex-1", ThreadID: "th_1", Socket: socket}, Message{}, "hi"); err == nil {
			t.Errorf("socket %q was accepted", socket)
		}
	}
}

// blockingRunner waits for its context, as a hung app-server would.
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
