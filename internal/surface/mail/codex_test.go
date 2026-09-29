package mail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	name string
	args []string
	out  []byte
	err  error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.name = name
	f.args = args
	return f.out, f.err
}

func TestCodexDeliver(t *testing.T) {
	r := &fakeRunner{}
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

	detail, err := a.Deliver(ctx, Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, "-rf hello")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"queue", "--thread=th_1", "--message=-rf hello"}
	if r.name != "codex" || !reflect.DeepEqual(r.args, want) {
		t.Fatalf("ran %s %q, want codex %q", r.name, r.args, want)
	}
	if !strings.Contains(detail, "th_1") {
		t.Fatalf("detail %q", detail)
	}

	r.err = errors.New("exit status 1")
	r.out = []byte("no such thread\x1b[0m\n")
	_, err = a.Deliver(ctx, Worker{Name: "codex-1", ThreadID: "th_1"}, Message{}, "hi")
	if !IsRetryable(err) || !strings.Contains(err.Error(), "no such thread") || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("runner failure: err = %v", err)
	}
}
