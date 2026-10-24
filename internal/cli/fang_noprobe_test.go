package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func TestFangProbeUnsafeFor(t *testing.T) {
	tty := func(term string) theme.Env { return theme.Env{StdinTTY: true, StdoutTTY: true, Term: term} }
	cases := []struct {
		name      string
		stdoutTTY bool
		env       theme.Env
		want      bool
	}{
		{"probe allowed", true, tty("xterm-256color"), false},
		{"tmux", true, tty("tmux-256color"), true},
		{"screen", true, tty("screen"), true},
		{"TERM=dumb", true, tty("dumb"), true},
		{"NO_COLOR", true, theme.Env{StdinTTY: true, StdoutTTY: true, Term: "xterm", NoColor: true}, true},
		// fang asks only on a stdout TTY, and lipgloss only with stdin a TTY
		// too, so neither case has a query to stop.
		{"stdout not a tty", false, tty("tmux-256color"), false},
		{"stdin not a tty", true, theme.Env{StdoutTTY: true, Term: "tmux-256color"}, false},
	}
	// An explicit mode is an answer, so fang's query is not wanted there either.
	if !fangProbeUnsafeFor(true, theme.ModeLight, tty("xterm-256color")) {
		t.Error("mode light on a clean tty: got false, want true")
	}
	for _, c := range cases {
		if got := fangProbeUnsafeFor(c.stdoutTTY, theme.ModeAuto, c.env); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWithoutTerminalStdin_SwapsAndRestores(t *testing.T) {
	prev := os.Stdin
	var during *os.File
	withoutTerminalStdin(func() { during = os.Stdin })
	if during == prev {
		t.Fatal("os.Stdin was not swapped during fn")
	}
	if during.Name() != os.DevNull {
		t.Errorf("os.Stdin during fn = %q, want %q", during.Name(), os.DevNull)
	}
	if os.Stdin != prev {
		t.Error("os.Stdin was not restored after fn")
	}
}

// forceFangProbeUnsafe pins fangExecute to the no-probe path.
func forceFangProbeUnsafe(t *testing.T, v bool) {
	t.Helper()
	prev := fangProbeUnsafe
	fangProbeUnsafe = func(theme.Mode) bool { return v }
	t.Cleanup(func() { fangProbeUnsafe = prev })
}

// runBothPaths runs args through plain fang.Execute and through the no-probe
// path, each on a fresh root, and returns both outputs and errors.
func runBothPaths(t *testing.T, args ...string) (fangOut, ownOut string, fangErr, ownErr error) {
	t.Helper()
	run := func(unsafe bool) (string, error) {
		forceFangProbeUnsafe(t, unsafe)
		root := newRoot(module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()})
		trimHelpFrames(root)
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		err := fangExecute(context.Background(), root, theme.Default(), fangOptions("9.9.9", "abcdef0123456", theme.Default())...)
		return buf.String(), err
	}
	fangOut, fangErr = run(false)
	ownOut, ownErr = run(true)
	return fangOut, ownOut, fangErr, ownErr
}

// The no-probe path must render exactly what fang renders: help, a usage
// error, the version, and the man page.
func TestExecuteWithoutProbe_MatchesFang(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"desk", "--help"},
		{"help", "env"},
		{"desk", "status", "--bogus"},
		{"--version"},
		{"version"},
	} {
		fangOut, ownOut, fangErr, ownErr := runBothPaths(t, args...)
		if fangOut != ownOut {
			t.Errorf("%q: output differs\nfang:\n%s\nno-probe:\n%s", args, fangOut, ownOut)
		}
		if ExitCode(fangErr) != ExitCode(ownErr) || (fangErr == nil) != (ownErr == nil) {
			t.Errorf("%q: err = %v (exit %d), want %v (exit %d)", args, ownErr, ExitCode(ownErr), fangErr, ExitCode(fangErr))
		}
	}
}

func TestExecuteWithoutProbe_KeepsTheManCommand(t *testing.T) {
	forceFangProbeUnsafe(t, true)
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}, Theme: theme.Default()})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	// man prints to os.Stdout itself, so check that it is registered rather
	// than running it.
	root.SetArgs([]string{"--version"})
	if err := fangExecute(context.Background(), root, theme.Default(), fangOptions("9.9.9", "abcdef0", theme.Default())...); err != nil {
		t.Fatalf("--version: %v\n%s", err, buf.String())
	}
	if man, _, err := root.Find([]string{"man"}); err != nil || man == root || !man.Hidden {
		t.Errorf("man command = %v, %v; want fang's hidden man command on the root", man, err)
	}
}

// A command's own error reaches forgectl's handler, terminal-safety filter
// included, on the no-probe path too.
func TestExecuteWithoutProbe_RendersCommandErrors(t *testing.T) {
	forceFangProbeUnsafe(t, true)
	boom := errors.New("boom \x1b[31mred")
	root := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return boom }}
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{})
	err := fangExecute(context.Background(), root, theme.Default(), fangOptions("9.9.9", "", theme.Default())...)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	// fang's frame title-cases the first word, so match past it.
	if got := buf.String(); !strings.Contains(got, "oom") || strings.Contains(got, "\x1b") {
		t.Errorf("error output = %q, want the message with the escape byte filtered", got)
	}
}
