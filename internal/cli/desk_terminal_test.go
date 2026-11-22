// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
)

func TestDeskQueuingTerminal(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"herdr-app terminal", map[string]string{"HC_TERMINAL_ID": "t148"}, "t148"},
		{"not in hc", map[string]string{}, ""},
		{"not an address", map[string]string{"HC_TERMINAL_ID": "rohan/t7"}, ""},
		{"a name, not an address", map[string]string{"HC_TERMINAL_ID": "reviewer"}, ""},
		{"a herdr 0.9 pane inside an hc terminal", map[string]string{"HC_TERMINAL_ID": "t7", "HERDR_PANE_ID": "w1:p2"}, ""},
		{"under tmux the variable can be an outer terminal", map[string]string{"HC_TERMINAL_ID": "t7", "TMUX": "/tmp/tmux-501/default,1,0"}, ""},
		{"under screen", map[string]string{"HC_TERMINAL_ID": "t7", "STY": "1.pts"}, ""},
		{"under zellij", map[string]string{"HC_TERMINAL_ID": "t7", "ZELLIJ": "0"}, ""},
		{"an empty TMUX is not tmux", map[string]string{"HC_TERMINAL_ID": "t7", "TMUX": ""}, "t7"},
		{"an HC_SESSION hc would refuse", map[string]string{"HC_TERMINAL_ID": "t7", "HC_SESSION": "a b"}, ""},
	}
	for _, c := range cases {
		lookup := func(k string) (string, bool) { v, ok := c.env[k]; return v, ok }
		term, sel := deskQueuingTerminal(lookup)
		if term != c.want {
			t.Errorf("%s: terminal = %q, want %q", c.name, term, c.want)
		}
		if (term == "") != (sel == "") || (sel != "" && !hcadapter.ValidSelection(sel)) {
			t.Errorf("%s: selection %q does not go with terminal %q", c.name, sel, term)
		}
	}
}

// The selection follows the core: another HC_SESSION, or another socket, is
// another core, and "default" is the same core as an unset HC_SESSION.
func TestDeskQueuingTerminalRecordsTheCore(t *testing.T) {
	sel := func(env map[string]string) string {
		env["HC_TERMINAL_ID"] = "t7"
		_, s := deskQueuingTerminal(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		return s
	}
	base := sel(map[string]string{"HOME": "/Users/x"})
	if base != sel(map[string]string{"HOME": "/Users/x", "HC_SESSION": "default"}) {
		t.Error("HC_SESSION=default is a different selection from unset")
	}
	if base == sel(map[string]string{"HOME": "/Users/x", "HC_SESSION": "work"}) {
		t.Error("a named core has the default core's selection")
	}
	if base == sel(map[string]string{"HOME": "/Users/x", "HC_SOCKET": "/tmp/other.sock"}) {
		t.Error("another socket has the same selection")
	}
}
