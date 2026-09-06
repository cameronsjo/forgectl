package theme

import "testing"

// TestShouldProbe_ExplicitModeNeverProbes pins that an explicit [theme].mode
// suppresses the terminal background query entirely.
//
// ShouldProbe used to ignore its Mode argument — the parameter was literally
// named `_` — so a config saying `mode = "light"` still probed, and the TUI
// then overwrote the setting with whatever the terminal answered. That defeats
// the one escape hatch documented for the case detection cannot get right: a
// light terminal, where the operator has to say so by hand and expects to be
// believed.
func TestShouldProbe_ExplicitModeNeverProbes(t *testing.T) {
	// The most probe-friendly environment there is: real TTYs, colour allowed,
	// a TERM that forwards the query.
	friendly := Env{StdinTTY: true, StdoutTTY: true, Term: "xterm-256color"}

	if !ShouldProbe(ModeAuto, friendly) {
		t.Fatal("ModeAuto in a friendly environment did not probe; this test could not distinguish the modes")
	}
	for _, m := range []Mode{ModeDark, ModeLight} {
		if ShouldProbe(m, friendly) {
			t.Errorf("mode %v probed the terminal; an explicit mode is an answer, so there is nothing to ask", m)
		}
	}

	// The environment gates still apply under auto.
	for name, env := range map[string]Env{
		"no stdin tty":  {StdoutTTY: true, Term: "xterm"},
		"no stdout tty": {StdinTTY: true, Term: "xterm"},
		"NO_COLOR":      {StdinTTY: true, StdoutTTY: true, Term: "xterm", NoColor: true},
		"inside tmux":   {StdinTTY: true, StdoutTTY: true, Term: "tmux-256color"},
		"inside screen": {StdinTTY: true, StdoutTTY: true, Term: "screen"},
	} {
		if ShouldProbe(ModeAuto, env) {
			t.Errorf("%s: probed when it should not", name)
		}
	}
}
