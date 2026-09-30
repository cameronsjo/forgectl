package redact

import (
	"regexp"
	"strings"
)

// Stdout returns a child's stdout, when that stdout is itself what the
// caller shows (npm outdated's rows, brew's upgrade lines), with every line
// that holds a credential shape replaced whole by Marker (#952). The line
// breaks survive, and every other line is kept as is.
//
// It is narrower than Text on purpose. Text withholds any line holding a
// word that is not Plain, so an '@' is enough: npm's node_modules/@scope
// rows and brew's python@3.12 read [redacted], which is a fair cost for a
// failure's diagnostic text and none for output that is the deliverable.
// Stdout withholds only a line that matches one of these shapes:
//
//   - URL userinfo: a scheme, any run of ':' and '/', then userinfo and '@'
//     (https://u:p@host, http:///U:P@h, http::http://U:P@h, and the
//     scheme-less user:pass@host word). git's own ssh user (ssh://git@host)
//     is not userinfo that carries a credential and is kept;
//   - an HTTP credential header (Authorization:, Proxy-Authorization:,
//     Private-Token:, X-Api-Key:, Cookie:, Set-Cookie:), or the Bearer
//     scheme with 8 or more token bytes, or Basic with 12 or more base64
//     bytes;
//   - a token with a known issued prefix at its issued minimum length and a
//     left word boundary, so task-1234… is not read as sk-… (stdoutTokens,
//     beside tokenPrefixes);
//   - a NAME=value whose NAME reads as a credential (credentialName):
//     GITHUB_TOKEN=…, --password=…;
//   - a PEM private-key block, from its BEGIN line through its END line. A
//     block with no END is withheld to the end of the text. An empty line
//     inside one carries nothing and stays empty.
//
// The unit is the line, for the reason Text gives: a credential can hold a
// space, and the line is the boundary it cannot cross. Nothing inside a line
// is masked.
//
// Text withholds every line Stdout does (Text is lineWithheld or
// stdoutWithheld), so a caller comparing a Text copy against a Stdout copy
// of the same text compares like with like on every line Stdout touches.
// Stdout keeps Marker as it is, so it is idempotent. Callers redact first,
// then escape (termsafe), then cap: a cap that cut a line first could split
// a credential shape so that neither half matches.
func Stdout(s string) string {
	var sc pemScan
	return withholdLines(s, sc.stdoutWithheld)
}

// withholdLines returns s with every line withheld reports true for replaced
// by Marker, calling withheld once per line in order (a pemScan carries
// state from one line to the next). It copies only when a line changes.
func withholdLines(s string, withheld func(line string) bool) string {
	var b strings.Builder
	changed := false
	for rest := s; ; {
		line, next, more := strings.Cut(rest, "\n")
		if withheld(line) {
			if !changed {
				changed = true
				b.Grow(len(s))
				b.WriteString(s[:len(s)-len(rest)])
			}
			b.WriteString(Marker)
		} else if changed {
			b.WriteString(line)
		}
		if !more {
			break
		}
		if changed {
			b.WriteByte('\n')
		}
		rest = next
	}
	if !changed {
		return s
	}
	return b.String()
}

