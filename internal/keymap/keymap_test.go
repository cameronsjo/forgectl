package keymap

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
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
// gap: each tea.NewProgram goes through ProgramOptions, and every file that
// runs a huh form outside the TUI package (where forms are embedded in a model
// that already has the filter) wraps each one in Suspendable, or Ctrl+Z does
// nothing there. The huh check counts per file because a form's chain spans
// lines.
//
// desk_model_unix.go is the one allowed gap: the desk files belong to another
// change and move onto ProgramOptions with it.
//
// Mutation that turns it red: write tea.WithContext(ctx) in place of
// keymap.ProgramOptions(ctx)... in cockpit.go, or drop Suspendable from confirm.go.
func TestEveryProgramTakesTheSuspendFilter(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if filepath.Base(path) == "desk_model_unix.go" {
			return nil
		}
		raw, err := os.ReadFile(path) // #nosec G304 G122 -- source scan of this repo's own tree, read-only
		if err != nil {
			return err
		}
		var code []string
		for _, line := range strings.Split(string(raw), "\n") {
			c, _, _ := strings.Cut(line, "//")
			code = append(code, c)
		}
		src := strings.Join(code, "\n")
		for i, line := range code {
			if strings.Contains(line, "tea.NewProgram(") && !strings.Contains(line, "keymap.ProgramOptions(") {
				t.Errorf("%s:%d: tea.NewProgram without keymap.ProgramOptions: Ctrl+Z will not suspend", path, i+1)
			}
		}
		if strings.Contains(filepath.ToSlash(path), "/tui/") || strings.HasSuffix(path, "keymap.go") {
			return nil
		}
		forms := strings.Count(src, "huh.NewForm(")
		wrapped := strings.Count(src, "keymap.Suspendable(")
		fields := len(regexp.MustCompile(`huh\.New(Confirm|Input|Text|Select|MultiSelect|Note)\b`).FindAllString(src, -1))
		if forms != wrapped || (forms == 0 && fields > 0) {
			t.Errorf("%s: %d huh.NewForm, %d keymap.Suspendable, %d bare fields: every form needs Suspendable or Ctrl+Z will not suspend", path, forms, wrapped, fields)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// teaOptionCount reads the form's program-option list. The field is
// unexported, so this reads only its length through reflection; a huh bump
// that renames it fails the test loudly instead of passing silently.
func teaOptionCount(t *testing.T, f *huh.Form) int {
	t.Helper()
	field := reflect.ValueOf(f).Elem().FieldByName("teaOptions")
	if !field.IsValid() {
		t.Fatal("huh.Form no longer has a teaOptions field; revisit keymap.Suspendable")
	}
	return field.Len()
}

// TestSuspendableKeepsFormOutputOnStderr pins the review finding on
// forgectl#1101: huh's WithProgramOptions replaces the form's option list, and
// the default it replaces is tea.WithOutput(os.Stderr), so a bare
// WithProgramOptions(filter) would move every picker's frames to stdout.
// Suspendable must add the filter and put the output back.
//
// Mutation that turns it red: drop .WithOutput(os.Stderr) from Suspendable.
func TestSuspendableKeepsFormOutputOnStderr(t *testing.T) {
	base := teaOptionCount(t, huh.NewForm())
	if base != 1 {
		t.Fatalf("a new huh form has %d program options, want 1 (the stderr default); revisit keymap.Suspendable", base)
	}
	if got := teaOptionCount(t, huh.NewForm().WithProgramOptions(tea.WithFilter(SuspendFilter))); got != 1 {
		t.Fatalf("WithProgramOptions left %d options; the premise that it replaces the list no longer holds", got)
	}
	if got := teaOptionCount(t, Suspendable(huh.NewForm())); got != 2 {
		t.Errorf("Suspendable form has %d program options, want 2 (the filter and the stderr output)", got)
	}
}
