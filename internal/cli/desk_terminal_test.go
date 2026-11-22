// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import "testing"

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
		{"under tmux the variable can be an outer terminal", map[string]string{"HC_TERMINAL_ID": "t7", "TMUX": "/tmp/tmux-501/default,1,0"}, ""},
		{"under screen", map[string]string{"HC_TERMINAL_ID": "t7", "STY": "1.pts"}, ""},
		{"under zellij", map[string]string{"HC_TERMINAL_ID": "t7", "ZELLIJ": "0"}, ""},
		{"an empty TMUX is not tmux", map[string]string{"HC_TERMINAL_ID": "t7", "TMUX": ""}, "t7"},
	}
	for _, c := range cases {
		lookup := func(k string) (string, bool) { v, ok := c.env[k]; return v, ok }
		if got := deskQueuingTerminal(lookup); got != c.want {
			t.Errorf("%s: deskQueuingTerminal = %q, want %q", c.name, got, c.want)
		}
	}
}
