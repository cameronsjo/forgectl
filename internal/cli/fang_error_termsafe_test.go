package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
)

// TestFangErrorSinkEmitsNothingUnsafe pins the sink that renders every error
// the cobra tree returns.
//
// This is the sink two prior security reviews assumed was covered and neither
// examined. fang.DefaultErrorHandler prints err.Error() with no filtering of
// its own, and a tmux-derived error carries text forgectl never composed — an
// *exec.CommandError concatenates raw tmux stderr, which echoes the session
// name it was given.
//
// The assertion runs fang.Execute against a real command so it proves the
// handler is WIRED, not merely that a sanitizing function exists somewhere.
func TestFangErrorSinkEmitsNothingUnsafe(t *testing.T) {
	root := &cobra.Command{
		Use:          "forgectl",
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("no such session: " + termsafetest.Hostile("work"))
		},
	}
	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs(nil)

	if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef")...); err == nil {
		t.Fatal("expected the command to fail; the check would pass vacuously")
	}
	if stderr.Len() == 0 {
		t.Fatal("fang wrote no error text; the check would pass vacuously")
	}
	termsafetest.AssertInert(t, "fang error sink", stderr.String())
	// Lowercased before matching: fang's own error style sentence-cases the
	// message, so "no such session" arrives as "No such session".
	if !strings.Contains(strings.ToLower(stderr.String()), "no such session") {
		t.Errorf("fang error sink lost the legible message: %q", stderr.String())
	}
}

// TestFangErrorSinkKeepsUsageDetection guards the one behavior sanitizing could
// have broken. fang decides whether to print "Try --help for usage" by matching
// prefixes against err.Error(), so rewriting the message must leave those
// prefixes byte-identical or an unknown flag stops offering the hint.
func TestFangErrorSinkKeepsUsageDetection(t *testing.T) {
	root := &cobra.Command{Use: "forgectl", SilenceUsage: true, RunE: func(*cobra.Command, []string) error { return nil }}
	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs([]string{"--nope"})

	if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef")...); err == nil {
		t.Fatal("expected an unknown-flag failure")
	}
	// The hint itself is the evidence: fang prints it only when its prefix
	// match against err.Error() recognized a usage error, so its presence
	// proves sanitizing left that prefix intact.
	if !strings.Contains(stderr.String(), "Try --help for usage") {
		t.Errorf("usage hint gone — sanitizing broke fang's prefix match: %q", stderr.String())
	}
}

func TestFangErrorSinkKeepsOrdinarySingleLineErrors(t *testing.T) {
	root := &cobra.Command{
		Use:          "forgectl",
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("plain failure")
		},
	}
	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs(nil)

	if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef")...); err == nil {
		t.Fatal("expected the command to fail")
	}
	out := stderr.String()
	if !strings.Contains(out, "Plain failure.") {
		t.Errorf("ordinary error changed: %q", out)
	}
	if strings.Contains(out, "Try --help for usage") {
		t.Errorf("ordinary error was misclassified as usage: %q", out)
	}
	termsafetest.AssertInert(t, "ordinary fang error", out)
}

func TestFangErrorSinkPreservesSafeSuggestionStructure(t *testing.T) {
	root := &cobra.Command{
		Use:          "forgectl",
		Args:         safeRootArgs,
		RunE:         showRootHelp,
		SilenceUsage: true,
	}
	root.SuggestionsMinimumDistance = 2
	root.AddCommand(&cobra.Command{Use: "tmux", Run: func(*cobra.Command, []string) {}})

	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs([]string{"tmuxx"})

	err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef")...)
	if err == nil {
		t.Fatal("expected an unknown-command failure")
	}
	if want := "unknown command \"tmuxx\" for \"forgectl\"\n\nDid you mean this?\n  tmux"; err.Error() != want {
		t.Fatalf("structured error = %q, want %q", err.Error(), want)
	}
	out := stderr.String()
	if !strings.Contains(out, "Unknown command") || !strings.Contains(out, "Did you mean this?") || !strings.Contains(out, "tmux") {
		t.Fatalf("structured suggestion lost readable fields: %q", out)
	}
	if strings.Contains(out, `\\n\\nDid you mean`) {
		t.Fatalf("trusted suggestion layout was flattened: %q", out)
	}
	if strings.Index(out, "Did you mean this?") <= strings.Index(out, "Unknown command") {
		t.Fatalf("trusted suggestion structure is out of order: %q", out)
	}
	if !strings.Contains(out, "Try --help for usage") {
		t.Fatalf("structured unknown-command error lost fang usage detection: %q", out)
	}
	termsafetest.AssertInert(t, "structured fang suggestion", out)
}

func TestFangErrorSinkQuotesHostileSuggestionFields(t *testing.T) {
	unknown := "tmuxx\nFORGED ERROR\x1b[2K\u202e"
	suggestion := "tmux\rFORGED-SUGGESTION\x1b[31m\u2066"
	root := &cobra.Command{
		Use:          "forge\x1bctl\u202e",
		Args:         safeRootArgs,
		RunE:         showRootHelp,
		SilenceUsage: true,
	}
	root.AddCommand(&cobra.Command{Use: suggestion, SuggestFor: []string{unknown}, Run: func(*cobra.Command, []string) {}})

	var stderr bytes.Buffer
	root.SetOut(new(bytes.Buffer))
	root.SetErr(&stderr)
	root.SetArgs([]string{unknown})

	if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef")...); err == nil {
		t.Fatal("expected an unknown-command failure")
	}
	out := stderr.String()
	for _, escaped := range []string{`tmuxx\nFORGED ERROR\x1b[2K\u202e`, `forge\x1bctl\u202e`, `tmux\rFORGED-SUGGESTION\x1b[31m\u2066`} {
		if !strings.Contains(out, escaped) {
			t.Errorf("structured error did not visibly quote %q: %q", escaped, out)
		}
	}
	for _, forgedLine := range []string{"\nFORGED ERROR", "\nFORGED-SUGGESTION"} {
		if strings.Contains(out, forgedLine) {
			t.Errorf("untrusted field created a fake physical line %q in %q", forgedLine, out)
		}
	}
	termsafetest.AssertInert(t, "hostile structured fang suggestion", out)
}
