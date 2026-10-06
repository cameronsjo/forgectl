package keymap

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestCancel_BindsEsc is the assertion the per-picker literals could not
// carry. The keymap is a plain value, so it needs no form and no TTY — only
// letting the FORM run requires one, because huh opens /dev/tty directly.
//
// Without this, the esc binding was verifiable only by hand: a regression on
// the next huh bump would surface as a picker that reads as stuck, with a
// green suite.
func TestCancel_BindsEsc(t *testing.T) {
	km := Cancel()

	keys := km.Quit.Keys()
	for _, want := range []string{"ctrl+c", "esc"} {
		if !slices.Contains(keys, want) {
			t.Errorf("Quit is not bound to %q (bound: %v) — the picker will read as stuck to anyone pressing it", want, keys)
		}
	}

	// The help text is what the picker actually shows, so a binding with no
	// visible hint is only half the fix.
	if got := km.Quit.Help().Key; got != "esc" {
		t.Errorf("Quit help key = %q, want %q", got, "esc")
	}
	if got := km.Quit.Help().Desc; got != "cancel" {
		t.Errorf("Quit help desc = %q, want %q", got, "cancel")
	}
}

// TestSuspendFilter pins forgectl#1101: Ctrl+Z becomes a suspend, every other
// message passes through untouched.
//
// Mutation that turns it red: make SuspendFilter return msg unchanged.
func TestSuspendFilter(t *testing.T) {
	if _, ok := SuspendFilter(nil, tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}).(tea.SuspendMsg); !ok {
		t.Error("ctrl+z did not become tea.SuspendMsg")
	}
	for _, msg := range []tea.Msg{
		tea.KeyPressMsg{Code: 'z', Text: "z"},
		tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
		tea.WindowSizeMsg{Width: 80, Height: 24},
	} {
		if got := SuspendFilter(nil, msg); got != msg {
			t.Errorf("%v was changed to %v", msg, got)
		}
	}
}

// TestEveryProgramTakesTheSuspendFilter keeps a new TUI from re-opening the
// gap: each tea.NewProgram goes through ProgramOptions and each one-shot huh
// picker passes Form(), or Ctrl+Z does nothing there.
//
// desk_model_unix.go is the one allowed gap: the desk files belong to another
// change and move onto ProgramOptions with it.
//
// Mutation that turns it red: write tea.WithContext(ctx) in place of
// keymap.ProgramOptions(ctx)... in cockpit.go.
func TestEveryProgramTakesTheSuspendFilter(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if filepath.Base(path) == "desk_model_unix.go" {
			return nil
		}
		src, err := os.ReadFile(path) // #nosec G304 G122 -- source scan of this repo's own tree, read-only
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if strings.Contains(code, "tea.NewProgram(") && !strings.Contains(code, "keymap.ProgramOptions(") {
				t.Errorf("%s:%d: tea.NewProgram without keymap.ProgramOptions: Ctrl+Z will not suspend", path, i+1)
			}
			if strings.Contains(code, ".Huh()).Run()") && !strings.Contains(code, "keymap.Form()") {
				t.Errorf("%s:%d: huh form without keymap.Form(): Ctrl+Z will not suspend", path, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
