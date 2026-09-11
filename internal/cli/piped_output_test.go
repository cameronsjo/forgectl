package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/ghostty"
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
	{"theme", "show"},
	{"theme", "preview"},
	// Two verbs are deliberately absent, both caught by the per-verb positive
	// control below rather than by reading the list.
	//
	// `review`'s styled path needs review.Source fixtures, not a runner, so
	// through this root-level harness it renders no rows and therefore no
	// colour. Its dimming is covered by TestReviewCmd_Table_DimsReviewedRow,
	// which builds the sources it needs.
	//
	// `ghostty cheat` is host-dependent in a way this harness cannot fake.
	// newRoot builds its client as ghostty.New(deps.Runner), and the client
	// resolves the real ghostty binary through lookPath/stat before it ever
	// reaches the Runner — so on a machine without ghostty the command errors
	// out and prints nothing. It passed here on the author's Mac and went red
	// on both CI runners, which is the shape a local-only green always takes.
	// Covered instead by TestGhosttyCheat_PipedAndForced below, which injects
	// the client directly.
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

// TestGhosttyCheat_PipedAndForced covers the verb pipedVerbs cannot reach.
//
// It is the verb that leaked hardest during this migration — 117 escape
// sequences into a pipe — so dropping it from the table without a replacement
// would have retired the assertion that caught the leak. Building the command
// directly lets the ghostty client be faked, which removes the host dependency
// the table has no way around.
//
// Both directions in one test, for the same reason the table carries a
// positive control: the piped half alone passes on a command that prints
// nothing, which is exactly how the host dependency hid.
func TestGhosttyCheat_PipedAndForced(t *testing.T) {
	client := ghostty.New(
		&exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
			if len(args) > 0 && args[0] == "+list-keybinds" {
				return "keybind = ctrl+shift+t=new_tab\nkeybind = ctrl+shift+w=close_surface\n", nil
			}
			return "", nil
		}},
		// WithBin skips the lookPath/stat resolution that made this verb
		// host-dependent in the first place.
		ghostty.WithBin("ghostty"),
	)

	run := func(t *testing.T, force bool) string {
		t.Helper()
		t.Setenv("NO_COLOR", "")
		if force {
			t.Setenv("CLICOLOR_FORCE", "1")
			t.Setenv("TERM", "xterm-256color")
		} else {
			t.Setenv("CLICOLOR_FORCE", "")
		}

		cmd := newGhosttyCmd(client, theme.Default())
		// The real root owns --no-icons; the subtree read it as a persistent
		// flag, so supply it here rather than reaching for newRoot.
		cmd.PersistentFlags().Bool("no-icons", false, "")
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stdout)
		cmd.SetArgs([]string{"cheat"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ghostty cheat: %v", err)
		}
		if stdout.Len() == 0 {
			t.Fatal("ghostty cheat printed nothing; neither half of this test could have failed")
		}
		return stdout.String()
	}

	t.Run("piped emits no escape", func(t *testing.T) {
		if i := strings.IndexByte(run(t, false), 0x1b); i >= 0 {
			t.Errorf("escape at byte %d; ghostty cheat must be plain when not a terminal", i)
		}
	})
	t.Run("forced emits colour", func(t *testing.T) {
		if !strings.ContainsRune(run(t, true), 0x1b) {
			t.Error("no escape under CLICOLOR_FORCE; the piped assertion above proves nothing")
		}
	})
}
