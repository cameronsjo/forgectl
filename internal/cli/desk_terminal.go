// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
)

// deskQueuingTerminal is the herdr-app (herdr-core) terminal desk add stamps
// into an item, so a viewer such as the notch can take you back to the
// session that queued it, and the selection of the hc core that address
// belongs to (hcadapter.Selection): an address is only unique within one
// core. Both are "" unless this process is a herdr-app terminal by the repo's
// own host detection (herdr.DetectHost: HC_TERMINAL_ID set and none of the
// HERDR_* variables, so a herdr 0.9 pane inside an hc terminal does not stamp
// the outer terminal), the address has the shape of a local hc address, and
// no tmux, screen or zellij sits in between (a multiplexer copies its
// starting environment, and with it an outer terminal's id, into every
// pane). This is progstatus's reading of the same variables. It guards
// against a stale value, not a hostile caller: the variables are the caller's
// own environment.
func deskQueuingTerminal(lookup func(string) (string, bool)) (terminal, selection string) {
	if herdr.DetectHost(lookup) != herdr.HostApp {
		return "", ""
	}
	for _, k := range []string{"TMUX", "STY", "ZELLIJ"} {
		if v, ok := lookup(k); ok && v != "" {
			return "", ""
		}
	}
	v, _ := lookup("HC_TERMINAL_ID")
	if !hcadapter.ValidAddress(v) {
		return "", ""
	}
	sel, err := hcadapter.Selection(func(k string) string { s, _ := lookup(k); return s })
	if err != nil {
		return "", ""
	}
	return v, sel
}
