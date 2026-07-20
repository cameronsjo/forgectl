package cli

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
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
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
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
		// A runnable parent that takes a positional (`pr <ref>`) must dispatch its
		// arg to Cobra, not be mistaken for an unknown subverb → menu.
		{"pr with ref positional stays with cobra", []string{"pr", "owner/repo#1"}, false},
		{"pr known subverb does not launch", []string{"pr", "list"}, false},
		// A group whose parent takes value-flags: the flag VALUE is a non-flag
		// token and must reach Cobra, not be mistaken for an unknown subverb →
		// menu (the review Use line declares this via its [--…] placeholders).
		{"review repo-flag value stays with cobra", []string{"review", "--json", "--repo", "owner/name"}, false},
		{"review kind-flag value stays with cobra", []string{"review", "--kind", "issue"}, false},
		{"review known subverb does not launch", []string{"review", "mark", "owner/repo#1"}, false},
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

// TestDecideRoute covers the headless TTY gate (forgectl#103): a menu-eligible
// invocation (bare, unknown top-level verb, or unknown subverb) opens the TUI
// only when tty is true; headless, it must route through Cobra/fang instead
// so cobra's own "unknown command" + "did you mean" suggestion path runs
// rather than silently opening the menu and exiting 0. tty is a plain bool
// here (not a real isatty call) so this never needs a real terminal.
func TestDecideRoute(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	cases := []struct {
		name string
		args []string
		tty  bool
		want menuRoute
	}{
		{"bare invoke, tty draws the menu", []string{}, true, routeTUI},
		{"bare invoke, headless routes to cobra", []string{}, false, routeHeadlessMenu},
		{"unknown top-level verb, tty draws the menu (kept)", []string{"frobnicate"}, true, routeTUI},
		{"unknown top-level verb, headless routes to cobra", []string{"frobnicate"}, false, routeHeadlessMenu},
		{"unknown tmux subverb, tty draws the menu", []string{"tmux", "frobnicate"}, true, routeTUI},
		{"unknown tmux subverb, headless routes to cobra", []string{"tmux", "frobnicate"}, false, routeHeadlessMenu},
		{"known verb dispatches regardless of tty (interactive)", []string{"tmux", "ls"}, true, routeDispatch},
		{"known verb dispatches regardless of tty (headless)", []string{"tmux", "ls"}, false, routeDispatch},
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
