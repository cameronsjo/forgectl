package organize

import (
	"fmt"
	"regexp"
	"strings"
)

// Match reports whether s matches glob with Python fnmatch.fnmatchcase
// semantics, which the script it replaces used: `*` matches any run of
// characters including `/`, `?` matches one character, `[abc]` and `[a-c]`
// match a class, `[!x]` negates it, an unclosed `[` is a literal, and
// everything else is literal. Matching is case-sensitive and anchored.
//
// A class the regexp engine rejects (a reversed range such as `[z-a]`)
// matches nothing.
func Match(glob, s string) bool {
	re, err := regexp.Compile(translate(glob))
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// CheckGlob reports why a glob can never match anything, or nil. Match treats
// such a glob as matching nothing; a config check calls this so the operator
// hears about it instead of watching every tab fall through to the default.
func CheckGlob(glob string) error {
	if _, err := regexp.Compile(translate(glob)); err != nil {
		return fmt.Errorf("invalid glob: %w", err)
	}
	return nil
}

// translate converts a glob to an anchored regexp source.
func translate(glob string) string {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	runes := []rune(glob)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '[':
			j := i + 1
			if j < len(runes) && runes[j] == '!' {
				j++
			}
			if j < len(runes) && runes[j] == ']' {
				j++
			}
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j >= len(runes) {
				b.WriteString(`\[`)
				continue
			}
			b.WriteString(translateClass(runes[i+1 : j]))
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`$`)
	return b.String()
}

// translateClass renders the inside of a [...] class.
func translateClass(inner []rune) string {
	var b strings.Builder
	b.WriteByte('[')
	if len(inner) > 0 && inner[0] == '!' {
		b.WriteByte('^')
		inner = inner[1:]
	} else if len(inner) > 0 && inner[0] == '^' {
		b.WriteString(`\^`)
		inner = inner[1:]
	}
	for _, c := range inner {
		switch c {
		case '\\', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	b.WriteByte(']')
	return b.String()
}
