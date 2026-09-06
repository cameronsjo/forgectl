package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// TestPlainVerbsEmitNoEscapesWhenPiped is the behavioural half of the colour
// boundary; colorout_test.go is the structural half.
//
// The structural guard reads syntax, so it cannot see a writer reached through
// a struct field, a helper's return value, or a shadowed name — its own doc
// comment lists those gaps. This runs the commands and looks at the bytes,
// which has no such blind spot, at the cost of only covering verbs that can be
// driven with a fake runner.
//
// Three commands leaked into pipes during this migration (ghostty cheat at 117
// escape sequences, tmux cheat at 23, bench status at 2), so this is a measured
// failure mode rather than a theoretical one.
func TestPlainVerbsEmitNoEscapesWhenPiped(t *testing.T) {
	// A destination that is not a terminal, with no force flag: the writer
	// must resolve to no colour at all.
	deps := module.Deps{
		Runner: &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }},
		Theme:  theme.Default(),
	}

	verbs := [][]string{
		{"doctor"},
		{"launch", "which"},
		{"launch", "doctor"},
		{"bench", "status"},
		{"tmux", "cheat"},
		{"ghostty", "cheat"},
		{"theme", "show"},
		{"theme", "preview"},
		{"review"},
	}

	for _, argv := range verbs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("CLICOLOR_FORCE", "")

			root := newRoot(deps)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(argv)
			// The error is deliberately ignored: several of these exit
			// non-zero against a fake runner (doctor finds problems, bench
			// finds no services). What is under test is the BYTES, and a
			// command that failed still printed.
			_ = root.Execute()

			if i := strings.IndexByte(stdout.String(), 0x1b); i >= 0 {
				t.Errorf("%s emitted an escape at stdout byte %d; it must be plain when not a terminal\n%q",
					strings.Join(argv, " "), i, stdout.String())
			}
		})
	}
}

// TestPipedOutputProbeCanFail is the positive control for the table above.
//
// Every assertion there is "no escape appeared". If the harness could not
// produce one — a fake runner that renders nothing, a root that never runs —
// the whole table would pass while testing nothing. This forces colour on and
// requires it to show up.
func TestPipedOutputProbeCanFail(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("TERM", "xterm-256color")

	deps := module.Deps{
		Runner: &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }},
		Theme:  theme.Default(),
	}
	root := newRoot(deps)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"theme", "preview"})
	_ = root.Execute()

	if !strings.ContainsRune(stdout.String(), 0x1b) {
		t.Fatal("forced colour produced no escape; the table above cannot go red and proves nothing")
	}
}
