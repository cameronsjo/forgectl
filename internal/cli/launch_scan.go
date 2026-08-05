package cli

import "strings"

// tomlLineScanner tracks cross-line TOML lexical state — an open multi-line
// string, an unterminated array — while classifying config.toml lines a
// [launch]-section rewrite must respect. A naive "trimmed line starts with
// [ and ends with ]" check (the scanner this replaces) misreads two classes
// of legal TOML: a table header carrying a trailing comment
// ("[bench] # local collector only" never ends in "]", so the scanner never
// notices the section boundary and drops everything after it — including
// unrelated sections — until some other header happens to parse), and a
// line inside a multi-line string or array that happens to read like
// "[table]" on its own.
//
// This scanner implements a conservative SUBSET of TOML's grammar — enough
// to correctly resolve every construct forgectl itself writes (comments,
// single/double-quoted strings with backslash escapes, inline and
// multi-line arrays, [[launch.project]] tables) — and reports ambiguous
// (ok=false) rather than guess on anything outside that subset. Callers
// MUST fail closed (return an error, write nothing) on an ambiguous result;
// see replaceLaunchSection.
type tomlLineScanner struct {
	inTripleBasic   bool // inside a """ ... """ string, carried across lines
	inTripleLiteral bool // inside a ''' ... ''' string, carried across lines
	bracketDepth    int  // unterminated [ ]/[[ ]] nesting carried across lines
}

// pending reports whether the file ended mid multi-line-string or
// mid-array — an ambiguous EOF the caller must fail on rather than guess
// which lines it dropped or kept.
func (s *tomlLineScanner) pending() bool {
	return s.inTripleBasic || s.inTripleLiteral || s.bracketDepth != 0
}

// scanLine classifies one line against the scanner's carried state. table
// is the bare table name when the line is unambiguously a bare table
// header line outside any open string or array ("[launch.defaults]" →
// "launch.defaults", "[[launch.project]]" → "launch.project"); "" means the
// line isn't a header (a key/value line, a comment, blank, or a line inside
// a string/array continuation). ok reports whether classification
// succeeded — false means the line's syntax falls outside this scanner's
// conservative subset (e.g. an unterminated single-line string) and the
// caller must treat the whole scan as ambiguous.
func (s *tomlLineScanner) scanLine(line string) (table string, ok bool) {
	if s.inTripleBasic || s.inTripleLiteral {
		delim := `"""`
		if s.inTripleLiteral {
			delim = `'''`
		}
		idx := strings.Index(line, delim)
		if idx == -1 {
			return "", true // still inside the string — never a header
		}
		s.inTripleBasic = false
		s.inTripleLiteral = false
		return s.scanLine(line[idx+len(delim):])
	}

	openedMidLine := s.bracketDepth > 0
	runes := []rune(line)
	n := len(runes)
	var content strings.Builder

	for i := 0; i < n; {
		switch r := runes[i]; {
		case r == '#':
			i = n // a comment runs to end of line — stop scanning content
		case matchesAt(runes, i, `"""`):
			rest := runes[i+3:]
			if end := indexOf(rest, `"""`); end != -1 {
				i += 3 + end + 3
				continue
			}
			s.inTripleBasic = true
			i = n
		case matchesAt(runes, i, `'''`):
			rest := runes[i+3:]
			if end := indexOf(rest, `'''`); end != -1 {
				i += 3 + end + 3
				continue
			}
			s.inTripleLiteral = true
			i = n
		case r == '"':
			end := indexStringEnd(runes[i+1:], '"', true)
			if end == -1 {
				return "", false // unterminated single-line string — ambiguous
			}
			content.WriteString(string(runes[i : i+1+end+1]))
			i += 1 + end + 1
		case r == '\'':
			end := indexStringEnd(runes[i+1:], '\'', false)
			if end == -1 {
				return "", false // unterminated single-line string — ambiguous
			}
			content.WriteString(string(runes[i : i+1+end+1]))
			i += 1 + end + 1
		case r == '[' || r == ']':
			if r == '[' {
				s.bracketDepth++
			} else {
				s.bracketDepth--
			}
			content.WriteRune(r)
			i++
		default:
			content.WriteRune(r)
			i++
		}
	}

	if openedMidLine || s.bracketDepth > 0 {
		// Still inside a multi-line array (either it was already open coming
		// into this line, or this line opened one it doesn't close) — never
		// a header, no matter what this line's content looks like.
		return "", true
	}
	if s.bracketDepth < 0 {
		return "", false // an unmatched "]" — not TOML this scanner can trust
	}
	return launchHeaderTable(content.String()), true
}

// launchHeaderTable returns the table name a syntactically-resolved TOML
// header line declares, or "" when it isn't a bare table-header shape.
// Callers MUST pass content with any trailing comment stripped and any
// quoted/string portions already consumed as opaque units — scanLine is the
// only caller, and does exactly that before delegating here.
func launchHeaderTable(content string) string {
	t := strings.TrimSpace(content)
	switch {
	case strings.HasPrefix(t, "[[") && strings.HasSuffix(t, "]]"):
		return strings.TrimSpace(t[2 : len(t)-2])
	case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
		return strings.TrimSpace(t[1 : len(t)-1])
	default:
		return ""
	}
}

// isLaunchTable reports whether a TOML table name belongs to [launch] —
// "launch" itself, or any dotted child ("launch.defaults", "launch.project").
func isLaunchTable(name string) bool {
	return name == "launch" || strings.HasPrefix(name, "launch.")
}

// matchesAt reports whether runes[i:] begins with lit (an ASCII literal).
func matchesAt(runes []rune, i int, lit string) bool {
	litRunes := []rune(lit)
	if i+len(litRunes) > len(runes) {
		return false
	}
	for j, r := range litRunes {
		if runes[i+j] != r {
			return false
		}
	}
	return true
}

// indexOf returns the rune index of lit's first occurrence in runes, or -1.
func indexOf(runes []rune, lit string) int {
	litRunes := []rune(lit)
	for i := 0; i+len(litRunes) <= len(runes); i++ {
		if matchesAt(runes, i, lit) {
			return i
		}
	}
	return -1
}

// indexStringEnd returns the rune index (relative to runes) of the closing
// quote for a single-line string whose opening quote has already been
// consumed, or -1 when runes ends without one. allowEscapes selects TOML
// basic-string semantics (a backslash escapes the following rune, so
// `\"` never terminates the string) versus literal-string semantics (no
// escapes at all — a backslash is just a character).
func indexStringEnd(runes []rune, quote rune, allowEscapes bool) int {
	for i := 0; i < len(runes); i++ {
		if allowEscapes && runes[i] == '\\' {
			i++ // skip the escaped rune too (it can't be the terminator)
			continue
		}
		if runes[i] == quote {
			return i
		}
	}
	return -1
}
