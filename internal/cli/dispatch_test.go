package cli

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

func TestNormalizeArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"autocorrect verb punctuation+case", []string{"tmux", "LS."}, []string{"tmux", "ls"}},
		{"autocorrect kill", []string{"tmux", "Kill,"}, []string{"tmux", "kill"}},
		{"alias rm -> kill", []string{"tmux", "rm"}, []string{"tmux", "kill"}},
		{"alias x -> kill with punct", []string{"tmux", "X."}, []string{"tmux", "kill"}},
		{"module spelling normalized + verb alias", []string{"Tmux.", "p"}, []string{"tmux", "pick"}},
		{"module alias tm + verb alias", []string{"tm", "mv"}, []string{"tmux", "rename"}},
		{"dash maps to last", []string{"tmux", "-"}, []string{"tmux", "last"}},
		{"flag passes through untouched", []string{"tmux", "--bogus"}, []string{"tmux", "--bogus"}},
		{"unknown verb left as-is (TUI fallthrough signal)", []string{"tmux", "frobnicate"}, []string{"tmux", "frobnicate"}},
		{"only the verb is normalized, not its args", []string{"tmux", "RM", "old name"}, []string{"tmux", "kill", "old name"}},
		{"global flag before module", []string{"-v"}, []string{"-v"}},
		{"non-tmux first token untouched", []string{"completion", "zsh"}, []string{"completion", "zsh"}},
		{"empty", []string{}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeArgs(tc.in)
			if strings.Join(got, "\x1f") != strings.Join(tc.want, "\x1f") {
				t.Errorf("normalizeArgs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeArgs_DoesNotMutateInput(t *testing.T) {
	in := []string{"tmux", "RM"}
	_ = normalizeArgs(in)
	if in[1] != "RM" {
		t.Errorf("normalizeArgs mutated its input: %v", in)
	}
}

func TestShouldLaunchTUI(t *testing.T) {
	root := newRoot(tmux.New(&exec.FakeRunner{}), projects.New(&exec.FakeRunner{}), quarantine.New(&exec.FakeRunner{}), config.Config{})
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare invoke", []string{}, true},
		{"unknown top-level verb", []string{"frobnicate"}, true},
		{"unknown tmux subverb", []string{"tmux", "frobnicate"}, true},
		{"known verb does not launch", []string{"tmux", "ls"}, false},
		{"known alias does not launch", []string{"tmux", "kill", "x"}, false},
		{"version flag stays with fang", []string{"--version"}, false},
		{"help flag stays with fang", []string{"--help"}, false},
		{"completion command does not launch", []string{"completion", "zsh"}, false},
		{"bare tmux module does not launch here", []string{"tmux"}, false},
		// Flag-only invocations: flags alone (including --no-icons) route to fang, not TUI
		{"no-icons flag alone stays with fang", []string{"--no-icons"}, false},
		// Flag + known verb: --no-icons should not prevent Cobra dispatch
		{"no-icons plus known verb stays with cobra", []string{"--no-icons", "tmux", "ls"}, false},
		// Flag + unknown verb still routes to TUI
		{"no-icons plus unknown verb launches TUI", []string{"--no-icons", "frobnicate"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLaunchTUI(root, tc.args); got != tc.want {
				t.Errorf("shouldLaunchTUI(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestCobraAliasResolution verifies the registry-driven Cobra aliases resolve
// — root.Find(["tmux","rm"]) must land on the kill command.
func TestCobraAliasResolution(t *testing.T) {
	client := tmux.New(&exec.FakeRunner{})
	root := newTmuxCmd(client)

	cases := map[string]string{
		"rm":  "kill",
		"k":   "kill",
		"x":   "kill",
		"mv":  "rename",
		"rn":  "rename",
		"l":   "ls",
		"p":   "pick",
		"new": "pick",
	}
	for alias, wantName := range cases {
		t.Run(alias, func(t *testing.T) {
			found, _, err := root.Find([]string{alias})
			if err != nil {
				t.Fatalf("Find(%q): %v", alias, err)
			}
			if found.Name() != wantName {
				t.Errorf("alias %q resolved to %q, want %q", alias, found.Name(), wantName)
			}
		})
	}
}
