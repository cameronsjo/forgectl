package ghostty

// Test plan for parse.go
//
// ParseThemes (Classification: pure parser)
//   [x] Happy: built-in and custom (user) themes both parse, Custom set correctly
//   [x] Happy: a name that itself contains parens ("Black Metal (Bathory)") splits
//       on the LAST " (...)" span, not the first
//   [x] Happy: blank lines are skipped
//   [x] Unhappy: a line with neither "(resources)" nor "(user)" is skipped
//
// ParseKeybinds (Classification: pure parser)
//   [x] Happy: an ordinary "keybind = trigger=action" line parses
//   [x] Happy: a trigger containing "=" ("super+=") still splits correctly —
//       the LAST "=" in the remainder is the trigger/action separator
//   [x] Happy: a trailing "# comment" on the action is peeled into Comment
//   [x] Happy: an action arg with ":" args is preserved whole
//   [x] Happy: blank lines are skipped
//   [x] Unhappy: a line without the "keybind = " prefix is skipped
//
// ParseConfig / Config.Theme (Classification: pure parser)
//   [x] Happy: "key = value" lines populate Values, repeatable keys append
//   [x] Happy: Theme() reads the "theme" key
//   [x] Unhappy: Theme() returns "" when no "theme" key is present

import (
	"reflect"
	"testing"
)

// themesFixture is lifted verbatim from a live `ghostty +list-themes --plain`
// run (cmux's bundled binary, /Applications/cmux.app/Contents/Resources/bin/
// ghostty) — 15 representative lines: both of Cameron's custom themes, a
// spread of built-ins, and the two-paren-group edge case.
const themesFixture = `0x96f (resources)
12-bit Rainbow (resources)
3024 Day (resources)
3024 Night (resources)
Aardvark Blue (resources)
Abernathy (resources)
Adventure (resources)
Adventure Time (resources)
Adwaita (resources)
Adwaita Dark (resources)
Black Metal (Bathory) (resources)
Hot Dog Stand (Mustard) (resources)
IBM 5153 CGA (Black) (resources)
artificer-dark (user)
artificer-light (user)`

func TestParseThemes_LiveFixture(t *testing.T) {
	themes := ParseThemes(themesFixture)
	if len(themes) != 15 {
		t.Fatalf("got %d themes, want 15 (fixture line count)", len(themes))
	}

	byName := map[string]Theme{}
	for _, th := range themes {
		byName[th.Name] = th
	}

	custom, ok := byName["artificer-dark"]
	if !ok || !custom.Custom {
		t.Errorf("artificer-dark: got %+v, want Custom=true", custom)
	}
	custom, ok = byName["artificer-light"]
	if !ok || !custom.Custom {
		t.Errorf("artificer-light: got %+v, want Custom=true", custom)
	}

	builtin, ok := byName["Adwaita Dark"]
	if !ok || builtin.Custom {
		t.Errorf("Adwaita Dark: got %+v, want Custom=false", builtin)
	}

	// The two-paren-group edge case: the discriminator suffix is the LAST
	// " (...)" span, so the theme's own parenthesized name survives intact.
	nested, ok := byName["Black Metal (Bathory)"]
	if !ok || nested.Custom {
		t.Errorf("Black Metal (Bathory): got %+v (ok=%v), want Name=%q Custom=false", nested, ok, "Black Metal (Bathory)")
	}
	nested, ok = byName["Hot Dog Stand (Mustard)"]
	if !ok || nested.Custom {
		t.Errorf("Hot Dog Stand (Mustard): got %+v (ok=%v), want Custom=false", nested, ok)
	}
}

func TestParseThemes_BlankLinesSkipped(t *testing.T) {
	themes := ParseThemes("\n\nartificer-dark (user)\n\n")
	if len(themes) != 1 {
		t.Fatalf("got %d themes, want 1", len(themes))
	}
}

func TestParseThemes_UnrecognizedSuffixSkipped(t *testing.T) {
	themes := ParseThemes("some garbage line\nartificer-dark (user)")
	if len(themes) != 1 {
		t.Fatalf("got %d themes, want 1 (garbage line skipped): %+v", len(themes), themes)
	}
	if themes[0].Name != "artificer-dark" {
		t.Errorf("Name = %q, want artificer-dark", themes[0].Name)
	}
}

// keybindsFixture is lifted verbatim from a live `ghostty +list-keybinds` run
// — 15 lines covering: a plain binding, the two three-"="-sign lines
// (super+= as a literal trigger key), multi-modifier chords, arrow/digit
// triggers, an escape trigger, and text: actions both with and without a
// trailing "# tmux command" comment.
const keybindsFixture = `keybind = super+shift+,=reload_config
keybind = copy=copy_to_clipboard:mixed
keybind = super+==increase_font_size:1
keybind = super+ctrl+==equalize_splits
keybind = super+ctrl+shift+j=write_screen_file:copy,plain
keybind = shift+arrow_left=adjust_selection:left
keybind = super+digit_1=goto_tab:1
keybind = escape=end_search
keybind = super+ctrl+arrow_up=resize_split:up,10
keybind = super+ctrl+f=text:\\x00\\x06    # Search in pane (Ctrl+Space, Ctrl+F)
keybind = super+ctrl+t=text:\\x00c       # New tmux window
keybind = super+ctrl+w=text:\\x00x       # Close tmux pane (with confirm)
keybind = super+ctrl+'=text:\\x00'  # Jump to last active session
keybind = super+ctrl+semicolon=text:\\x00;   # Jump to last active window
keybind = super+ctrl+z=text:\\x00z           # Toggle pane zoom (fullscreen)`

