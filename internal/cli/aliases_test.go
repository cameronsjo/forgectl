package cli

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/forgive"
)

// TestApplyAliases_SkipsDashAndSelfName pins the two skip rules the shared
// helper carries: "-" (tmux last's argv-only shorthand — not a valid
// standalone Cobra command name) and a self-alias. TmuxAliases["last"] is
// {"-"} exactly, so last must end up with NO Cobra aliases — the "-" spelling
// stays argv-only (dispatch.go), never a cobra alias.
func TestApplyAliases_SkipsDashAndSelfName(t *testing.T) {
	parent := &cobra.Command{Use: "parent"}
	last := &cobra.Command{Use: "last"}
	kill := &cobra.Command{Use: "kill"}
	parent.AddCommand(last, kill)

	applyAliases(parent, map[string][]string{
		"last": {"-"},
		"kill": {"kill", "k", "rm"},
	})

	if len(last.Aliases) != 0 {
		t.Errorf("last.Aliases = %v, want none (dash is argv-only)", last.Aliases)
	}
	if got, want := len(kill.Aliases), 2; got != want {
		t.Fatalf("kill.Aliases = %v, want self-name skipped (%d aliases)", kill.Aliases, want)
	}
	for _, a := range kill.Aliases {
		if a == "kill" {
			t.Errorf("kill.Aliases contains self-name: %v", kill.Aliases)
		}
	}
}

// TestApplyAliases_TmuxLastSurface pins the live tmux map through the shared
// helper: the real newTmuxCmd tree keeps last alias-free while kill/rename/…
// keep their registered aliases (TestCobraAliasResolution covers resolution).
func TestApplyAliases_TmuxLastSurface(t *testing.T) {
	parent := &cobra.Command{Use: "tmux"}
	for _, name := range []string{"ls", "pick", "kill", "rename", "windows", "tree", "last", "cheat"} {
		parent.AddCommand(&cobra.Command{Use: name})
	}
	applyAliases(parent, forgive.TmuxAliases)

	for _, sub := range parent.Commands() {
		if sub.Name() == "last" {
			if len(sub.Aliases) != 0 {
				t.Errorf("tmux last grew Cobra aliases %v; \"-\" must stay argv-only", sub.Aliases)
			}
			return
		}
	}
	t.Fatal("last subcommand not found")
}
