package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/fang"
	cterm "github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// fang v1.0.0 asks the terminal for its background (an OSC 11 and a DA1
// query) every time it renders help or an error to a stdout TTY, and has no
// option to skip it (#546; fang v2 is the same). Where forgectl's own policy
// refuses a probe — a multiplexer that swallows the reply, NO_COLOR,
// TERM=dumb, or an explicit [theme] mode — the query bytes land in the
// output, and a terminal that never answers stalls each render for about
// 4.5 s.
//
// The probe sits in lipgloss.HasDarkBackground, which on Unix asks only when
// both stdin and stdout are terminals. So fangExecute runs fang's own help
// renderer and error styles with os.Stdin pointed at /dev/null for the length
// of the render, and nothing else changes: the same styled frames, the same
// handler, no query. forgectl already ignores the answer in these
// environments (fangTrustsProbe), so the colors are what they were.

// fangProbeUnsafe reports whether fang would query the terminal where
// forgectl's policy refuses it, for the configured theme mode. A seam, so
// tests can force either answer.
var fangProbeUnsafe = func(mode theme.Mode) bool {
	return fangProbeUnsafeFor(cterm.IsTerminal(os.Stdout.Fd()), mode, currentProbeEnv())
}

// fangProbeUnsafeFor is the pure decision behind fangProbeUnsafe. With stdin
// not a terminal lipgloss never asks, so there is nothing to suppress.
func fangProbeUnsafeFor(stdoutTTY bool, mode theme.Mode, env theme.Env) bool {
	return stdoutTTY && env.StdinTTY && !probeAllowed(mode, env)
}

// probeAllowed is forgectl's policy for a background query from the CLI:
// theme.ShouldProbe, and not TERM=dumb, which cannot answer either (#1152).
func probeAllowed(mode theme.Mode, env theme.Env) bool {
	return theme.ShouldProbe(mode, env) && !tui.DumbTerminal(env.Term)
}

// currentProbeEnv reads the process's terminal facts for the probe policy.
func currentProbeEnv() theme.Env {
	return theme.Env{
		StdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		Term:      os.Getenv("TERM"),
		NoColor:   os.Getenv("NO_COLOR") != "",
	}
}

// fangExecute is fang.Execute, minus the background query where forgectl's
// policy refuses one. Every fang dispatch goes through it, with the same theme
// its options were built from.
func fangExecute(ctx context.Context, root *cobra.Command, th theme.Theme, opts ...fang.Option) error {
	if !fangProbeUnsafe(th.Mode()) {
		return fang.Execute(ctx, root, opts...)
	}
	return executeWithoutProbe(ctx, root, opts...)
}

// errFangSetup is what the throwaway command returns so fang hands its error
// styles to the capturing handler.
var errFangSetup = errors.New("fang setup")

// executeWithoutProbe does what fang.Execute does (help function, version,
// silenced cobra output, the man command, styled error frame) with fang's
// own pieces, taken from a throwaway run with no terminal on stdin. It then
// runs root itself, so no fang render ever sees a terminal on stdin.
func executeWithoutProbe(ctx context.Context, root *cobra.Command, opts ...fang.Option) error {
	var styles fang.Styles
	setup := &cobra.Command{
		Use:  root.Use,
		RunE: func(*cobra.Command, []string) error { return errFangSetup },
	}
	setup.SetArgs([]string{}) // empty, not nil: nil makes cobra read os.Args
	setup.SetOut(io.Discard)
	setup.SetErr(io.Discard)
	capture := fang.WithErrorHandler(func(_ io.Writer, s fang.Styles, _ error) { styles = s })
	withoutTerminalStdin(func() {
		_ = fang.Execute(ctx, setup, append(slices.Clone(opts), capture)...)
	})

	// fang's help function renders whatever command cobra hands it; only its
	// colour lookup reads stdin, so it runs with stdin swapped too.
	help := setup.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		withoutTerminalStdin(func() { help(c, args) })
	})
	root.Version = setup.Version
	root.SilenceUsage = true
	root.SilenceErrors = true
	if man, _, err := setup.Find([]string{"man"}); err == nil && man != setup {
		setup.RemoveCommand(man)
		root.AddCommand(man)
	}

	if err := root.ExecuteContext(ctx); err != nil {
		termsafeErrorHandler(colorprofile.NewWriter(root.ErrOrStderr(), os.Environ()), styles, err)
		return err //nolint:wrapcheck // returned as is, the way fang.Execute does
	}
	return nil
}

// withoutTerminalStdin runs fn with os.Stdin pointed at the null device, then
// puts it back. If the null device cannot be opened, fn runs as it is: a
// stray query is better than a lost help page.
func withoutTerminalStdin(fn func()) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		fn()
		return
	}
	prev := os.Stdin
	os.Stdin = null
	defer func() {
		os.Stdin = prev
		_ = null.Close()
	}()
	fn()
}
