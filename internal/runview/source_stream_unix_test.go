// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// streamEvents waits for the stream to end and loads it once.
func streamEvents(t *testing.T, s *StreamSource) Delta {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("the stream did not end: %v", err)
	}
	refs, err := s.List()
	if err != nil || len(refs) != 1 {
		t.Fatalf("List = %v, %v", refs, err)
	}
	d, err := s.Load(refs[0], &Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func eventNames(d Delta) []string {
	var out []string
	for _, e := range d.Events {
		out = append(out, e.Name)
	}
	return out
}

func TestReaderSourceReadsAStreamToItsEnd(t *testing.T) {
	// The last line has no newline: the stream is over, so it is read, not held.
	in := strings.NewReader("{\"event\":\"a\"}\nnot json\n{\"event\":\"b\"}")
	s, err := NewReaderSource("stdin", in, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	d := streamEvents(t, s)
	if got := strings.Join(eventNames(d), ","); got != "a,b" {
		t.Errorf("events = %q, want a,b", got)
	}
	if d.Dropped != 1 || d.Held || d.Err != nil {
		t.Errorf("dropped=%d held=%v err=%v, want 1, false, nil", d.Dropped, d.Held, d.Err)
	}
}

func TestReaderSourceReadsThroughALens(t *testing.T) {
	lens, err := ParseLens("app", []byte(`format = "text"
[[step]]
id = "build"
[[rule]]
action = "start"
match = '^building$'
step = "build"
[[rule]]
action = "close"
match = '^built$'
step = "build"
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewReaderSource("stdin", strings.NewReader("building\nbuilt\n"), lens, LogKeys{})
	if err != nil {
		t.Fatal(err)
	}
	d := streamEvents(t, s)
	f := NewFolder(s.Spec(RunRef{}), d.Defs)
	f.Append(d.Events...)
	st := f.At(f.Len())
	if len(st.Steps) != 1 || st.Steps[0].Status != StepClosed {
		t.Errorf("steps = %+v, want build closed", st.Steps)
	}
}

func TestReaderSourceLoadsIncrementally(t *testing.T) {
	s, err := NewReaderSource("stdin", strings.NewReader("{\"event\":\"a\"}\n"), nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	_ = streamEvents(t, s)
	cur := &Cursor{}
	if d, _ := s.Load(RunRef{}, cur); len(d.Events) != 1 {
		t.Fatalf("first load = %d events, want 1", len(d.Events))
	}
	if d, _ := s.Load(RunRef{}, cur); len(d.Events) != 0 || d.Reset {
		t.Errorf("second load = %d events, reset %v; want nothing new", len(d.Events), d.Reset)
	}
}

func TestCommandSourceReadsAnExitedCommand(t *testing.T) {
	s, err := NewCommandSource(t.Context(), `printf '{"event":"a"}\n{"event":"b"}\n'`, false, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := streamEvents(t, s)
	if got := strings.Join(eventNames(d), ","); got != "a,b" || d.Err != nil {
		t.Errorf("events = %q, err = %v; want a,b and no error", got, d.Err)
	}
}

func TestCommandSourceSaysWhyTheCommandFailed(t *testing.T) {
	s, err := NewCommandSource(t.Context(), `echo '{"event":"a"}'; echo 'pod not found' >&2; exit 3`, false, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := streamEvents(t, s)
	if len(d.Events) != 1 {
		t.Errorf("events = %v, want the one line written before the failure", eventNames(d))
	}
	if d.Err == nil || !strings.Contains(d.Err.Error(), "exit status 3") || !strings.Contains(d.Err.Error(), "pod not found") {
		t.Errorf("err = %v, want the exit status and the stderr line", d.Err)
	}
}

func TestCommandSourceRestartsAndReplacesTheRun(t *testing.T) {
	old := streamRestartMin
	streamRestartMin = 10 * time.Millisecond
	t.Cleanup(func() { streamRestartMin = old })
	counter := filepath.Join(t.TempDir(), "n")
	cmd := `n=$(cat ` + counter + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + counter + `; echo "{\"event\":\"run$n\"}"; [ $n -ge 2 ] && sleep 30`
	s, err := NewCommandSource(t.Context(), cmd, true, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cur := &Cursor{}
	var seen []string
	reset := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := s.Load(RunRef{}, cur)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reset {
			reset = true
			seen = nil
		}
		seen = append(seen, eventNames(d)...)
		if strings.Join(seen, ",") == "run2" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !reset || strings.Join(seen, ",") != "run2" {
		t.Fatalf("after a restart: reset=%v events=%v; want the second run alone, after a reset", reset, seen)
	}
	start := time.Now()
	s.Close()
	if time.Since(start) > 5*time.Second {
		t.Errorf("Close took %s to stop a running command", time.Since(start))
	}
}

func TestCommandSourceRefusesAnEmptyCommand(t *testing.T) {
	if _, err := NewCommandSource(t.Context(), "  ", true, nil, DefaultLogKeys); err == nil {
		t.Error("an empty command was accepted")
	}
}

func TestLastLineKeepsTheLastNonBlankLine(t *testing.T) {
	var l lastLine
	_, _ = l.Write([]byte("first\nsecond\n\n"))
	if got := l.String(); got != "second" {
		t.Errorf("lastLine = %q, want second", got)
	}
	_, _ = l.Write([]byte("third, no newline"))
	if got := l.String(); got != "third, no newline" {
		t.Errorf("lastLine = %q, want the unfinished line", got)
	}
}