func TestParseKeybinds_LiveFixture(t *testing.T) {
	binds := ParseKeybinds(keybindsFixture)
	if len(binds) != 15 {
		t.Fatalf("got %d keybinds, want 15 (fixture line count)", len(binds))
	}

	byTrigger := map[string]Keybind{}
	for _, kb := range binds {
		byTrigger[kb.Trigger] = kb
	}

	if kb := byTrigger["super+shift+,"]; kb.Action != "reload_config" {
		t.Errorf("super+shift+,: Action = %q, want reload_config", kb.Action)
	}

	// The trigger itself contains "=" (the equals key) — the LAST "=" in the
	// remainder must be the trigger/action separator, not the first.
	kb, ok := byTrigger["super+="]
	if !ok {
		t.Fatalf("expected trigger %q to parse, got triggers: %v", "super+=", triggerNames(binds))
	}
	if kb.Action != "increase_font_size:1" {
		t.Errorf("super+=: Action = %q, want increase_font_size:1", kb.Action)
	}

	kb, ok = byTrigger["super+ctrl+="]
	if !ok {
		t.Fatalf("expected trigger %q to parse, got triggers: %v", "super+ctrl+=", triggerNames(binds))
	}
	if kb.Action != "equalize_splits" {
		t.Errorf("super+ctrl+=: Action = %q, want equalize_splits", kb.Action)
	}

	// A trailing "# ..." comment is peeled off the action, not left inline.
	kb = byTrigger["super+ctrl+t"]
	if kb.Action != `text:\\x00c` {
		t.Errorf("super+ctrl+t: Action = %q, want %q", kb.Action, `text:\\x00c`)
	}
	if kb.Comment != "New tmux window" {
		t.Errorf("super+ctrl+t: Comment = %q, want %q", kb.Comment, "New tmux window")
	}

	// An action arg containing ":" is preserved whole.
	kb = byTrigger["super+ctrl+arrow_up"]
	if kb.Action != "resize_split:up,10" {
		t.Errorf("super+ctrl+arrow_up: Action = %q, want resize_split:up,10", kb.Action)
	}

	// A binding with no comment leaves Comment empty.
	kb = byTrigger["shift+arrow_left"]
	if kb.Comment != "" {
		t.Errorf("shift+arrow_left: Comment = %q, want empty", kb.Comment)
	}

	// The apostrophe trigger is itself echoed at the tail of its action,
	// right before the comment — confirms the comment split doesn't eat it.
	kb = byTrigger["super+ctrl+'"]
	if kb.Action != `text:\\x00'` {
		t.Errorf("super+ctrl+': Action = %q, want %q", kb.Action, `text:\\x00'`)
	}
	if kb.Comment != "Jump to last active session" {
		t.Errorf("super+ctrl+': Comment = %q, want %q", kb.Comment, "Jump to last active session")
	}
}

func triggerNames(binds []Keybind) []string {
	names := make([]string, len(binds))
	for i, kb := range binds {
		names[i] = kb.Trigger
	}
	return names
}

func TestParseKeybinds_BlankLinesSkipped(t *testing.T) {
	binds := ParseKeybinds("\n\nkeybind = escape=end_search\n\n")
	if len(binds) != 1 {
		t.Fatalf("got %d keybinds, want 1", len(binds))
	}
}

func TestParseKeybinds_MissingPrefixSkipped(t *testing.T) {
	binds := ParseKeybinds("not a keybind line\nkeybind = escape=end_search")
	if len(binds) != 1 {
		t.Fatalf("got %d keybinds, want 1 (non-keybind line skipped): %+v", len(binds), binds)
	}
	if binds[0].Trigger != "escape" {
		t.Errorf("Trigger = %q, want escape", binds[0].Trigger)
	}
}

func TestParseConfig_ValuesAndRepeatableKeys(t *testing.T) {
	out := "theme = artificer-dark\nfont-size = 14\npalette = 0=#5f6576\npalette = 1=#a04540\n"
	cfg := ParseConfig(out)

	want := map[string][]string{
		"theme":     {"artificer-dark"},
		"font-size": {"14"},
		"palette":   {"0=#5f6576", "1=#a04540"},
	}
	if !reflect.DeepEqual(cfg.Values, want) {
		t.Errorf("Values = %+v, want %+v", cfg.Values, want)
	}
}

func TestConfig_Theme(t *testing.T) {
	cfg := ParseConfig("theme = artificer-dark\n")
	if got := cfg.Theme(); got != "artificer-dark" {
		t.Errorf("Theme() = %q, want artificer-dark", got)
	}
}

func TestConfig_Theme_AbsentReturnsEmpty(t *testing.T) {
	cfg := ParseConfig("font-size = 14\n")
	if got := cfg.Theme(); got != "" {
		t.Errorf("Theme() = %q, want empty", got)
	}
}
