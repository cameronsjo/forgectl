package launch

import (
	"fmt"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// IsPermissionMode reports whether v is a Claude Code --permission-mode value
// forgectl knows (claudePermissionRank). It is an exact match: case, spaces,
// and a leading dash all fail, so a value read from disk that passes cannot
// be read as another flag or split into two arguments.
func IsPermissionMode(v string) bool {
	_, ok := claudePermissionRank.level(v)
	return ok
}

// maxCarriedModeEcho caps how much of an unknown transcript value a fallback
// note repeats.
const maxCarriedModeEcho = 32

// CarryPermissionMode returns p with its PermissionMode replaced by mode, the
// mode a session's transcript last recorded, so a resumed session comes back
// in the mode it was in rather than the posture's default (forgectl#1332).
//
// It falls back to p unchanged, with a note saying why, when:
//   - mode is empty (no transcript, or none recorded);
//   - mode is not a known --permission-mode value — transcript text never
//     reaches the argv unchecked;
//   - mode is bypassPermissions and the posture could not reach it, that is,
//     allow_danger is off (or was cleared because stdout is not a terminal)
//     and the posture's own mode is not bypassPermissions. Carrying it then
//     would hand a resume more than the operator's config grants a launch.
//
// On success the note names the carried mode. The note is for stderr; it is
// already safe to print.
func CarryPermissionMode(p Profile, mode string) (Profile, string) {
	fallback := func(why string) (Profile, string) {
		return p, fmt.Sprintf("%s; resuming in the posture's %s", why, p.PermissionMode)
	}
	switch {
	case mode == "":
		return fallback("no permission mode recorded in the session's transcript")
	case !IsPermissionMode(mode):
		return fallback("the transcript's last permission mode " + termsafe.QuoteArgMax(mode, maxCarriedModeEcho) + " is not one forgectl knows")
	case mode == "bypassPermissions" && !p.AllowDanger && p.PermissionMode != "bypassPermissions":
		return fallback("the session was in bypassPermissions, which this posture cannot reach (allow_danger is off here, or was cleared because stdout is not a terminal)")
	}
	p.PermissionMode = mode
	return p, "resuming in the session's last permission mode, " + mode
}
