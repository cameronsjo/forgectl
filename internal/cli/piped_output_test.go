package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// pipedVerbs are the plain-output commands a fake runner can drive. Both the
// absence table and its positive control walk this same list, so a verb cannot
// be asserted clean without also being shown capable of colour.
var pipedVerbs = [][]string{
	{"doctor"},
	{"launch", "which"},
	{"launch", "doctor"},
	{"bench", "status"},
	{"tmux", "cheat"},
	{"ghostty", "cheat"},
	{"theme", "show"},
	{"theme", "preview"},
	// `review` is deliberately absent. Its styled path needs review.Source
	// fixtures, not a runner, so through this root-level harness it renders no
	// rows and therefore no colour — its clean run would have asserted nothing.
	// The per-verb positive control below is what caught that; the verb's own
	// dimming is covered directly by TestReviewCmd_Table_DimsReviewedRow, which
	// builds the sources it needs.
}

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

	for _, argv := range pipedVerbs {
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

// TestPipedOutputProbeCanFail is the positive control for the table above,
// run PER VERB rather than once.
//
// Every assertion up there is "no escape appeared", which a verb that renders
// nothing at all satisfies for the wrong reason — a fake runner returning
// empty, a subcommand that bailed before printing. Running one verb forced and
// generalising would have left the other eight assumed. This measures each of
// them: under CLICOLOR_FORCE every verb in the table must produce colour, so
// its clean run above means something.
func TestPipedOutputProbeCanFail(t *testing.T) {
	deps := module.Deps{
		Runner: &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }},
		Theme:  theme.Default(),
	}

	for _, argv := range pipedVerbs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("CLICOLOR_FORCE", "1")
			t.Setenv("TERM", "xterm-256color")

			root := newRoot(deps)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(argv)
			_ = root.Execute()

			if !strings.ContainsRune(stdout.String(), 0x1b) {
				t.Errorf("%s produced no escape under CLICOLOR_FORCE; its clean run in the table above proves nothing\n%q",
					strings.Join(argv, " "), stdout.String())
			}
		})
	}
}