// stdoutUserinfo is a URL's userinfo: a scheme, then one or more ':' and any
// '/' (every slash count git 2.43 still sends, and the transport-helper
// "::"), then userinfo up to '@'. The userinfo is any run without
// whitespace, '/', '?', '#' or '@' except exactly "git", the ssh user of
// ssh://git@host/o/r, spelled as the alternation RE2 needs for "not git":
// four or more bytes, one or two, or three that differ from g, i, t
// somewhere. It is matched only on a line holding an '@' and a ':'.
var stdoutUserinfo = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*:+/*` +
	`(?:[^\s/?#@]{3}[^\s/?#@]+|[^\s/?#@]{1,2}|[^\s/?#@g][^\s/?#@]{2}|g[^\s/?#@i][^\s/?#@]|gi[^\s/?#@t])@`)

// stdoutHeaders are the HTTP credential header names, lowercase. A name
// matches as the end of the hyphenated word before a ':', so Proxy-
// Authorization: and Set-Cookie: match through authorization and cookie.
var stdoutHeaders = []string{"authorization", "private-token", "x-api-key", "cookie"}

// stdoutShape reports whether line holds one of Stdout's single-line shapes.
// Each check is linear in line: RE2 is, headerIn and credentialAssignment
// scan back no further than the previous ':' or '=', and the word-start
// scan reads at most
// a fixed number of bytes at each position (runAtLeast), bar the whitespace
// after a scheme word, which ends before the next word starts.
func stdoutShape(line string) bool {
	// Every URL and header shape holds a ':'.
	if colon := strings.IndexByte(line, ':'); colon >= 0 {
		if strings.IndexByte(line, '@') >= 0 && stdoutUserinfo.MatchString(line) || headerIn(line, colon) {
			return true
		}
	}
	for i := 0; i < len(line); i++ {
		// Every shape below starts a word: task-… is not read as sk-….
		if i > 0 && isWordByte(line[i-1]) {
			continue
		}
		if schemeAt(line[i:]) || tokenAt(line[i:]) {
			return true
		}
	}
	return credentialAssignment(line)
}

// headerIn reports whether a ':' in line at or after from ends a
// stdoutHeaders name: the name, then optional spaces or tabs, then the ':',
// the name starting a word (at the line's start or after a byte that is not
// a letter, digit or '_'). Each scan back stops at the previous ':', so the
// cost is linear in line.
func headerIn(line string, from int) bool {
	for i := from; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		j := i
		for j > 0 && (line[j-1] == ' ' || line[j-1] == '\t') {
			j--
		}
		end := j
		for j > 0 && (isWordByte(line[j-1]) || line[j-1] == '-') {
			j--
		}
		word := line[j:end]
		for _, h := range stdoutHeaders {
			if len(word) < len(h) || !strings.EqualFold(word[len(word)-len(h):], h) {
				continue
			}
			if k := len(word) - len(h); k == 0 || !isWordByte(word[k-1]) {
				return true
			}
		}
	}
	return false
}

// schemeAt reports whether s starts with the Bearer scheme word and a token
// of 8 or more bytes, or the Basic one and 12 or more bytes of base64, any
// case, with whitespace between.
func schemeAt(s string) bool {
	var n int
	var charset func(byte) bool
	switch {
	case len(s) > 6 && strings.EqualFold(s[:6], "bearer"):
		s, n, charset = s[6:], 8, isBearerByte
	case len(s) > 5 && strings.EqualFold(s[:5], "basic"):
		s, n, charset = s[5:], 12, isBase64Byte
	default:
		return false
	}
	rest := strings.TrimLeft(s, " \t\r\v\f")
	return len(rest) < len(s) && runAtLeast(rest, n, charset)
}

// tokenAt reports whether s starts with a stdoutTokens prefix and that
// format's minimum run of token bytes. Slack's xox?- (xoxb-, xoxp- and the
// rest) takes 10 or more letters, digits and '-'.
func tokenAt(s string) bool {
	if len(s) > 4 && strings.HasPrefix(s, "xox") && s[3] >= 'a' && s[3] <= 'z' && s[4] == '-' {
		return runAtLeast(s[5:], 10, func(c byte) bool { return isAlnum(c) || c == '-' })
	}
	if s == "" {
		return false
	}
	for _, tok := range stdoutTokensByFirst[s[0]] {
		if strings.HasPrefix(s, tok.prefix) {
			return runAtLeast(s[len(tok.prefix):], tok.min, tok.charset)
		}
	}
	return false
}

// stdoutTokensByFirst is stdoutTokens by the first byte of the prefix, so
// the word-start scan compares only the few that can match.
var stdoutTokensByFirst = func() map[byte][]stdoutToken {
	m := map[byte][]stdoutToken{}
	for _, tok := range stdoutTokens {
		m[tok.prefix[0]] = append(m[tok.prefix[0]], tok)
	}
	return m
}()

// runAtLeast reports whether s starts with n bytes that are all in charset.
// It reads at most n bytes, so a run of overlapping prefixes stays linear.
func runAtLeast(s string, n int, charset func(byte) bool) bool {
	if len(s) < n {
		return false
	}
	for i := range n {
		if !charset(s[i]) {
			return false
		}
	}
	return true
}

// isWordByte is a byte of RE2's \w: a letter, a digit or '_'.
func isWordByte(c byte) bool { return isAlnum(c) || c == '_' }

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isTokenByte is a letter, a digit, '_' or '-'.
func isTokenByte(c byte) bool { return isAlnum(c) || c == '_' || c == '-' }

func isUpperDigit(c byte) bool { return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' }

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func isBase64Byte(c byte) bool { return isAlnum(c) || c == '+' || c == '/' }

// isBearerByte is a byte of RFC 6750's b64token.
func isBearerByte(c byte) bool {
	return isAlnum(c) || strings.IndexByte("-._~+/=", c) >= 0
}

// pemBegin and pemEnd are a PEM private-key block's armor lines (PKCS#1,
// PKCS#8, EC, DSA, OpenSSH, encrypted, and PGP's PRIVATE KEY BLOCK).
var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
)

// pemScan carries whether the line being scanned sits inside a PEM
// private-key block that an earlier line opened.
type pemScan struct{ inKey bool }

// stdoutWithheld reports whether Stdout withholds line, and moves the PEM
// state past it. A line inside a block, or one that opens or closes one, is
// withheld, unless it is empty. After the line the scan is inside a block
// when its last armor line is a BEGIN; a line with no armor leaves the state
// as it was.
func (sc *pemScan) stdoutWithheld(line string) bool {
	withheld := sc.inKey
	if strings.Contains(line, "-----") {
		begins := pemBegin.FindAllStringIndex(line, -1)
		ends := pemEnd.FindAllStringIndex(line, -1)
		if len(begins) > 0 || len(ends) > 0 {
			withheld = withheld || len(begins) > 0
			lastBegin, lastEnd := -1, -1
			if len(begins) > 0 {
				lastBegin = begins[len(begins)-1][0]
			}
			if len(ends) > 0 {
				lastEnd = ends[len(ends)-1][0]
			}
			sc.inKey = lastBegin > lastEnd
		}
	}
	if withheld {
		// An empty line inside a block carries nothing to withhold.
		return line != ""
	}
	return stdoutShape(line)
}

// credentialAssignment reports whether line holds a NAME=value whose NAME
// reads as a credential (credentialName) and whose value is not empty: NAME
// is the run of name bytes ([A-Za-z0-9._-]) right before the '=', and the
// byte after it is neither whitespace nor the end of the line. Each scan back
// stops at the previous '=', so the cost is linear in line.
func credentialAssignment(line string) bool {
	for i := 0; i < len(line); i++ {
		if line[i] != '=' {
			continue
		}
		if i+1 == len(line) || strings.IndexByte(" \t\r\v\f", line[i+1]) >= 0 {
			continue
		}
		j := i
		for j > 0 && isNameByte(line[j-1]) {
			j--
		}
		if j < i && credentialName(line[j:i]) {
			return true
		}
	}
	return false
}
