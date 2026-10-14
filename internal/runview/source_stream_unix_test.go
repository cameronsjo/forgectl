// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"context"
	"os"
	"os/exec"
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

// gone waits for pid to exit (or be a zombie no one reaped yet) and reports
// whether it did within a few seconds.
func gone(t *testing.T, pid string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", pid).Output() //nolint:gosec // G204: a pid the test read
		if st := strings.TrimSpace(string(out)); st == "" || strings.HasPrefix(st, "Z") {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// readPid waits for a command to write its child's pid to path.
func readPid(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp file
		if err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the command never wrote its child's pid")
	return ""
}

// Cancelling the context (a signal, in desk show) stops the command's whole
// process group, not only the shell.
func TestCommandSourceStopsTheGroupWhenTheContextEnds(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(t.Context())
	s, err := NewCommandSource(ctx, `sleep 1000 & echo $! > `+pidFile+`; wait`, false, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	pid := readPid(t, pidFile)
	cancel()
	waitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	if err := s.Wait(waitCtx); err != nil {
		t.Fatalf("the source did not end after its context did: %v", err)
	}
	if !gone(t, pid) {
		t.Errorf("the command's child %s outlived the context", pid)
	}
}

// A descendant that ignores SIGTERM and holds stdout open cannot hang
// Close: the group gets SIGKILL after the grace.
func TestCommandSourceCloseKillsATermIgnoringChild(t *testing.T) {
	old := streamStopGrace
	streamStopGrace = 100 * time.Millisecond
	t.Cleanup(func() { streamStopGrace = old })
	pidFile := filepath.Join(t.TempDir(), "pid")
	s, err := NewCommandSource(t.Context(), `trap '' TERM; sleep 1000 & echo $! > `+pidFile+`; wait`, true, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	pid := readPid(t, pidFile)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a command that ignores SIGTERM")
	}
	if !gone(t, pid) {
		t.Errorf("the TERM-ignoring child %s survived Close", pid)
	}
}

// Once the lens read the run's end, the command is not restarted.
func TestCommandSourceDoesNotRestartAnEndedRun(t *testing.T) {
	old := streamRestartMin
	streamRestartMin = 10 * time.Millisecond
	t.Cleanup(func() { streamRestartMin = old })
	lens, err := ParseLens("app", []byte("format = \"text\"\n[[rule]]\naction = \"end\"\nmatch = '^done rc=(?P<exit>\\d+)'\n"))
	if err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "n")
	s, err := NewCommandSource(t.Context(), `echo x >> `+counter+`; echo 'done rc=0'`, true, lens, LogKeys{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("an ended run's command was still being restarted: %v", err)
	}
	b, _ := os.ReadFile(counter) //nolint:gosec // G304: the test's own temp file
	if strings.Count(string(b), "x") != 1 {
		t.Errorf("the command ran %d times, want once", strings.Count(string(b), "x"))
	}
}

// A command that keeps exiting with no output is given up on.
func TestCommandSourceGivesUpOnAQuietCommand(t *testing.T) {
	oldMin, oldQuiet := streamRestartMin, streamQuietRestarts
	streamRestartMin, streamQuietRestarts = time.Millisecond, 3
	t.Cleanup(func() { streamRestartMin, streamQuietRestarts = oldMin, oldQuiet })
	s, err := NewCommandSource(t.Context(), `exit 2`, true, nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := streamEvents(t, s)
	if d.Err == nil || !strings.Contains(d.Err.Error(), "not restarted: 3 runs in a row wrote nothing") {
		t.Errorf("err = %v, want the give-up note", d.Err)
	}
}

// The cap: a chunk that straddles it keeps the part below it, a capped
// stream's cut line is not completed, and a restart's output resets it.
func TestStreamSourceCap(t *testing.T) {
	s, err := newStreamSource("t", nil, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	s.limit = 20
	if _, err := s.pump(strings.NewReader("{\"event\":\"a\"}\n{\"event\":\"b\"}\n"), false); err != nil {
		t.Fatal(err)
	}
	s.endLine()
	if len(s.buf) != 20 || !s.capped || s.buf[19] == '\n' {
		t.Fatalf("buf = %q (capped %v), want the first 20 bytes, capped, with no newline added", s.buf, s.capped)
	}
	d, _ := s.Load(RunRef{}, &Cursor{})
	if !d.Partial || len(d.Events) != 1 {
		t.Errorf("a capped stream: partial=%v events=%d, want partial and the one whole line", d.Partial, len(d.Events))
	}
	if _, err := s.pump(strings.NewReader("{\"event\":\"c\"}\n"), true); err != nil {
		t.Fatal(err)
	}
	if s.capped || s.gen != 1 || string(s.buf) != "{\"event\":\"c\"}\n" {
		t.Errorf("after a replacing run: buf=%q capped=%v gen=%d, want the new run alone, uncapped", s.buf, s.capped, s.gen)
	}
}

func TestCommandName(t *testing.T) {
	for cmd, want := range map[string]string{
		"kubectl logs -f deploy/app --token=secret": "kubectl",
		"FOO=1 /usr/bin/ssh host tail -F x":         "ssh",
		"A=1":                                       "command",
	} {
		if got := commandName(cmd); got != want {
			t.Errorf("commandName(%q) = %q, want %q", cmd, got, want)
		}
	}
}
