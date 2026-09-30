package pr

import (
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode"
)

// githubTokenShape matches the published GitHub credential formats: the
// prefixed tokens gh[pousr]_ (personal access, OAuth, user-to-server,
// server-to-server, refresh) and fine-grained personal access tokens,
// github_pat_. The length floors sit below the real lengths (36 characters
// after a gh?_ prefix, 82 after github_pat_) so a token cut short in a paste
// still matches, and above anything prose writes when it merely names the
// prefix. There is no leading word boundary: a token glued to other text is
// still a token.
//
// It is a shape match, not a proof. An agent that encodes a token (base64,
// split across lines, markdown emphasis inside it) passes it; this is the
// tripwire behind the approval gate, not a replacement for it
// (forgectl#681).
var githubTokenShape = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`)

// errReviewHasTokenShape is PostReview's refusal of a drafted review that
// carries a GitHub token shape. Its text never includes the match.
var errReviewHasTokenShape = errors.New("refusing to post the review: it contains text shaped like a GitHub token; " +
	"remove it from the review before posting")

// maxUnescapeRounds bounds the HTML-reference decoding: enough for a
// reference encoded inside another (&amp;#112;) a few times over.
const maxUnescapeRounds = 4

// scanReviewForTokens refuses a review whose text carries a GitHub token
// shape. A prompt-injected reviewer can read gh's stored credentials and
// paste a token into its draft, and a long review makes one easy to miss at
// the approval gate (forgectl#681).
//
// It matches the text as a reader of the posted review would see it
// (normalizeReviewText), so a token split by an invisible character or
// spelled with character references is still caught.
func scanReviewForTokens(review string) error {
	if githubTokenShape.MatchString(review) || githubTokenShape.MatchString(normalizeReviewText(review)) {
		return errReviewHasTokenShape
	}
	return nil
}

// normalizeReviewText undoes the spellings that hide a token from a plain
// match but not from someone reading the rendered review:
//
//   - Unicode format characters (category Cf: zero-width space U+200B, word
//     joiner U+2060, BOM U+FEFF, soft hyphen U+00AD, and the rest), which
//     render as nothing;
//   - HTML character references, named, decimal, and hex (&lowbar;, &#112;,
//     &#x70;), which GitHub's markdown renders as the character, decoded
//     repeatedly so a reference inside another (&amp;#112;) is caught;
//   - markdown backslash escapes of ASCII punctuation (ghp\_…), which render
//     as the bare punctuation.
//
// Format characters are stripped after decoding, since a reference can spell
// one (&#8203;). One inside a reference breaks the reference when it renders,
// so there is nothing to strip before decoding.
func normalizeReviewText(s string) string {
	for range maxUnescapeRounds {
		u := html.UnescapeString(s)
		if u == s {
			break
		}
		s = u
	}
	return unescapeMarkdownPunct(stripFormatChars(s))
}

func stripFormatChars(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// unescapeMarkdownPunct drops a backslash that escapes ASCII punctuation, the
// CommonMark backslash escape.
func unescapeMarkdownPunct(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isASCIIPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}
