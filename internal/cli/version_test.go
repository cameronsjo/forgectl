package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func TestVersionCmd_PrintsRootVersion(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.Version = "1.2.3 (abcdef0)"
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute() error = %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "forgectl version 1.2.3 (abcdef0)"
	if got != want {
		t.Errorf("version command output = %q, want %q", got, want)
	}
}

// TestVersion_VerbMatchesFlagThroughFang is a regression guard: the bare
// `version` verb (version.go) and fang's injected `--version` flag must stay
// byte-identical, since version.go reads cmd.Root().Version rather than
// duplicating fang's format. Runs both through the real fang.Execute path
// (not root.Execute directly) using the same fangVersionOptions execCommand
// calls — not a hand-rolled copy — so an option added to that wiring later
// is exercised by this guard too. Each invocation gets a fresh root: fang
// registers a `man` subcommand every call, so reusing one root across calls
// double-registers it and errors.
func TestVersion_VerbMatchesFlagThroughFang(t *testing.T) {
	const wantSuffix = "forgectl version 9.9.9 (abcdef0)"

	runFang := func(t *testing.T, arg string) string {
		t.Helper()
		root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs([]string{arg})

		err := fang.Execute(context.Background(), root, fangVersionOptions("9.9.9", "abcdef0123456")...)
		if err != nil {
			t.Fatalf("fang.Execute(%q) error = %v", arg, err)
		}
		return strings.TrimSpace(buf.String())
	}

	verbOut := runFang(t, "version")
	flagOut := runFang(t, "--version")

	// Each output must carry the fang-populated commit suffix — proves
	// fang's version-string overwrite actually ran via the real Execute
	// path, not just that a hardcoded string happens to match.
	if verbOut != wantSuffix {
		t.Errorf("version verb output = %q, want %q", verbOut, wantSuffix)
	}
	if flagOut != wantSuffix {
		t.Errorf("--version flag output = %q, want %q", flagOut, wantSuffix)
	}

	// The parity guard itself: the two surfaces must agree, checked
	// directly against each other rather than transitively through a
	// shared constant.
	if verbOut != flagOut {
		t.Errorf("version verb ⇄ --version parity broken: %q vs %q", verbOut, flagOut)
	}
}
