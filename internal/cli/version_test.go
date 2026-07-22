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
// (not root.Execute directly) so fang's version-string overwrite — including
// the 7-char commit suffix — actually happens, the same way execute.go's
// execCommand invokes it.
func TestVersion_VerbMatchesFlagThroughFang(t *testing.T) {
	const want = "forgectl version 9.9.9 (abcdef0)"

	outputs := make(map[string]string)
	for _, arg := range []string{"version", "--version"} {
		// Fresh root per call: fang registers a `man` subcommand each
		// invocation, so reusing one root across calls double-registers it
		// and errors.
		root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs([]string{arg})

		err := fang.Execute(context.Background(), root,
			fang.WithVersion("9.9.9"),
			fang.WithCommit("abcdef0123456"),
		)
		if err != nil {
			t.Fatalf("fang.Execute(%q) error = %v", arg, err)
		}

		got := strings.TrimSpace(buf.String())
		if got != want {
			t.Errorf("fang.Execute(%q) output = %q, want %q", arg, got, want)
		}
		outputs[arg] = got
	}

	if outputs["version"] != outputs["--version"] {
		t.Errorf("version verb and --version flag diverged: verb = %q, flag = %q",
			outputs["version"], outputs["--version"])
	}
}
