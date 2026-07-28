// Package ghostty is the ops layer for `forgectl ghostty`: a Client over
// Ghostty's only control surface, the `ghostty +<action>` CLI (Ghostty
// exposes no IPC/socket and no AppleScript — forgectl#7). It knows nothing of
// Cobra — that decoupling is the house pattern (see internal/tmux,
// internal/net).
//
// parse.go holds the pure parsers: no I/O, table-tested directly against
// lines lifted from a real `ghostty +list-themes`/`+list-keybinds` run
// (internal/ghostty/parse_test.go), so a shape the live binary actually
// produces is what's pinned — not a guess at one.
package ghostty

import "strings"

// Theme is one entry from `ghostty +list-themes --plain`. Custom discriminates
// Cameron's own themes (~/.config/ghostty/themes/) from the ~460 built-ins
// bundled in the app resources. Active is never set by ParseThemes itself —
// the pure parser has no notion of "currently active"; Client.Themes sets it
// afterward by cross-referencing +show-config's theme key.
type Theme struct {
	Name   string
	Custom bool
	Active bool
}

// ParseThemes parses the plain-text output of `ghostty +list-themes --plain`.
// Each line is "Name (resources)" or "Name (user)" — verified against a live
// 465-theme listing, including names that themselves contain parens ("Black
// Metal (Bathory) (resources)"), which is why the split looks for the LAST
// " (...)" span rather than the first. Blank lines and any line that doesn't
// end in a recognized suffix are skipped.
func ParseThemes(output string) []Theme {
	var themes []Theme
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if name, custom, ok := splitThemeLine(line); ok {
			themes = append(themes, Theme{Name: name, Custom: custom})
		}
	}
	return themes
}

// splitThemeLine splits "Name (resources)" / "Name (user)" on the last
// " (" span, returning whether the theme is custom ("user") and ok=false for
// any line that doesn't end in one of those two recognized suffixes.
func splitThemeLine(line string) (name string, custom bool, ok bool) {
	if !strings.HasSuffix(line, ")") {
		return "", false, false
	}
	idx := strings.LastIndex(line, " (")
	if idx < 0 {
		return "", false, false
	}
	name = line[:idx]
	kind := line[idx+2 : len(line)-1]
	switch kind {
	case "user":
		return name, true, true
	case "resources":
		return name, false, true
	default:
		return "", false, false
	}
}

// Keybind is one parsed `ghostty +list-keybinds` line: the trigger chord, the
// action (with any ":"-separated args, e.g. "resize_split:up,10"), and an
// optional trailing comment. Cameron's config annotates tmux-passthrough
// bindings with a trailing "# ..." (e.g. "text:\x00c  # New tmux window") —
// Comment surfaces that so `cheat` can show ghostty-native vs
// tmux-via-keysend bindings side by side (forgectl#7's "ideally" note).
type Keybind struct {
	Trigger string
	Action  string
	Comment string
}

// keybindPrefix is the fixed lead-in every `+list-keybinds` line carries.
const keybindPrefix = "keybind = "

// ParseKeybinds parses the output of `ghostty +list-keybinds`. Each line is
// "keybind = <trigger>=<action>" and can carry more than the two "=" signs
// that shape implies, because the trigger itself can contain one — verified
// against a live 116-binding listing, which has exactly two such lines:
// "keybind = super+==increase_font_size:1" (super, plus the "=" key) and
// "keybind = super+ctrl+==equalize_splits". No action in that listing
// contains a literal "=", so the trigger/action split is on the LAST "=" in
// the remainder after the "keybind = " prefix — not the first. Blank lines
// and any line missing the prefix or a "=" in its remainder are skipped.
func ParseKeybinds(output string) []Keybind {
	var binds []Keybind
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if kb, ok := parseKeybindLine(line); ok {
			binds = append(binds, kb)
		}
	}
	return binds
}

func parseKeybindLine(line string) (Keybind, bool) {
	rest, ok := strings.CutPrefix(line, keybindPrefix)
	if !ok {
		return Keybind{}, false
	}
	idx := strings.LastIndex(rest, "=")
	if idx < 0 {
		return Keybind{}, false
	}
	trigger := rest[:idx]
	action, comment := splitTrailingComment(rest[idx+1:])
	return Keybind{Trigger: trigger, Action: action, Comment: comment}, true
}

// splitTrailingComment peels a trailing "# ..." annotation off an action —
// e.g. "text:\x00c       # New tmux window". Only a "#" preceded by
// whitespace counts as a comment start, since no action's own argument ever
// carries one (verified against the live fixture).
func splitTrailingComment(action string) (string, string) {
	idx := strings.Index(action, " #")
	if idx < 0 {
		return strings.TrimSpace(action), ""
	}
	return strings.TrimSpace(action[:idx]), strings.TrimSpace(action[idx+2:])
}

// Config is a parsed `ghostty +show-config` snapshot — the resolved
// key/value pairs Ghostty is actually running with. Repeatable keys
// (`palette = N=#hex`) keep every value in encounter order; Theme reads the
// single-valued "theme" key. Nothing outside this package needs more than
// that today — Config exists for Client.Themes' internal use, not as a
// general config-inspection surface (forgectl#7 scope: no fold into
// `forgectl config`).
type Config struct {
	Values map[string][]string
}

// Theme returns the active theme name, or "" if the resolved config carries
// no explicit "theme" key (Ghostty's own built-in default is in effect).
func (c Config) Theme() string {
	vs := c.Values["theme"]
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

// ParseConfig parses the output of `ghostty +show-config`: "key = value"
// lines, blank lines skipped. Ghostty's own --help for the command notes the
// line order isn't guaranteed to match the user's config file but IS
// consistent between runs; ParseConfig doesn't depend on order at all.
func ParseConfig(output string) Config {
	values := map[string][]string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		values[key] = append(values[key], value)
	}
	return Config{Values: values}
}
