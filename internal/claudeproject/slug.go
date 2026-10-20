// Package claudeproject holds Claude Code's on-disk project naming. It
// imports nothing from forgectl, so any package may use it, including the
// surface adapters that the backend barrier test keeps away from
// internal/launch.
package claudeproject

import "strings"

// Slug reproduces Claude Code's project-directory encoding: every character
// outside [A-Za-z0-9] becomes '-', so /Users/x/.dotfiles is -Users-x--dotfiles.
func Slug(dir string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, dir)
}
