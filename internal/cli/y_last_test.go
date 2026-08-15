package cli

// Test plan for y.go's `last` subcommand
//
// newYLastCmd (Classification: API handler / cobra command over untrusted input)
//   [x] Happy: `y last` prints the single most recent command
//   [x] Happy: `y last 3` prints three commands, oldest first
//   [x] Happy: the `l` alias resolves to last
//   [x] Security: control and bidi characters in history are escaped at the sink
//   [x] Fail-closed: an absent $HISTFILE errors and prints nothing
//   [x] Fail-closed: a malformed history errors and prints nothing
//   [x] Fail-closed: a non-numeric or non-positive count errors
//   [x] Fail-closed: a failing stdout surfaces rather than truncating quietly

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// seedHistory writes a history file and points $HISTFILE at it.
func seedHistory(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".zsh_history")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	t.Setenv("HISTFILE", path)
	return path
}

// runYLast executes `y last` with the given args and returns stdout.
func runYLast(t *testing.T, args ...string) (string, error) {
	t.Helper()
	client := clippkg.New(&exec.FakeRunner{}, clippkg.WithGOOS("darwin"))
	cmd := newYCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(append([]string{"last"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

func TestYLastCmd_PrintsMostRecentCommand(t *testing.T) {
	seedHistory(t, ": 1690000000:0;echo one\n: 1690000060:0;echo two\n")

	stdout, err := runYLast(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := stdout, "echo two\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestYLastCmd_PrintsCountOldestFirst(t *testing.T) {
	seedHistory(t, "echo one\necho two\necho three\necho four\n")

	stdout, err := runYLast(t, "3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "echo two\necho three\necho four\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestYLastCmd_EscapesTerminalControlsFromHistory(t *testing.T) {
	// Built from code points so this source file holds no control or bidi
	// character of its own.
	esc, rlo := string(rune(0x1B)), string(rune(0x202E))
	seedHistory(t, ": 1690000000:0;echo "+esc+"[2J"+rlo+"txt.exe\n")

	stdout, err := runYLast(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.ContainsAny(stdout, esc+rlo) {
		t.Fatal("y last wrote a raw terminal control from history to stdout")
	}
	// The expected renderings are spelled as ASCII text, never as the
	// characters themselves, so this file stays free of both.
	wantEscapes := []string{"\\x1b", "\\u202e"}
	for _, want := range wantEscapes {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain the escape %s", stdout, want)
		}
	}
	if lines := strings.Count(stdout, "\n"); lines != 1 {
		t.Errorf("stdout spans %d lines, want 1 — one entry must stay one physical line", lines)
	}
}

func TestYLastCmd_MultilineCommandStaysOneLine(t *testing.T) {
	seedHistory(t, ": 1690000000:0;for f in a b\\\ndo echo $f\\\ndone\n")

	stdout, err := runYLast(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout = %q, want a single physical line", stdout)
	}
}

func TestYLastCmd_FailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		args     []string
		absent   bool
	}{
		{name: "absent history", absent: true},
		{name: "empty history", contents: ""},
		{name: "malformed history", contents: ": nope:0;echo one\n"},
		{name: "truncated history", contents: ": 1690000000:0;echo one\\\n"},
		{name: "non-numeric count", contents: "echo one\n", args: []string{"three"}},
		{name: "zero count", contents: "echo one\n", args: []string{"0"}},
		// "--" so cobra hands "-2" through as an argument rather than
		// rejecting it as an unknown shorthand flag — the refusal under test
		// is the count check, not flag parsing.
		{name: "negative count", contents: "echo one\n", args: []string{"--", "-2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.absent {
				t.Setenv("HISTFILE", filepath.Join(t.TempDir(), "nope"))
			} else {
				seedHistory(t, tc.contents)
			}
			stdout, err := runYLast(t, tc.args...)
			if err == nil {
				t.Fatalf("y last returned no error and printed %q; this input must refuse", stdout)
			}
			if stdout != "" {
				t.Errorf("y last printed %q alongside a refusal", stdout)
			}
		})
	}
}

// errWriter fails every write, standing in for a closed pipe.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

func TestYLastCmd_SurfacesWriteFailures(t *testing.T) {
	seedHistory(t, "echo one\necho two\n")

	client := clippkg.New(&exec.FakeRunner{}, clippkg.WithGOOS("darwin"))
	cmd := newYCmdForClient(client)
	cmd.SetOut(errWriter{})
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"last", "2"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("y last reported success while every write to stdout failed")
	}
}

func TestYLastCmd_AliasResolves(t *testing.T) {
	client := clippkg.New(&exec.FakeRunner{}, clippkg.WithGOOS("darwin"))
	cmd := newYCmdForClient(client)

	found, _, err := cmd.Find([]string{"l"})
	if err != nil {
		t.Fatalf(`Find("l"): %v`, err)
	}
	if found.Name() != "last" {
		t.Errorf(`alias "l" resolved to %q, want "last"`, found.Name())
	}
}
