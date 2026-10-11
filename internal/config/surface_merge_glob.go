package config

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The [surface.merge] paths grammar. A glob is a '/'-separated list of
// segments, matched segment by segment against a changed path's exact bytes,
// case-sensitively:
//
//   - a segment without '*' matches that segment exactly;
//   - a segment with '*' matches any one segment, each '*' standing for any
//     run of bytes (none included) within the segment;
//   - "**" as a whole segment matches zero or more whole segments, and may
//     appear only after at least one literal segment (no wildcard in it).
//
// A glob that is "*" or "**", that starts with "**", or that holds "**"
// inside a longer segment is refused, and so is anything a changed path
// itself may not hold: an empty, "." or ".." segment, a leading '/', a
// backslash, a control byte, a non-ASCII byte or invalid UTF-8, or one of
// '?', '[', ']', '{', '}' (the grammar has no other metacharacters, so these
// would only mislead).

// maxMergeGlobLen bounds a glob.
const maxMergeGlobLen = 200

// CheckMergeGlob refuses a glob outside the grammar above, naming why.
func CheckMergeGlob(g string) error {
	quoted := quoteConfigValue(g)
	if g == "" || len(g) > maxMergeGlobLen {
		return fmt.Errorf("want a glob of 1-%d bytes, got %s", maxMergeGlobLen, quoted)
	}
	if g == "*" || g == "**" {
		return fmt.Errorf("a bare %s matches every path; name a directory first, got %s", g, quoted)
	}
	if err := checkPathBytes(g); err != nil {
		return fmt.Errorf("%w, got %s", err, quoted)
	}
	if strings.ContainsAny(g, "?[]{}") {
		return fmt.Errorf("the only wildcards are '*' and a whole-segment '**', got %s", quoted)
	}
	segs := strings.Split(g, "/")
	literalSeen := false
	for _, s := range segs {
		switch {
		case s == "**":
			if !literalSeen {
				return fmt.Errorf("'**' may only follow at least one literal segment, got %s", quoted)
			}
		case strings.Contains(s, "**"):
			return fmt.Errorf("'**' must be a whole segment, got %s", quoted)
		case !strings.Contains(s, "*"):
			literalSeen = true
		}
	}
	return nil
}

// CheckChangedPath refuses a changed path the merge policy will not judge:
// one with an empty, "." or ".." segment, a leading '/', a backslash, a
// control byte (including DEL), a non-ASCII byte, or invalid UTF-8.
func CheckChangedPath(p string) error {
	if p == "" {
		return errors.New("the path is empty")
	}
	return checkPathBytes(p)
}

// checkPathBytes is the byte and segment floor CheckMergeGlob and
// CheckChangedPath share.
func checkPathBytes(p string) error {
	if strings.HasPrefix(p, "/") {
		return errors.New("a path may not start with '/'")
	}
	if strings.Contains(p, `\`) {
		return errors.New("a path may not hold a backslash")
	}
	if !utf8.ValidString(p) {
		return errors.New("a path must be valid UTF-8")
	}
	for i := 0; i < len(p); i++ {
		if b := p[i]; b < 0x20 || b == 0x7f {
			return fmt.Errorf("a path may not hold the control byte 0x%02x", b)
		}
		// A case-folding filesystem can fold a non-ASCII rune onto an ASCII
		// one (U+017F long s onto 's' on APFS), so a path spelled with one
		// could land on a refused file while matching none of the refusals.
		if p[i] >= utf8.RuneSelf {
			return fmt.Errorf("a path may hold only ASCII, got the byte 0x%02x", p[i])
		}
	}
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "":
			return errors.New("a path may not hold an empty segment")
		case ".", "..":
			return fmt.Errorf("a path may not hold a %q segment", s)
		}
	}
	return nil
}

// MatchMergeGlob reports whether path matches glob, segment-wise and
// case-sensitively. Both are assumed to have passed their checks; a glob
// that did not never matches.
func MatchMergeGlob(glob, path string) bool {
	if CheckMergeGlob(glob) != nil {
		return false
	}
	return matchSegments(strings.Split(glob, "/"), strings.Split(path, "/"))
}

func matchSegments(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			rest := g[1:]
			for i := 0; i <= len(p); i++ {
				if matchSegments(rest, p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 || !matchSegment(g[0], p[0]) {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}

// matchSegment matches one segment, where each '*' is any run of bytes.
func matchSegment(g, s string) bool {
	parts := strings.Split(g, "*")
	if len(parts) == 1 {
		return g == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return len(s) >= len(last) && strings.HasSuffix(s, last)
}
