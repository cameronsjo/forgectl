// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import "github.com/cameronsjo/forgectl/internal/surface/hcadapter"

// deskQueuingTerminal is the herdr-app (herdr-core) terminal desk add stamps
// into an item, so a viewer such as the notch can take you back to the
// session that queued it: $HC_TERMINAL_ID when it has the shape of a local hc
// address. Under tmux, screen or zellij it is "": a multiplexer copies its
// starting environment into every pane, so the variable can name an outer
// terminal every pane shares (the same reading progstatus makes). This guards
// against a stale value, not a hostile caller: the variable is the caller's
// own environment.
func deskQueuingTerminal(lookup func(string) (string, bool)) string {
	for _, k := range []string{"TMUX", "STY", "ZELLIJ"} {
		if v, ok := lookup(k); ok && v != "" {
			return ""
		}
	}
	v, ok := lookup("HC_TERMINAL_ID")
	if !ok || !hcadapter.ValidAddress(v) {
		return ""
	}
	return v
}
