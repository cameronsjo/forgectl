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
