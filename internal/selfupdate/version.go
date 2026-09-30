package selfupdate

import (
	"fmt"
	"regexp"
	"strings"
)

// versionToken is the whole shape of a version doctor and upgrade render: a
// dotted release number of two to four parts, optionally after a "v", with an
// optional Homebrew revision (_N), a short pre-release, and short build
// metadata. It is matched against a WHOLE token (see FindVersions), never
// searched for inside one, and the charset and lengths are fixed, so a match
// carries nothing the tool chose beyond a version.
var versionToken = regexp.MustCompile(`^v?([0-9]{1,6}(?:\.[0-9]{1,6}){1,3}(?:_[0-9]{1,4})?(?:-[0-9A-Za-z]{1,12}(?:\.[0-9A-Za-z]{1,12}){0,3})?(?:\+[0-9A-Za-z]{1,12}(?:\.[0-9A-Za-z]{1,12}){0,3})?)$`)

// isTokenRune reports whether r can be part of a version token. Everything
// else (space, parentheses, comparison signs, "/", ",") separates tokens.
func isTokenRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
		r == '.' || r == '_' || r == '-' || r == '+'
}

// FindVersions returns every version in s, without any "v" prefix. s is cut
// into tokens at every rune a version cannot hold, a trailing sentence period
// is dropped, and a token counts only when versionToken matches it whole: a
// Homebrew revision (0.9.0_1), a five-part number (1.2.3.4.5), an over-long
// suffix or a version glued to other text yields nothing, never a truncated
// piece of itself (#738).
func FindVersions(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return !isTokenRune(r) }) {
		if m := versionToken.FindStringSubmatch(strings.TrimRight(tok, ".")); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// OutdatedDetail words CheckOutdated's detail from the version numbers it
// contains, never from its text (#716, #738): brew's output relays what the
// tap sends. brew's verbose form is "<cask> (<installed>) != <latest>"; any
// other shape, including the terse cask-name-only form, reads as a plain
// "newer version available".
func OutdatedDetail(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	if vs := FindVersions(line); len(vs) == 2 {
		return fmt.Sprintf("forgectl %s installed, %s available", vs[0], vs[1])
	}
	return "a newer forgectl is available"
}

// UpgradedVersions reads the from and to versions out of Upgrade's output from
// version tokens only, never from its text (#761): brew reports the cask it
// upgraded as "<cask> <from> -> <to>". It takes the last line that names
// forgectl and holds exactly two versions, since the upgrade step's output
// follows brew update's. ok is false when no such line exists (brew changed
// its wording, or had nothing to upgrade).
func UpgradedVersions(out string) (from, to string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "forgectl") {
			continue
		}
		if vs := FindVersions(line); len(vs) == 2 {
			from, to, ok = vs[0], vs[1], true
		}
	}
	return from, to, ok
}
