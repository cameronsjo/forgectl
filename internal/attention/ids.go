package attention

import "regexp"

var (
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	paneRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,63}$`)
)

// ValidSession reports whether s has the shape of a Claude Code session id.
func ValidSession(s string) bool { return sessionRe.MatchString(s) }

// ValidPane reports whether s is a herdr pane id (herdr-bridge's ID_RE).
func ValidPane(s string) bool { return paneRe.MatchString(s) }
