package redact

import (
	"bytes"
	"encoding/base64"
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
//     scheme-less user:pass@host word), and a password after user: that
//     holds a raw '/', '?' or '#', which git echoes as it is
//     (http://u:pa/ss@h, #974). git's own ssh user (ssh://git@host) is not
//     userinfo that carries a credential and is kept, and neither is a
//     host:port/path@… (the part after ':' starts with digits and a '/');
//   - an HTTP credential header (Authorization:, Proxy-Authorization:,
//     Private-Token:, X-Api-Key:, Cookie:, Set-Cookie:, and every name
//     credentialHeaderName accepts, as Text does: X-Auth-Token:,
//     X-GitHub-Token:), or the Bearer scheme with 8 or more token bytes, or
//     Basic with 12 or more base64 bytes;
//   - a token with a known issued prefix at its issued minimum length and a
//     left word boundary, so task-1234… is not read as sk-… (stdoutTokens,
//     beside tokenPrefixes), and a JWT (stdoutJWT);
//   - a NAME=value whose NAME reads as a credential (credentialName):
//     GITHUB_TOKEN=…, --password=…; the flag with its value after a space
//     (--password abc), as Text does; a JSON "NAME": value (or Python's
//     'NAME': value) anywhere in the line, and a YAML NAME: value, a TOML
//     NAME = value or a Go NAME := value as the line's key (#974, #983),
//     unless the value is empty (keyValueCarries); and the lines after a
//     YAML NAME: whose value starts on the next line (password: |), while
//     they are indented past it (pemScan.inValue, #983);
//   - a PEM private-key block, from its BEGIN line through its END line. A
//     block with no END is withheld to the end of the text. An empty line
//     inside one carries nothing and stays empty.
//
// A line holding an ANSI escape is checked as it is and again with its CSI
// and OSC sequences stripped (stripEscapes), so a token split by a color code
// or an OSC 8 hyperlink is seen the way a terminal shows it (#974, #983).
//
// Stdout makes exemptions Text does not, for false positives on output
// that is the deliverable (#974, #983, pemScan.deliverable):
//
//   - basic or bearer in prose: a scheme word not spelled Basic, BASIC,
//     Bearer or BEARER, followed by a run with no digit, '+', '/' or '=',
//     that for bearer is under 16 bytes and for basic does not decode as
//     base64 to text holding a ':' (basicCredential);
//   - a NAME=value, header or key whose value is a status word (benignValue:
//     auth=ok, Cookie: none), or a pass= count of one to three digits beside
//     a fail= count (pass=1 fail=0, benignAssignment);
//   - a key whose value is a status such as expired or required
//     (keyStatusValues) or a home or system path (pathValue), or whose name
//     ends in a descriptor (token_type, password_policy, credential.helper:
//     keyNameCarries);
//   - a credential flag followed by a word of prose (use --token to pass it:
//     flagProse);
//   - a git diffstat row whose path is a file path, which skips the
//     NAME=value shape alone ( src/token=x.go | 1 +, diffstatPath);
//   - a container image pinned by digest, which is no userinfo
//     (docker.io/library/node:22@sha256:…, withoutDigestRefs).

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
	sc := pemScan{deliverable: true}
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
// somewhere. Or it is a user with none of those bytes and no ':', a ':',
// then a password that may hold a raw '/', '?' or '#' (#974: git 2.43
// echoes http://u:pa/ss@h/r whole in its error) but does not start with
// digits and one of them, which is a port and a path
// (http://registry:4873/@scope/pkg). It is matched only on a line holding an
// '@' and a ':'.
var stdoutUserinfo = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*:+/*` +
	`(?:[^\s/?#@]{3}[^\s/?#@]+|[^\s/?#@]{1,2}|[^\s/?#@g][^\s/?#@]{2}|g[^\s/?#@i][^\s/?#@]|gi[^\s/?#@t]` +
	`|[^\s/?#@:]+:(?:[^\s@0-9]|[0-9]+[^\s@/?#0-9])[^\s@]*)@`)

// stdoutJWT is a JSON Web Token at the start of a word: "eyJ" (a base64url
// '{"'), 7 or more base64url bytes of header, a '.', 4 or more of
// payload, and a second '.' (the signature may be empty, alg none). JWE's
// five segments start the same way.
var stdoutJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{7,}\.[A-Za-z0-9_-]{4,}\.`)

// stdoutHeaders are the HTTP credential header names, lowercase. A name
// matches as the end of the hyphenated word before a ':', so Proxy-
// Authorization: and Set-Cookie: match through authorization and cookie.
var stdoutHeaders = []string{"authorization", "private-token", "x-api-key", "cookie"}

// stdoutShape reports whether line holds one of Stdout's single-line shapes,
// as it is or, when it holds an ESC, with its CSI and OSC sequences stripped
// (a token split by a color code or an OSC 8 hyperlink). Both are checked,
// so stripping can only add a match. deliverable makes Stdout's exemptions
// (pemScan.deliverable).
func stdoutShape(line string, deliverable bool) bool {
	if lineShape(line, deliverable) {
		return true
	}
	if strings.IndexByte(line, 0x1b) < 0 {
		return false
	}
	return lineShape(stripEscapes(line), deliverable)
}

// lineShape reports whether line holds one of Stdout's single-line shapes.
// Each check is linear in line: RE2 is, headerIn, jsonKeyIn and
// credentialAssignment scan back no further than the previous ':' or '=',
// and forward over a bounded value or a run the next check does not re-read;
// yamlKey, assignKey, flagValueIn and withoutDigestRefs read the line once;
// and the word-start scan reads at most a fixed number of bytes at each
// position (runAtLeast), bar a scheme word's whitespace and run, which end
// before the next scheme word starts.
func lineShape(line string, deliverable bool) bool {
	// Every URL, header and key shape holds a ':'.
	if colon := strings.IndexByte(line, ':'); colon >= 0 {
		if userinfoIn(line, deliverable) ||
			headerIn(line, colon, deliverable) || jsonKeyIn(line, colon, deliverable) || yamlKey(line, deliverable) {
			return true
		}
	}
	if strings.Contains(line, "eyJ") && stdoutJWT.MatchString(line) {
		return true
	}
	for i := 0; i < len(line); i++ {
		// Every shape below starts a word: task-… is not read as sk-….
		if i > 0 && isWordByte(line[i-1]) {
			continue
		}
		if schemeAt(line[i:], deliverable) || tokenAt(line[i:]) {
			return true
		}
	}
	return flagValueIn(line, deliverable) || credentialAssignment(line, deliverable) || assignKey(line, deliverable)
}

// userinfoIn reports whether line holds URL userinfo (stdoutUserinfo). When
// deliverable, a container image pinned by digest
// (docker.io/library/node:22@sha256:<64 hex>) is no userinfo: its name:tag
// before the '@' reads as user:pass, so each such word is blanked first
// (withoutDigestRefs, #983).
func userinfoIn(line string, deliverable bool) bool {
	if strings.IndexByte(line, '@') < 0 {
		return false
	}
	if deliverable && strings.Contains(line, "@sha256:") {
		line = withoutDigestRefs(line)
	}
	return stdoutUserinfo.MatchString(line)
}

// withoutDigestRefs returns line with every whitespace-separated word that
// is an image reference pinned by digest replaced by a space: a name of
// letters, digits, '.', '_', '/', ':' and '-' (no "://" or "::"), one '@',
// then "sha256:" and exactly 64 lowercase hex digits ending the word. It
// reads line once.
func withoutDigestRefs(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		if isSpaceByte(line[i]) {
			b.WriteByte(line[i])
			i++
			continue
		}
		start := i
		for i < len(line) && !isSpaceByte(line[i]) {
			i++
		}
		if w := line[start:i]; digestRef(w) {
			b.WriteByte(' ')
		} else {
			b.WriteString(w)
		}
	}
	return b.String()
}

// digestRef reports whether w is name@sha256:<64 lowercase hex>, the name
// as withoutDigestRefs describes it.
func digestRef(w string) bool {
	name, digest, ok := strings.Cut(w, "@sha256:")
	if !ok || name == "" || len(digest) != 64 || strings.Contains(name, "://") || strings.Contains(name, "::") {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !isNameByte(c) && c != '/' && c != ':' {
			return false
		}
	}
	for i := 0; i < len(digest); i++ {
		if c := digest[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// stripEscapes returns line without its ANSI CSI sequences (ESC '[',
// parameter bytes 0x30-0x3f, intermediate bytes 0x20-0x2f and one final byte
// 0x40-0x7e; SGR color codes are the common case) and its OSC sequences (ESC
// ']' through BEL or ESC '\'; an OSC 8 hyperlink is the common case, #983). An
// ESC that starts no complete sequence is kept. Once an OSC finds no
// terminator, none after it can, so the search for one runs at most once
// past each byte and the cost stays linear.
func stripEscapes(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	oscMayEnd := true
	for i := 0; i < len(line); {
		if line[i] != 0x1b || i+1 >= len(line) {
			b.WriteByte(line[i])
			i++
			continue
		}
		switch line[i+1] {
		case '[':
			if end := csiEnd(line, i+2); end >= 0 {
				i = end
				continue
			}
		case ']':
			if oscMayEnd {
				if end := oscEnd(line, i+2); end >= 0 {
					i = end
					continue
				}
				oscMayEnd = false
			}
		}
		b.WriteByte(line[i])
		i++
	}
	return b.String()
}

// csiEnd returns the index just past the final byte of a CSI sequence whose
// parameter bytes start at from, or -1 when none completes it.
func csiEnd(line string, from int) int {
	j := from
	for j < len(line) && line[j] >= 0x30 && line[j] <= 0x3f {
		j++
	}
	for j < len(line) && line[j] >= 0x20 && line[j] <= 0x2f {
		j++
	}
	if j >= len(line) || line[j] < 0x40 || line[j] > 0x7e {
		return -1
	}
	return j + 1
}

// oscEnd returns the index just past the BEL or ESC '\' that ends an OSC
// sequence whose body starts at from, or -1 when nothing ends it.
func oscEnd(line string, from int) int {
	for k := from; k < len(line); k++ {
		switch {
		case line[k] == 0x07:
			return k + 1
		case line[k] == 0x1b && k+1 < len(line) && line[k+1] == '\\':
			return k + 2
		}
	}
	return -1
}

// headerIn reports whether a ':' in line at or after from ends a
// stdoutHeaders name or one credentialHeaderName accepts: the name, then
// optional spaces or tabs, then the ':', the name starting a word (at the
// line's start or after a byte that is not a letter, digit or '_'). When
// deliverable, a header whose whole value is a status word (Cookie: none) is
// not one. Each scan back stops at the previous ':', so the cost is linear
// in line.
func headerIn(line string, from int, deliverable bool) bool {
	// The end of line's text, found once so each header's benign check reads
	// only its own value, not the tail every later ':' shares.
	textEnd := len(strings.TrimRight(line, " \t\r\v\f"))
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
		if !isCredentialHeader(word) {
			continue
		}
		if deliverable && benignRest(line, i+1, textEnd) {
			continue
		}
		return true
	}
	return false
}

// isCredentialHeader reports whether word, the hyphenated word before a ':',
// ends in a stdoutHeaders name that starts a word, or is a name
// credentialHeaderName accepts.
func isCredentialHeader(word string) bool {
	for _, h := range stdoutHeaders {
		if len(word) < len(h) || !strings.EqualFold(word[len(word)-len(h):], h) {
			continue
		}
		if k := len(word) - len(h); k == 0 || !isWordByte(word[k-1]) {
			return true
		}
	}
	return credentialHeaderName(strings.ToLower(word))
}

// jsonKeyIn reports whether a ':' in line at or after from ends a JSON key
// whose name reads as a credential ("token": "…", {"access_token":"…"}), or
// a Python dict key in single quotes ({'token': 'abc'}, #983): the ':'
// follows optional spaces and a quoted run of name bytes, both quotes the
// same, and the value after it carries something (keyValueCarries). Each scan
// back stops at the byte before the opening quote, which is not a name byte,
// so the cost is linear in line.
func jsonKeyIn(line string, from int, deliverable bool) bool {
	for i := from; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		j := i
		for j > 0 && (line[j-1] == ' ' || line[j-1] == '\t') {
			j--
		}
		if j == 0 || line[j-1] != '"' && line[j-1] != '\'' {
			continue
		}
		quote := line[j-1]
		end := j - 1
		k := end
		for k > 0 && isNameByte(line[k-1]) {
			k--
		}
		if k == end || k == 0 || line[k-1] != quote || !keyNameCarries(line[k:end], deliverable) {
			continue
		}
		v, short := shortJSONValue(line[i+1:])
		if !short || keyValueCarries(v, deliverable) {
			return true
		}
	}
	return false
}

// maxShortValue bounds how far a value is read to decide it carries
// nothing: every value keyValueCarries and benignValue accept is shorter.
const maxShortValue = 12

// shortJSONValue returns the JSON value at the start of s, after spaces and
// tabs, when it is short: a string in double quotes, or single ones as Python
// writes it, whose closing quote comes within maxShortValue bytes, quotes
// included, or an unquoted run that ends at ',', '}', ']', whitespace or the
// end of s within that bound. short is false for any longer value, which
// carries something. It reads no more than the leading blanks, which belong
// to this key alone, and maxShortValue bytes, so a line of many keys stays
// linear.
func shortJSONValue(s string) (v string, short bool) {
	s = strings.TrimLeft(s, " \t")
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:min(len(s), maxShortValue)], s[0]); end >= 0 {
			return s[:end+2], true
		}
		return "", false
	}
	if strings.HasPrefix(s, "[]") {
		return "[]", true
	}
	for i := 0; i < len(s) && i <= maxShortValue; i++ {
		if strings.IndexByte(",}] \t\r", s[i]) >= 0 {
			return s[:i], true
		}
	}
	if len(s) <= maxShortValue {
		return s, true
	}
	return "", false
}

// yamlKey reports whether line is a YAML mapping entry whose key reads as a
// credential (password: …, - token: …; yamlEntry) and whose value carries
// something (keyValueCarries). Only the line's key counts, so prose such as
// "error validating token: expired" is kept. A value that starts on the next
// line (password: followed by an indented line, or a block scalar's '|') is
// pemScan's: the key line carries nothing, the lines after it do.
func yamlKey(line string, deliverable bool) bool {
	e, ok := yamlEntry(line)
	return ok && keyNameCarries(e.name, deliverable) && keyValueCarries(e.value, deliverable)
}

// yamlKeyEntry is one YAML mapping entry: its key, its value with the blanks
// and a trailing ',' trimmed, the indentation of the line, and whether the
// key follows a sequence's "- ".
type yamlKeyEntry struct {
	name, value string
	indent      int
	dashed      bool
}

// yamlEntry parses line as a YAML mapping entry: optional indentation, an
// optional "- ", the key as name bytes (optionally quoted), optional spaces,
// then a ':' that ends the line or is followed by whitespace (a CRLF line's
// '\r' included). It reads line once.
func yamlEntry(line string) (yamlKeyEntry, bool) {
	var e yamlKeyEntry
	s := strings.TrimLeft(line, " \t")
	e.indent = len(line) - len(s)
	if rest, ok := strings.CutPrefix(s, "- "); ok {
		s, e.dashed = strings.TrimLeft(rest, " \t"), true
	}
	quote := byte(0)
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		quote, s = s[0], s[1:]
	}
	n := 0
	for n < len(s) && isNameByte(s[n]) {
		n++
	}
	if n == 0 {
		return e, false
	}
	e.name, s = s[:n], s[n:]
	if quote != 0 {
		if s == "" || s[0] != quote {
			return e, false
		}
		s = s[1:]
	}
	s = strings.TrimLeft(s, " \t")
	if s == "" || s[0] != ':' || len(s) > 1 && s[1] != ' ' && s[1] != '\t' && s[1] != '\r' {
		return e, false
	}
	e.value = strings.TrimRight(strings.TrimLeft(s[1:], " \t"), " \t\r,")
	return e, true
}

// valueOpener reports whether line is a YAML credential key whose value
// starts on the next line: nothing after the ':', or a block scalar's
// indicator ('|' or '>' with its chomping and indent), once its node
// properties and a comment are set aside (openerValue: password: # db,
// password: !!binary |, password: &pw). It returns the entry, whose
// indentation bounds the lines that hold the value (pemScan). The key line
// itself is yamlKey's, which reads its value as written, so a line such as
// password: &pw is still withheld on its own.
func valueOpener(line string, deliverable bool) (yamlKeyEntry, bool) {
	e, ok := yamlEntry(line)
	if !ok || !keyNameCarries(e.name, deliverable) {
		return e, false
	}
	v := openerValue(e.value)
	if v == "" || (v[0] == '|' || v[0] == '>') && strings.Trim(v[1:], "+-0123456789") == "" {
		return e, true
	}
	return e, false
}

// openerValue returns v, a YAML value as yamlEntry trims it, without its
// leading node properties (a !tag or an &anchor, each ending at a blank) and
// without a comment ('#' at its start or after a blank, to the end). It
// reads v once.
func openerValue(v string) string {
	for v != "" && (v[0] == '!' || v[0] == '&') {
		end := strings.IndexAny(v, " \t")
		if end < 0 {
			return ""
		}
		v = strings.TrimLeft(v[end:], " \t")
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '#' && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimRight(v[:i], " \t")
		}
	}
	return v
}

// assignKey reports whether line is an assignment, as the line's key, whose
// name reads as a credential and whose value carries something
// (keyValueCarries): TOML's password = "x" and token = '…', and Go's
// token := "abc" (#983). The name is name bytes, optionally quoted, after the
// line's indentation; then ":=", or '=' (not "==") with blanks or a closing
// quote before it or a blank after it (password= "x"), since a bare
// NAME=value is credentialAssignment's, with its own exemptions. It reads
// line once.
func assignKey(line string, deliverable bool) bool {
	s := strings.TrimLeft(line, " \t")
	quote := byte(0)
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		quote, s = s[0], s[1:]
	}
	n := 0
	for n < len(s) && isNameByte(s[n]) {
		n++
	}
	if n == 0 {
		return false
	}
	name, s := s[:n], s[n:]
	if quote != 0 {
		if s == "" || s[0] != quote {
			return false
		}
		s = s[1:]
	}
	op := strings.TrimLeft(s, " \t")
	spaced := quote != 0 || len(op) < len(s) || len(op) > 1 && (op[1] == ' ' || op[1] == '\t')
	switch {
	case strings.HasPrefix(op, ":="):
		op = op[2:]
	case spaced && strings.HasPrefix(op, "=") && !strings.HasPrefix(op, "=="):
		op = op[1:]
	default:
		return false
	}
	if !keyNameCarries(name, deliverable) {
		return false
	}
	return keyValueCarries(strings.TrimRight(strings.TrimLeft(op, " \t"), " \t\r,;"), deliverable)
}

// keyValueCarries reports whether v, the value after a JSON, YAML or TOML
// key, with the blanks around it trimmed, carries anything to withhold.
// Nothing, an empty quoted string, null, ~, a lone '{' that opens an object
// on the lines after (its keys are checked on their own), [], and a block
// scalar's indicator ('|' or '>' with its chomping and indent, the text on
// the lines after, which pemScan withholds) carry nothing; a trailing ',' is
// not part of it. When deliverable, a status word (benignValue, or a key's
// keyStatusValues: token: expired) or a home or system path
// (credentials: ~/.aws/credentials, pathValue) carries nothing either.
func keyValueCarries(v string, deliverable bool) bool {
	switch v {
	case "", `""`, "''", "null", "~", "{", "[]":
		return false
	}
	if (v[0] == '|' || v[0] == '>') && strings.Trim(v[1:], "+-0123456789") == "" {
		return false
	}
	if deliverable {
		u := strings.Trim(v, `"'`)
		if benignValue(u) || keyStatusValues[strings.ToLower(u)] || pathValue(u) {
			return false
		}
	}
	return true
}

// keyStatusValues are the words beyond benignValues that Stdout reads as no
// credential when one is a key's whole value (token: expired,
// password: required, #983). Only a key's value takes them: a NAME=value is
// an assignment, and its value may be a word.
var keyStatusValues = map[string]bool{
	"expired": true, "required": true, "missing": true, "invalid": true, "valid": true,
	"revoked": true, "unset": true, "present": true, "absent": true,
}

// pathPrefixes are the starts of a key's value that pathValue reads as a
// file path: a home-relative or relative path, or one under a home or system
// directory.
var pathPrefixes = []string{"~/", "./", "../", "/home/", "/Users/", "/etc/", "/opt/", "/usr/", "/var/", "/tmp/", "/root/"}

// pathValue reports whether v is a file path a key names rather than a
// credential (credentials: ~/.aws/credentials): it starts with "~/", "./",
// "../" or one of pathPrefixes, and holds only name bytes and '/' after it.
func pathValue(v string) bool {
	rest, ok := "", false
	for _, p := range pathPrefixes {
		if r, cut := strings.CutPrefix(v, p); cut {
			rest, ok = r, true
			break
		}
	}
	if !ok || rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if !isNameByte(rest[i]) && rest[i] != '/' {
			return false
		}
	}
	return true
}

// keyNameCarries reports whether a JSON, YAML or TOML key's name reads as a
// credential (credentialName). When deliverable, a name whose last segment
// (after '-', '_' or '.', or a lower-to-upper case change) is a descriptor
// (keyDescriptors: token_type, token_expiry, password_policy,
// credential.helper, #983) names something about a credential, not one, and
// does not; except secret_id (secretId, SECRET_ID), which is Vault AppRole's
// bearer secret, not the id of one.
func keyNameCarries(name string, deliverable bool) bool {
	if !credentialName(name) {
		return false
	}
	if !deliverable {
		return true
	}
	seg := lastNameSegment(name)
	if len(seg) == len(name) || !keyDescriptors[strings.ToLower(seg)] {
		return true
	}
	rest := strings.TrimRight(name[:len(name)-len(seg)], "-_.")
	return strings.EqualFold(seg, "id") && rest != "" && strings.EqualFold(lastNameSegment(rest), "secret")
}

// keyDescriptors are the last name segments that describe a credential
// rather than hold one. Each pairs with a credential word without naming a
// secret (token_id, key_id, secret_name, private_key_file), bar secret_id,
// which keyNameCarries keeps a credential.
var keyDescriptors = map[string]bool{
	"type": true, "expiry": true, "expires": true, "expiration": true, "policy": true, "helper": true,
	"ttl": true, "url": true, "uri": true, "endpoint": true, "file": true, "path": true, "id": true,
	"name": true, "scope": true, "scopes": true, "length": true, "format": true, "prefix": true,
}

// lastNameSegment returns name's last segment: the text after its last '-',
// '_' or '.', or from its last lower-to-upper case change (tokenType: Type).
func lastNameSegment(name string) string {
	seg := name[strings.LastIndexAny(name, "-_.")+1:]
	for k := len(seg) - 1; k > 0; k-- {
		if seg[k] >= 'A' && seg[k] <= 'Z' && seg[k-1] >= 'a' && seg[k-1] <= 'z' {
			return seg[k:]
		}
	}
	return seg
}

// flagValueIn reports whether a whitespace-separated word of line is a flag
// whose name reads as a credential (--password, --token, -api-key) with no
// '=' and another word after it on the line: the value in the space form,
// which Text withholds through argWord. When deliverable, a following word
// that is a common English word (flagProse: use --token to pass it, #983) is
// prose, not a value.
func flagValueIn(line string, deliverable bool) bool {
	for i := 0; i < len(line); {
		for i < len(line) && isSpaceByte(line[i]) {
			i++
		}
		start := i
		for i < len(line) && !isSpaceByte(line[i]) {
			i++
		}
		w := line[start:i]
		if len(w) < 2 || w[0] != '-' || strings.IndexByte(w, '=') >= 0 || !credentialName(w) {
			continue
		}
		for i < len(line) && isSpaceByte(line[i]) {
			i++
		}
		if i == len(line) {
			continue
		}
		if deliverable {
			end := i
			for end < len(line) && !isSpaceByte(line[end]) && end-i <= len("instead") {
				end++
			}
			if flagProse[line[i:end]] {
				continue
			}
		}
		return true
	}
	return false
}

// flagProse are the lowercase words that follow a flag in prose rather than
// its value (use --token to pass it).
var flagProse = map[string]bool{
	"to": true, "for": true, "and": true, "or": true, "the": true, "a": true, "an": true, "with": true,
	"is": true, "if": true, "when": true, "in": true, "of": true, "as": true, "not": true, "instead": true,
}

func isSpaceByte(c byte) bool { return strings.IndexByte(" \t\r\v\f", c) >= 0 }

// benignValues are the status words Stdout reads as no credential when one
// is a credential name's whole value (auth=ok, Cookie: none, #974).
var benignValues = map[string]bool{
	"ok": true, "true": true, "false": true, "yes": true, "no": true, "on": true, "off": true,
	"none": true, "null": true, "nil": true, "enabled": true, "disabled": true,
}

// benignRest reports whether the text of line from start to textEnd (the
// end of line's text) is one benignValue word after blanks. It reads the
// leading blanks, which belong to this value alone, and at most one word no
// longer than benignValue accepts.
func benignRest(line string, start, textEnd int) bool {
	for start < textEnd && isSpaceByte(line[start]) {
		start++
	}
	if textEnd-start > len("disabled") {
		return false
	}
	return benignValue(line[start:textEnd])
}

// benignValue reports whether v is a status word (benignValues, any case).
// A count is not one (password=123): only a pass= count beside a fail= one
// is (benignAssignment, #983).
func benignValue(v string) bool {
	if v == "" || len(v) > len("disabled") {
		return false
	}
	return benignValues[strings.ToLower(v)]
}

// schemeAt reports whether s starts with the Bearer scheme word and a token
// of 8 or more bytes, or the Basic one and 12 or more bytes of base64, any
// case, with whitespace between. When deliverable, a scheme word not spelled
// Bearer, BEARER, Basic or BASIC is prose (basic authentication disabled,
// #974) unless the run after it holds a digit, '+', '/' or '=', or reads as a
// credential anyway (#983): for bearer, a run of 16 or more bytes; for basic,
// one that decodes as base64 to text holding a ':' (user:pass).
func schemeAt(s string, deliverable bool) bool {
	var n int
	var charset func(byte) bool
	exact, bearer := false, false
	switch {
	case len(s) > 6 && strings.EqualFold(s[:6], "bearer"):
		exact, bearer = s[:6] == "Bearer" || s[:6] == "BEARER", true
		s, n, charset = s[6:], 8, isBearerByte
	case len(s) > 5 && strings.EqualFold(s[:5], "basic"):
		exact = s[:5] == "Basic" || s[:5] == "BASIC"
		s, n, charset = s[5:], 12, isBase64Byte
	default:
		return false
	}
	rest := strings.TrimLeft(s, " \t\r\v\f")
	if len(rest) == len(s) || !runAtLeast(rest, n, charset) {
		return false
	}
	if !deliverable || exact {
		return true
	}
	i := 0
	for ; i < len(rest) && (charset(rest[i]) || rest[i] == '='); i++ {
		if c := rest[i]; c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			return true
		}
	}
	if bearer {
		return i >= 16
	}
	return basicCredential(rest[:i])
}

// basicCredential reports whether run, a run of base64 bytes, decodes to
// text holding a ':', as a Basic credential's user:pass does. It decodes run
// once, padded or not.
func basicCredential(run string) bool {
	enc := base64.RawStdEncoding
	if strings.HasSuffix(run, "=") {
		enc = base64.StdEncoding
	}
	b, err := enc.DecodeString(run)
	return err == nil && bytes.IndexByte(b, ':') >= 0
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

// pemScan carries state from one line to the next: whether the line being
// scanned sits inside a PEM private-key block that an earlier line opened,
// or inside the value of a YAML credential key that starts on the next line
// (inValue, #983); and whether it scans for Stdout (deliverable) or for Text.
// Stdout makes the exemptions for deliverable output that Text does not
// (#974); every exemption only keeps a line the other mode withholds, so Text
// withholds a superset.
type pemScan struct {
	inKey, deliverable bool
	// inValue: the lines after a credential key with no value on its own
	// line (password:, password: |) hold its value while they are indented
	// past valueIndent, the key line's indentation, or, under a key with no
	// "- " before it, start a sequence entry ("- ") at valueIndent.
	inValue     bool
	valueIndent int
	valueSeq    bool
}

// stdoutWithheld reports whether Stdout withholds line, and moves the PEM
// and YAML-value state past it. A line inside a block, or one that opens or
// closes one, is withheld, unless it is empty. After the line the scan is
// inside a block when its last armor line is a BEGIN; a line with no armor
// leaves the state as it was. A line inside a YAML credential value is
// withheld unless it is blank; the first line that is not ends the value. It
// reads each line a fixed number of times, so the cost stays linear in the
// text.
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
	if sc.inValue {
		body := strings.TrimLeft(line, " \t")
		if strings.TrimRight(body, " \t\r") == "" {
			return false // a blank line carries nothing and ends nothing
		}
		indent := len(line) - len(body)
		if indent > sc.valueIndent || sc.valueSeq && indent == sc.valueIndent && (body == "-" || strings.HasPrefix(body, "- ")) {
			return true
		}
		sc.inValue = false
	}
	shape := stdoutShape(line, sc.deliverable)
	sc.openValue(line)
	return shape
}

// openValue starts a YAML credential value on the lines after line when line
// is a valueOpener, checked as it is and, when it holds an ESC, with its
// escape sequences stripped.
func (sc *pemScan) openValue(line string) {
	e, ok := valueOpener(line, sc.deliverable)
	if !ok && strings.IndexByte(line, 0x1b) >= 0 {
		e, ok = valueOpener(stripEscapes(line), sc.deliverable)
	}
	if ok {
		sc.inValue, sc.valueIndent, sc.valueSeq = true, e.indent, !e.dashed
	}
}

// credentialAssignment reports whether line holds a NAME=value whose NAME
// reads as a credential (credentialName) and whose value is not empty: NAME
// is the run of name bytes ([A-Za-z0-9._-]) right before the '=', and the
// byte after it is neither whitespace nor the end of the line. Each scan back
// stops at the previous '=', so the cost is linear in line.
//
// When deliverable (#974), a git diffstat row ( src/token=x.go | 1 +) holds
// a file name, not an assignment, and is skipped (diffstatPath); and a value
// that is a status word, or a pass= count beside a fail= one
// (benignAssignment: pass=1 fail=0, auth=ok), is not one.
func credentialAssignment(line string, deliverable bool) bool {
	if deliverable && strings.Contains(line, " | ") && diffstatRow.MatchString(line) && diffstatPath(line) {
		return false
	}
	counts := -1 // whether line holds a fail= count, found once when asked
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
			if deliverable {
				if counts < 0 {
					counts = 0
					if failCountIn(line) {
						counts = 1
					}
				}
				if benignAssignment(line, line[j:i], i+1, counts == 1) {
					continue
				}
			}
			return true
		}
	}
	return false
}

// diffstatRow is one row of git's diffstat: a path with no whitespace, then
// " | " and a change count with its +/- bar, or Bin and the sizes.
var diffstatRow = regexp.MustCompile(`^\s*\S+\s+\|\s+(?:[0-9]+(?: [-+]+)?|Bin(?: [0-9]+ -> [0-9]+ bytes)?)\s*$`)

// diffstatPath reports whether a diffstatRow line's path reads as a file
// path rather than an assignment that only looks like a row
// (PASSWORD=hunter2 | 3, #983): it holds a '/' or a '.', and every
// credential NAME= in it has a value that runs to the end of its path
// segment and ends in a file extension (src/token=x.go, not
// src/TOKEN=abc/x.go). It reads the path once, bar the scan back from each
// '=', which stops at the previous '='.
func diffstatPath(line string) bool {
	path := strings.TrimLeft(line, " \t")
	if end := strings.IndexAny(path, " \t"); end >= 0 {
		path = path[:end]
	}
	if !strings.ContainsAny(path, "/.") {
		return false
	}
	for i := 0; i < len(path); i++ {
		if path[i] != '=' {
			continue
		}
		j := i
		for j > 0 && isNameByte(path[j-1]) {
			j--
		}
		if j == i || !credentialName(path[j:i]) {
			continue
		}
		seg := path[i+1:]
		if end := strings.IndexByte(seg, '/'); end >= 0 {
			seg = seg[:end]
		}
		if !fileNameWithExtension(seg) {
			return false
		}
	}
	return true
}

// fileNameWithExtension reports whether seg is a name, then a '.', then an
// extension of 1 to 10 letters and digits that ends it.
func fileNameWithExtension(seg string) bool {
	dot := strings.LastIndexByte(seg, '.')
	if dot <= 0 || len(seg)-dot-1 < 1 || len(seg)-dot-1 > 10 {
		return false
	}
	for i := dot + 1; i < len(seg); i++ {
		if !isAlnum(seg[i]) {
			return false
		}
	}
	return true
}

// failCountIn reports whether line holds a NAME= whose NAME starts with
// "fail", any case (fail=0, FAILED=2): the context in which pass= is a
// count. Each scan back stops at the previous '='.
func failCountIn(line string) bool {
	for i := 0; i < len(line); i++ {
		if line[i] != '=' {
			continue
		}
		j := i
		for j > 0 && isNameByte(line[j-1]) {
			j--
		}
		if i-j >= 4 && strings.EqualFold(line[j:j+4], "fail") {
			return true
		}
	}
	return false
}

// benignAssignment reports whether the value that starts at line[start],
// assigned to name, is a status word (benignValue), or a count of one to
// three digits assigned to pass beside a fail= count (counts: pass=1 fail=0,
// #983), that ends the line, or is followed by whitespace, ',', ';' or '&'
// and then another NAME= (pass=1 fail=0). A word followed by anything else
// may be the first word of a value that holds a space, and is not benign. It
// reads the value no further than benignValue's longest word, and each run
// after it belongs to this assignment alone, so the cost stays linear in
// line.
func benignAssignment(line, name string, start int, counts bool) bool {
	end := start
	for end < len(line) && end-start <= len("disabled") && !isValueEnd(line[end]) {
		end++
	}
	v := line[start:end]
	passCount := counts && strings.EqualFold(name, "pass") && isCount(v)
	if !benignValue(v) && !passCount {
		return false
	}
	k := end
	for k < len(line) && isValueEnd(line[k]) {
		k++
	}
	if k == len(line) {
		return true
	}
	m := k
	for m < len(line) && isNameByte(line[m]) {
		m++
	}
	return m > k && m < len(line) && line[m] == '='
}

// isCount reports whether v is one to three digits.
func isCount(v string) bool {
	return v != "" && len(v) <= 3 && strings.Trim(v, "0123456789") == ""
}

// isValueEnd is a byte that ends a NAME=value's value in benignAssignment.
func isValueEnd(c byte) bool { return isSpaceByte(c) || c == ',' || c == ';' || c == '&' }
