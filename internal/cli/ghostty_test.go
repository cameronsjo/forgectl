package cli

// Test plan for ghostty.go
//
// newGhosttyThemesCmd (Classification: API handler / cobra command)
//   [x] Happy: default output lists only custom themes, active one marked
//   [x] Happy: --all appends built-in themes after the custom ones
//   [x] Happy: no themes at all prints a clear "none found" message
//
// newGhosttyCheatCmd (Classification: API handler / cobra command)
//   [x] Happy: renders the keybind sheet from parsed +list-keybinds output

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	ghosttypkg "github.com/cameronsjo/forgectl/internal/ghostty"
)

func ghosttyFixture(themesOut, keybindsOut, configOut string) *ghosttypkg.Client {
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			if len(args) == 0 {
				return "", nil
			}
			switch args[0] {
			case "+list-themes":
				return themesOut, nil
			case "+list-keybinds":
				return keybindsOut, nil
			case "+show-config":
				return configOut, nil
			}
			return "", nil
		},
	}
	return ghosttypkg.New(fake, ghosttypkg.WithBin("ghostty"))
}

func TestGhosttyThemesCmd_DefaultListsCustomOnly_ActiveMarked(t *testing.T) {
	client := ghosttyFixture(
		"artificer-dark (user)\nartificer-light (user)\nAdwaita Dark (resources)\n",
		"",
		"theme = artificer-dark\n",
	)
	cmd := newGhosttyThemesCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "artificer-dark") || !strings.Contains(out, "artificer-light") {
		t.Fatalf("expected both custom themes in output, got: %q", out)
	}
	if strings.Contains(out, "Adwaita Dark") {
		t.Errorf("built-in theme should not appear without --all, got: %q", out)
	}
	// The active theme's row carries the "*" marker; the inactive custom
	// theme's row does not.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var activeLine, inactiveLine string
	for _, l := range lines {
		if strings.Contains(l, "artificer-dark") {
			activeLine = l
		}
		if strings.Contains(l, "artificer-light") {
			inactiveLine = l
		}
	}
	if !strings.HasPrefix(activeLine, "*") {
		t.Errorf("active theme line = %q, want a leading * marker", activeLine)
	}
	if strings.HasPrefix(inactiveLine, "*") {
		t.Errorf("inactive theme line = %q, want no leading * marker", inactiveLine)
	}
}

func TestGhosttyThemesCmd_AllFlag_AppendsBuiltins(t *testing.T) {
	client := ghosttyFixture(
		"artificer-dark (user)\nAdwaita Dark (resources)\n0x96f (resources)\n",
		"",
		"theme = artificer-dark\n",
	)
	cmd := newGhosttyThemesCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--all"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"artificer-dark", "Adwaita Dark", "0x96f"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in --all output, got: %q", want, out)
		}
	}
	// Custom themes come first: artificer-dark's line index precedes both
	// built-ins'.
	customIdx := strings.Index(out, "artificer-dark")
	builtinIdx := strings.Index(out, "Adwaita Dark")
	if customIdx < 0 || builtinIdx < 0 || customIdx > builtinIdx {
		t.Errorf("expected custom themes before built-ins, got: %q", out)
	}
}

func TestGhosttyThemesCmd_NoneFound_PrintsClearMessage(t *testing.T) {
	client := ghosttyFixture("", "", "")
	cmd := newGhosttyThemesCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "no ghostty themes found") {
		t.Errorf("expected a clear empty message, got: %q", stdout.String())
	}
}

func TestGhosttyCheatCmd_RendersParsedKeybinds(t *testing.T) {
	client := ghosttyFixture("", "keybind = escape=end_search\n", "")
	cmd := newGhosttyCheatCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "escape") || !strings.Contains(out, "end_search") {
		t.Errorf("expected the parsed keybind in output, got: %q", out)
	}
}
