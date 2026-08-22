package k8s

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
)

type streamCall struct {
	name string
	args []string
}

type fakeStreamingRunner struct {
	calls []streamCall
	run   func(context.Context, io.Reader, io.Writer, io.Writer) error
}

func (f *fakeStreamingRunner) RunStreaming(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, streamCall{name: name, args: append([]string(nil), args...)})
	if f.run != nil {
		return f.run(ctx, stdin, stdout, stderr)
	}
	return nil
}

func TestLogs_ForwardsKubectlArgvAndStreamsOrdinaryText(t *testing.T) {
	runner := &fakeStreamingRunner{run: func(_ context.Context, _ io.Reader, stdout, _ io.Writer) error {
		for _, chunk := range []string{"first", " line\nsecond", " line\n"} {
			if _, err := io.WriteString(stdout, chunk); err != nil {
				return err
			}
		}
		return nil
	}}
	var stdout bytes.Buffer
	args := []string{"-n", "prod", "-l", "app=api", "-f", "--all-containers"}

	err := New(runner).Logs(context.Background(), strings.NewReader(""), &stdout, io.Discard, args, LogsOptions{MinLevel: LevelTrace})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	if got, want := runner.calls[0], (streamCall{name: "kubectl", args: append([]string{"logs"}, args...)}); !reflect.DeepEqual(got, want) {
		t.Errorf("call = %#v, want %#v", got, want)
	}
	if got, want := stdout.String(), "first line\nsecond line\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestLogs_FiltersRecognizedJSONButKeepsOperationalContext(t *testing.T) {
	input := strings.Join([]string{
		`{"level":"debug","message":"details"}`,
		`starting sidecar`,
		`{"severity":"warning","message":"slow"}`,
		`{"LEVEL":"ERROR","message":"failed"}`,
		`{"level":17,"message":"unknown shape"}`,
	}, "\n") + "\n"
	runner := &fakeStreamingRunner{run: func(_ context.Context, _ io.Reader, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, input)
		return err
	}}
	var stdout bytes.Buffer

	err := New(runner).Logs(context.Background(), nil, &stdout, io.Discard, []string{"pod/api"}, LogsOptions{MinLevel: LevelWarn})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	want := "starting sidecar\n" +
		`{"severity":"warning","message":"slow"}` + "\n" +
		`{"LEVEL":"ERROR","message":"failed"}` + "\n" +
		`{"level":17,"message":"unknown shape"}` + "\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestLogs_EscapesHostileRunesBeforeAddingTrustedSeverityStyle(t *testing.T) {
	// C1 controls and bidi formatting are legal as raw JSON string runes even
	// though they are unsafe at a terminal. C0 controls such as ESC are not
	// legal raw JSON, and the stderr assertion below covers that plain-text path.
	input := "{\"level\":\"error\",\"message\":\"bad\u009b31m link\u202e\x7f\"}\n"
	runner := &fakeStreamingRunner{run: func(_ context.Context, _ io.Reader, stdout, stderr io.Writer) error {
		if _, err := io.WriteString(stdout, input); err != nil {
			return err
		}
		_, err := io.WriteString(stderr, "diagnostic\x1b[2J\r\n")
		return err
	}}
	var stdout, stderr bytes.Buffer

	err := New(runner).Logs(context.Background(), nil, &stdout, &stderr, []string{"pod/api"}, LogsOptions{MinLevel: LevelTrace, Color: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "\x1b[31m") || !strings.HasSuffix(got, "\x1b[0m\n") {
		t.Fatalf("trusted error style missing: %q", got)
	}
	withoutTrusted := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b[31m"), "\x1b[0m\n")
	if strings.ContainsRune(withoutTrusted, '\u009b') || strings.ContainsRune(withoutTrusted, '\x7f') || strings.ContainsRune(withoutTrusted, '\u202e') {
		t.Errorf("untrusted controls reached stdout: %q", withoutTrusted)
	}
	for _, escaped := range []string{`\u009b`, `\u202e`, `\x7f`} {
		if !strings.Contains(withoutTrusted, escaped) {
			t.Errorf("stdout missing visible escape %q: %q", escaped, withoutTrusted)
		}
	}
	if got, want := stderr.String(), `diagnostic\x1b[2J\r`+"\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestLogs_OversizedLineStreamsBoundedlyAndPreservesSplitUTF8(t *testing.T) {
	prefix := strings.Repeat("a", maxBufferedLine+1)
	runner := &fakeStreamingRunner{run: func(_ context.Context, _ io.Reader, stdout, _ io.Writer) error {
		if _, err := io.WriteString(stdout, prefix); err != nil {
			return err
		}
		if _, err := stdout.Write([]byte{0xe2}); err != nil {
			return err
		}
		if _, err := stdout.Write([]byte{0x82, 0xac, '\n'}); err != nil {
			return err
		}
		return nil
	}}
	var stdout bytes.Buffer

	err := New(runner).Logs(context.Background(), nil, &stdout, io.Discard, []string{"pod/api"}, LogsOptions{MinLevel: LevelFatal})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if got, want := stdout.String(), prefix+"€\n"; got != want {
		t.Errorf("oversized stdout changed: lengths got=%d want=%d, suffix=%q", len(got), len(want), got[len(got)-8:])
	}
}

func TestLogs_PropagatesCancellationAndRunnerErrors(t *testing.T) {
	t.Run("already canceled does not invoke runner", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		runner := &fakeStreamingRunner{}
		err := New(runner).Logs(ctx, nil, io.Discard, io.Discard, []string{"pod/api"}, LogsOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if len(runner.calls) != 0 {
			t.Errorf("calls = %d, want 0 for an already-canceled context", len(runner.calls))
		}
	})

	t.Run("runner error and streamed output survive", func(t *testing.T) {
		sentinel := errors.New("kubectl failed")
		runner := &fakeStreamingRunner{run: func(_ context.Context, _ io.Reader, stdout, stderr io.Writer) error {
			_, _ = io.WriteString(stdout, "partial\n")
			_, _ = io.WriteString(stderr, "reason\n")
			return sentinel
		}}
		var stdout, stderr bytes.Buffer
		err := New(runner).Logs(context.Background(), nil, &stdout, &stderr, []string{"pod/api"}, LogsOptions{})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want sentinel", err)
		}
		if stdout.String() != "partial\n" || stderr.String() != "reason\n" {
			t.Errorf("partial streams lost: stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
}

func TestLogs_ValidationRefusesBeforeRunner(t *testing.T) {
	runner := &fakeStreamingRunner{}
	err := New(runner).Logs(context.Background(), nil, io.Discard, io.Discard, nil, LogsOptions{})
	if err == nil {
		t.Fatal("expected missing-argument error")
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %d, want 0", len(runner.calls))
	}
}

func TestLogs_PropagatesOutputWriteFailure(t *testing.T) {
	sentinel := errors.New("sink closed")
	runner := &fakeStreamingRunner{run: func(_ context.Context, _ io.Reader, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, "line\n")
		return err
	}}
	err := New(runner).Logs(context.Background(), nil, failingWriter{err: sentinel}, io.Discard, []string{"pod/api"}, LogsOptions{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sink failure", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestParseLevel_AliasesAndRefusals(t *testing.T) {
	for value, want := range map[string]Level{
		"TRACE": LevelTrace, "debug": LevelDebug, "info": LevelInfo,
		"warning": LevelWarn, "err": LevelError, "panic": LevelFatal,
	} {
		got, err := ParseLevel(value)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v, nil", value, got, err, want)
		}
	}
	if _, err := ParseLevel("verbose\x1b[2J"); err == nil || strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("hostile invalid level error = %q, want safe refusal", err)
	}
}

var _ forgexec.StreamingRunner = (*fakeStreamingRunner)(nil)
