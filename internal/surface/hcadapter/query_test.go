package hcadapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestQueries_ShareTheAdaptersRunner: the watch's reads go through the same
// runner as every adapter call, so each drops the launcher's session handles,
// and a missing terminal is told apart from a failed call.
func TestQueries_ShareTheAdaptersRunner(t *testing.T) {
	f := &fakeHC{terminals: map[string]string{"t1": `{"address":"t1"}`}, lsReply: `{"terminals":[]}`}
	run := f.runner()
	a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if raw, err := a.GetJSON(ctx, "t1"); err != nil || string(raw) != `{"address":"t1"}` {
		t.Errorf("GetJSON(t1) = %q, %v", raw, err)
	}
	if _, err := a.GetJSON(ctx, "t5"); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("GetJSON(t5) err = %v, want ErrNoTerminal", err)
	}
	if raw, err := a.ListJSON(ctx); err != nil || string(raw) != `{"terminals":[]}` {
		t.Errorf("ListJSON = %q, %v", raw, err)
	}
	if _, complete, err := a.ReadText(ctx, "t1"); err != nil || !complete {
		t.Errorf("ReadText complete = %v, err %v", complete, err)
	}
	want := exec.UnsetLauncherSessionHandles()
	calls := run.Calls()
	if len(calls) != 4 {
		t.Fatalf("%d hc calls, want 4", len(calls))
	}
	for i, c := range calls {
		if len(c.Env) != 1 || !c.Env[0].Equal(want) {
			t.Errorf("hc call %d (%s) does not drop the launcher's session handles", i, f.verbs[i])
		}
		if !c.Path.Equal(exec.Secret("/usr/local/bin/hc")) {
			t.Errorf("hc call %d (%s) does not run the adapter's hc path", i, f.verbs[i])
		}
	}

	f.lsFail = true
	_, err = a.ListJSON(ctx)
	if !errors.Is(err, ErrQueryFailed) || errors.Is(err, ErrNoTerminal) || strings.Contains(err.Error(), "internal") {
		t.Errorf("a failed ls err = %v, want ErrQueryFailed without hc's stderr", err)
	}
}

// TestQueries_CutOutput: a JSON answer over the cap is a failure, never a
// parse of half a document; a screen over the cap comes back marked cut.
func TestQueries_CutOutput(t *testing.T) {
	over := func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
			Stdout: exec.BoundedOutputForTest([]byte(`{"terminals":[`), exec.OutputOverflowed),
			Stderr: exec.BoundedOutputForTest(nil, exec.OutputComplete),
		}, nil
	}
	a, err := New(&exec.FakeSensitiveRunner{RunFunc: over}, "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ListJSON(context.Background()); !errors.Is(err, ErrQueryFailed) {
		t.Errorf("ListJSON over the cap err = %v, want ErrQueryFailed", err)
	}
	if _, complete, err := a.ReadText(context.Background(), "t1"); err != nil || complete {
		t.Errorf("ReadText over the cap: complete %v, err %v; want a cut screen and no error", complete, err)
	}
}

// recordingStreamer records the command StreamEvents runs.
type recordingStreamer struct{ argv []string }

func (r *recordingStreamer) RunStreamingScrubbed(_ context.Context, stdout, _ io.Writer, name string, args ...string) error {
	r.argv = append([]string{name}, args...)
	_, err := io.WriteString(stdout, "{}\n")
	return err
}

func TestStreamEventsRunsTheAdaptersHc(t *testing.T) {
	a, err := New((&fakeHC{}).runner(), "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := &recordingStreamer{}
	if err := a.StreamEvents(context.Background(), s, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.argv, " "); got != "/usr/local/bin/hc events --json" {
		t.Errorf("ran %q, want the adapter's hc path with events --json", got)
	}
	if out.String() != "{}\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if err := a.StreamEvents(context.Background(), nil, &out); !errors.Is(err, ErrQueryFailed) {
		t.Errorf("StreamEvents with no streamer err = %v", err)
	}
}
