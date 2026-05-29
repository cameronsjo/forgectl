package cli

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
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

// TestCobraAliasResolution verifies the registry-driven Cobra aliases resolve
// — root.Find(["tmux","rm"]) must land on the kill command.
func TestCobraAliasResolution(t *testing.T) {
	client := tmux.New(&exec.FakeRunner{})
	root := newTmuxCmd(client)

	cases := map[string]string{
		"rm":   "kill",
		"k":    "kill",
		"x":    "kill",
		"mv":   "rename",
		"rn":   "rename",
		"l":    "ls",
		"p":    "pick",
		"new":  "pick",
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
