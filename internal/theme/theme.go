// Package theme is the single source every colored surface in forgectl draws
// from — the Artificer terminal palette, resolved per role, with a config
// escape hatch (preset, mode, per-role overrides) and adapters for lipgloss,
// huh, bubbles' list, and fang.
//
// It is a leaf over internal/config: it may import config to decode
// [theme], and must never import internal/cli, internal/tui, or
// internal/module — every one of those imports theme, not the reverse.
package theme

import (
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Mode is the [theme].mode a config or Options declares — which background
// a Theme resolves against, or whether to detect it.
type Mode int

const (
	// ModeAuto detects the terminal background where it is safe to probe
	// (ShouldProbe), and falls back to dark otherwise.
	ModeAuto Mode = iota
	// ModeDark always resolves dark, with zero probe calls.
	ModeDark
	// ModeLight always resolves light, with zero probe calls.
	ModeLight
)

// Options is a fully-resolved [theme] section: which preset to start from,
// which mode to render, and any per-role colour overrides. FromConfig builds
// one from config.ThemeConfig; New turns one into a Theme.
type Options struct {
	Preset    string
	Mode      Mode
	Overrides map[Role]Pair
}

// Env is the environment Detect and ShouldProbe read to decide whether
// probing the terminal background is safe. It is a plain struct — not read
// from os.Environ() internally — so callers (Execute, the TUI, tests) control
// exactly what it sees.
type Env struct {
	StdinTTY  bool
	StdoutTTY bool
	Term      string
	NoColor   bool
}

// ShouldProbe reports whether it is safe to ask the terminal for its
// background colour: both stdin and stdout must be a real TTY, NO_COLOR must
// be unset, and TERM must not be a multiplexer that intercepts the query
// (screen*, tmux* — those never forward the OSC 11 response, so a probe there
// hangs until its own timeout). This is the one predicate every probe site
// (Execute's plain-print path, huh forms, the TUI) shares — it does not
// itself decide whether to probe, only whether probing is safe.
func ShouldProbe(_ Mode, env Env) bool {
	if !env.StdinTTY || !env.StdoutTTY || env.NoColor {
		return false
	}
	if strings.HasPrefix(env.Term, "screen") || strings.HasPrefix(env.Term, "tmux") {
		return false
	}
	return true
}

// Detect resolves isDark for mode. ModeDark and ModeLight are fixed and never
// call probe. ModeAuto calls probe only when ShouldProbe(mode, env) is true;
// otherwise — including when probe is nil — it returns dark, since dark is
// the far more common terminal background and a theme that guesses wrong is
// recoverable (WithDark) while one that panics is not.
func Detect(mode Mode, env Env, probe func() bool) bool {
	switch mode {
	case ModeDark:
		return true
	case ModeLight:
		return false
	default: // ModeAuto
		if probe == nil || !ShouldProbe(mode, env) {
			return true
		}
		return probe()
	}
}

// FromConfig maps a decoded config.ThemeConfig onto Options, translating
// preset/mode strings and [theme.colors] role names into their typed forms.
// It returns an error naming the bad value rather than defaulting silently —
// config.ThemeConfig.Validate should already have caught this for a strict
// decode, but FromConfig owns the domain mapping and re-checks rather than
// trusting an unvalidated ThemeConfig (Load stays tolerant; theme.FromConfig
// is a stricter boundary a caller may choose to enforce).
func FromConfig(c config.ThemeConfig) (Options, error) {
	var o Options

	switch c.Preset {
	case "", "artificer", "legacy":
		o.Preset = c.Preset
	default:
		return Options{}, fmt.Errorf("theme: unknown preset %q; must be \"artificer\" or \"legacy\"", c.Preset)
	}

	switch c.Mode {
	case "", "auto":
		o.Mode = ModeAuto
	case "dark":
		o.Mode = ModeDark
	case "light":
		o.Mode = ModeLight
	default:
		return Options{}, fmt.Errorf("theme: unknown mode %q; must be \"auto\", \"dark\", or \"light\"", c.Mode)
	}

	if len(c.Colors) > 0 {
		o.Overrides = make(map[Role]Pair, len(c.Colors))
		for name, ov := range c.Colors {
			r, ok := roleByName(strings.ToLower(name))
			if !ok {
				return Options{}, fmt.Errorf("theme: unknown role %q; roles are %s", name, strings.Join(RoleNames(), ", "))
			}
			o.Overrides[r] = Pair{Dark: ov.Dark, Light: ov.Light}
		}
	}

	return o, nil
}

// Theme is a fully-resolved colour set: a palette (preset plus overrides)
// pinned to one background. Every accessor is argument-free — the isDark
// decision is made once, at construction (New) or WithDark, not re-derived
// per call.
//
// The zero value Theme{} behaves exactly like Default() for every method
// (TestTheme_ZeroValueMatchesDefault pins this): module.Deps.Theme is a zero
// value in any test that does not set it, and it must render usable styles
// rather than panicking or emitting empty hex.
//
// The background is stored INVERTED — isLight, not isDark — so that the zero
// value resolves to DARK. forgectl is dark-first everywhere else, and a
// zero-value Theme rendering light on a dark terminal is the same defect this
// package exists to prevent: huh v2 ships exactly that bug, defaulting a
// standalone form to light because its own flag zero-values to false
// (internal/keymap pins it dark for that reason). A field whose safe state is
// its zero state cannot be got wrong by forgetting to set it.
type Theme struct {
	opts Options
	// isLight is the inverse of the question every caller asks. Read it
	// through IsDark; nothing outside this file should touch it.
	isLight bool
}

// New builds a Theme from o, pinned to isDark.
func New(o Options, isDark bool) Theme {
	return Theme{opts: o, isLight: !isDark}
}

// Default is the zero-configuration Theme: Artificer, resolved DARK — see the
// Theme.isLight field comment for why the zero value lands there rather than
// on light. Callers that need a real runtime default should still go through
// FromConfig and Detect; Default exists so code that only ever sees
// module.Deps.Theme's zero value gets a working, correctly-oriented theme.
func Default() Theme {
	return Theme{}
}

// palette resolves t's preset (defaulting to Artificer) with overrides
// applied.
func (t Theme) palette() Palette {
	base := Artificer()
	if t.opts.Preset == "legacy" {
		base = Legacy()
	}
	return base.withOverrides(t.opts.Overrides)
}

// WithDark returns a copy of t pinned to isDark — the TUI calls this from a
// tea.BackgroundColorMsg to rebuild every style once real terminal state
// arrives.
func (t Theme) WithDark(isDark bool) Theme {
	t.isLight = !isDark
	return t
}

// Mode reports the Mode this Theme was configured with — ModeAuto for a
// Theme built from a zero Options, since that is FromConfig's mapping of an
// absent or "auto" [theme].mode.
func (t Theme) Mode() Mode {
	return t.opts.Mode
}

// IsDark reports which background this Theme is currently pinned to.
func (t Theme) IsDark() bool {
	return !t.isLight
}

// Hex returns the resolved hex colour for r, in this Theme's current mode.
func (t Theme) Hex(r Role) string {
	return t.palette().hex(r, t.IsDark())
}
