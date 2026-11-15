// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package herdr

import "strings"

// Host is the terminal host the current process runs in.
type Host int

const (
	// HostNone means the process is in neither a herdr pane nor a herdr-app
	// terminal.
	HostNone Host = iota
	// HostHerdr is a herdr 0.9 pane: HERDR_ENV, HERDR_SOCKET_PATH or
	// HERDR_PANE_ID is set. The [Client] talks to it.
	HostHerdr
	// HostApp is a herdr-app terminal: HC_TERMINAL_ID is set and no HERDR_*
	// variable is. The [AppClient] talks to it.
	HostApp
)

func (h Host) String() string {
	switch h {
	case HostHerdr:
		return "herdr"
	case HostApp:
		return "herdr-app"
	}
	return "none"
}

// envTerminal is what hc exports into every terminal it runs; herdr-app
// addresses a terminal by it (it is not a pane id).
const envTerminal = "HC_TERMINAL_ID"

// DetectHost reports which host the environment describes. It is pure: it
// reads only through lookupEnv.
//
// When both sets of variables are present, the herdr 0.9 host wins. A process
// that carries HERDR_* was started in (or inside) a herdr 0.9 pane, and that
// path predates herdr-app; keeping it first leaves every 0.9 session behaving
// exactly as it did. A variable set to the empty string counts as unset for
// HC_TERMINAL_ID, but any HERDR_* variable that is set at all selects herdr,
// so [CheckSession] reports its precise complaint (a wrong HERDR_ENV value,
// say) instead of this function guessing.
func DetectHost(lookupEnv func(string) (string, bool)) Host {
	for _, k := range []string{envSession, envSocket, envPane} {
		if _, ok := lookupEnv(k); ok {
			return HostHerdr
		}
	}
	if v, ok := lookupEnv(envTerminal); ok && strings.TrimSpace(v) != "" {
		return HostApp
	}
	return HostNone
}
