package cli

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func TestNormalizeArgs_DoesNotMutateInput(t *testing.T) {
	in := []string{"projects", "list"}
	got := normalizeArgs(in)
	if len(got) != 2 || got[0] != "projects" || got[1] != "list" {
		t.Errorf("normalizeArgs(%v) = %v, want unchanged", in, got)
	}
}

// TestShouldLaunchTUI pins the hub-era narrowing (forgectl#479): the TUI
// opens on a bare invoke only. An unknown top-level verb and an unknown
// subverb of a known group both fall through to Cobra/fang's own
// unknown-command error now — the group parents' Args: cobra.NoArgs
// (projects/quarantine) is what makes that safe (TestGroupParentsRefuseStrayTokens).
func TestShouldLaunchTUI(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare invoke launches the hub", []string{}, true},
		{"unknown top-level verb stays with cobra", []string{"frobnicate"}, false},
		{"unknown projects subverb stays with cobra", []string{"projects", "frobnicate"}, false},
		{"typo'd verb stays with cobra (did-you-mean)", []string{"lauch"}, false},
		{"known verb does not launch", []string{"projects", "list"}, false},
		{"pr with ref positional stays with cobra", []string{"pr", "owner/repo#1"}, false},
		{"pr known subverb does not launch", []string{"pr", "list"}, false},
		{"review repo-flag value stays with cobra", []string{"review", "--json", "--repo", "owner/name"}, false},
		{"review kind-flag value stays with cobra", []string{"review", "--kind", "issue"}, false},
		{"review known subverb does not launch", []string{"review", "mark", "owner/repo#1"}, false},
		{"version flag stays with fang", []string{"--version"}, false},
		{"help flag stays with fang", []string{"--help"}, false},
		{"completion command does not launch", []string{"completion", "zsh"}, false},
		{"bare projects module does not launch here", []string{"projects"}, false},
		// Flag-only invocations: flags alone (including --no-icons) route to fang, not TUI
		{"no-icons flag alone stays with fang", []string{"--no-icons"}, false},
		// Flag + known verb: --no-icons should not prevent Cobra dispatch
		{"no-icons plus known verb stays with cobra", []string{"--no-icons", "projects", "list"}, false},
		// Flag + unknown verb now stays with cobra too (the hub only opens bare)
		{"no-icons plus unknown verb stays with cobra", []string{"--no-icons", "frobnicate"}, false},
		{"bare version verb stays with cobra", []string{"version"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLaunchTUI(root, tc.args); got != tc.want {
				t.Errorf("shouldLaunchTUI(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestDecideRoute covers the headless TTY gate (forgectl#103).
func TestDecideRoute(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	cases := []struct {
		name string
		args []string
		tty  bool
		want menuRoute
	}{
		{"bare invoke, tty draws the hub", []string{}, true, routeTUI},
		{"bare invoke, headless routes to cobra", []string{}, false, routeHeadlessMenu},
		{"unknown top-level verb, tty routes to cobra", []string{"frobnicate"}, true, routeDispatch},
		{"unknown top-level verb, headless routes to cobra", []string{"frobnicate"}, false, routeDispatch},
		{"unknown projects subverb, tty routes to cobra", []string{"projects", "frobnicate"}, true, routeDispatch},
		{"unknown projects subverb, headless routes to cobra", []string{"projects", "frobnicate"}, false, routeDispatch},
		{"known verb dispatches regardless of tty (interactive)", []string{"projects", "list"}, true, routeDispatch},
		{"known verb dispatches regardless of tty (headless)", []string{"projects", "list"}, false, routeDispatch},
		{"version flag dispatches even headless", []string{"--version"}, false, routeDispatch},
		{"help flag dispatches even headless", []string{"--help"}, false, routeDispatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideRoute(root, tc.args, tc.tty); got != tc.want {
				t.Errorf("decideRoute(%v, tty=%v) = %v, want %v", tc.args, tc.tty, got, tc.want)
			}
		})
	}
}
