// Package redact decides what of a command line or a child's output may be
// written down, in a log line or an error's text, without carrying a
// credential that rides inside a URL (#734). It is a leaf package so that
// exec, sandbox, and anything else that renders a repository argument share
// one rule rather than each growing its own.
//
// The rule is an allowlist, not a cutter. Three attempts to find and cut the
// userinfo out of a URL were each bypassed by a form git 2.43 still sends as
// Basic auth: http:///U:P@host/r (net/url reads no userinfo), four or more
// slashes (curl refuses, and git's error echoes the URL whole), and the
// transport-helper form http::http://U:P@host/r. So nothing is cut. A string
// that could be a credential-bearing locator (it holds an '@', "://" or "::")
// is shown only when it parses positively as a plain host/owner/repo, and
// then as exactly that; otherwise it is replaced whole.
//
// A credential with no URL marker (a header after git's -c
// http.extraHeader, curl's -H, a --token flag, a query-string token) is found
// by the flag or key it follows instead (Args, #749), and withheld whole.
//
// Args and Text serve exec's rendering of argv and stderr; Arg renders one
// element. LogRepo serves a caller logging one repository argument
// (internal/sandbox), where a local path is also shown as is. All use Repo's
// shapes, so there is one allowlist.
package redact

import (
	"regexp"
	"strings"
)

// Marker replaces a withheld word of free text (Text).
const Marker = "[redacted]"

// ArgMarker replaces a withheld argv element or repository argument (Arg).
const ArgMarker = "[redacted-arg]"

// UserArgMarker replaces a user-written argv value or positional (UserArgs).
const UserArgMarker = "[user-arg]"

// UserArgs renders argv that a user wrote and forgectl passes through (a
// workflow run step, docker's pass-through arguments) as flag names only
// (#749). Guessing which of a user's arguments carries a credential is a
// denylist over every tool's flag grammar, and it kept leaking (docker login
// -p X, mysql -pX, curl -u u:X, gh secret set --body X). So no user value is
// shown:
//
//   - "--" and "-" as they are;
//   - --name as it is, and --name=VALUE as --name=[user-arg], when the name
//     is ASCII letters, digits, '-', '_' and '.';
//   - -x as it is, and a glued -xVALUE as -x[user-arg], when x is an ASCII
//     letter or digit;
//   - everything else, every value and positional, as UserArgMarker.
func UserArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = userArg(a)
	}
	return out
}

func userArg(a string) string {
	switch {
	case a == "--" || a == "-":
		return a
	case strings.HasPrefix(a, "--"):
		name, _, joined := strings.Cut(a, "=")
		if len(name) == 2 || !isFlagName(name[2:]) {
			return UserArgMarker
		}
		if joined {
			return name + "=" + UserArgMarker
		}
		return name
	case strings.HasPrefix(a, "-") && isFlagName(a[1:2]) && a[1] != '-' && a[1] != '.' && a[1] != '_':
		if len(a) == 2 {
			return a
		}
		return a[:2] + UserArgMarker
	}
	return UserArgMarker
}

// isFlagName reports whether s is a non-empty run of ASCII letters, digits,
// '-', '_' and '.'.
func isFlagName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '-' && c != '_' && c != '.' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// Arg returns one argv element (or a repository argument) as it may be
// written down:
//
//   - verbatim when it is Plain;
//   - as "host/owner/repo" when it is exactly one of Repo's shapes;
//   - otherwise ArgMarker, whole.
func Arg(s string) string {
	if Plain(s) {
		return s
	}
	if r, ok := Repo(s); ok {
		return r
	}
	return ArgMarker
}

// Args returns argv as it may be written down: each element through Arg, and
// on top of that the credentials Arg cannot see because they carry no URL
// marker (#749). A value that follows a credential-bearing flag or config
// key is withheld whole, as ArgMarker, while the flag or key itself is shown:
//
//   - a flag whose name reads as a credential (--header, --token,
//     --password, --api-key, …, see credentialName), or -H: the next element,
//     or the part after '=' in the joined form (--header=…);
//   - a KEY=VALUE element whose KEY reads as one (git's -c
//     http.extraHeader=…, an env assignment GITHUB_TOKEN=…): the VALUE; one
//     where a later NAME= reads as one (a query string such as
//     repos?per_page=1&access_token=…): the element, whole;
//   - a dotted config key on its own whose last segment reads as one (git
//     config http.extraHeader VALUE), or the word Bearer: the next element;
//   - an element that carries an HTTP credential header or scheme
//     (Authorization:, Private-Token:, Bearer …): the element, whole.
//
// It is a withhold list, not a parser of any one tool's flags, so it errs
// toward withholding: a boolean flag with a credential-looking name costs the
// element after it. A withheld element that would itself withhold the next
// one, "--", or a marker keeps withholding armed. A value already rendered as
// a marker (Marker, ArgMarker, UserArgMarker) is left as it is. Args copies
// only when an element changes.
func Args(args []string) []string {
	var out []string
	withhold := false
	for i, a := range args {
		r := a
		if withhold {
			// The withheld element is the value, whatever it looks like. If
			// it would itself withhold the next one (-H -H X, --token
			// --password X), or is "--" or an already-rendered marker, where
			// the value may be the element after it (--token -- X), stay
			// armed.
			if !isMarker(a) {
				r = ArgMarker
			}
			_, next := argWord(a)
			withhold = next || a == "--" || isMarker(a)
		} else {
			r, withhold = argWord(a)
		}
		if r == a && out == nil {
			continue
		}
		if out == nil {
			out = append(make([]string, 0, len(args)), args[:i]...)
		}
		out = append(out, r)
	}
	if out == nil {
		return args
	}
	return out
}

// argWord renders one element for Args, and reports whether the element
// after it is a credential value to withhold.
func argWord(a string) (string, bool) {
	if strings.HasPrefix(a, "-") {
		name, value, joined := strings.Cut(a, "=")
		if !Plain(name) {
			return ArgMarker, false
		}
		if !joined {
			// -H is curl's and gh's header flag; glued (-HAuthorization:…)
			// the value is the rest of the element.
			if a == "-H" {
				return a, true
			}
			if strings.HasPrefix(a, "-H") {
				return ArgMarker, false
			}
			return a, credentialName(name)
		}
		if credentialName(name) {
			return withheldValue(name, value), false
		}
		// --config=http.extraHeader=… carries a KEY=VALUE of its own.
		if v, _ := argWord(value); v != value {
			if v == ArgMarker {
				return ArgMarker, false
			}
			return name + "=" + v, false
		}
		return a, false
	}
	if key, value, ok := strings.Cut(a, "="); ok && credentialName(key) {
		if !Plain(key) {
			return ArgMarker, false
		}
		return withheldValue(key, value), false
	}
	// An HTTP credential in the element itself, or a later assignment, as in
	// a query string passed as a plain argument
	// (repos?per_page=1&access_token=…): withhold it all.
	if credentialValue(a) || credentialKeyIn(a) {
		return ArgMarker, false
	}
	// The scheme word of a header split across elements ("Bearer", TOKEN).
	if strings.EqualFold(a, "bearer") {
		return a, true
	}
	// A config key on its own (git config http.extraHeader VALUE): its last
	// dotted segment must read as a credential, so a file name such as
	// token.txt does not swallow the next element.
	if i := strings.LastIndexByte(a, '.'); i >= 0 && !strings.Contains(a, "=") && credentialName(a[i+1:]) {
		return Arg(a), true
	}
	return Arg(a), false
}

// withheldValue shows key=ArgMarker, leaving an already-rendered value as it
// is so Args is idempotent over exec's masked KEY=[redacted] entries.
func withheldValue(key, value string) string {
	if isMarker(value) {
		return key + "=" + value
	}
	return key + "=" + ArgMarker
}

// credentialFragments are the substrings that make a flag or config key name
// credential-bearing, matched against the name lowercased with '-' and '_'
// removed. "header" matches only as the name's end (--header,
// http.extraHeader, --proxy-header), so kubectl's boolean --no-headers does
// not swallow the next element.
var credentialFragments = []string{
	"password", "passwd", "passphrase", "token", "secret", "apikey", "accesskey",
	"privatekey", "credential", "authorization", "bearer", "cookie", "signature",
}

// credentialSegments are the short words that make a name credential-bearing
// only as a whole segment between '-', '_' or '.': --auth, NPM_AUTH, GH_PAT,
// SSH_KEY, but not --author or --keymap.
var credentialSegments = map[string]bool{
	"auth": true, "pass": true, "pw": true, "pwd": true, "pat": true, "key": true, "sig": true,
}

var nameSeparators = strings.NewReplacer("-", "", "_", "")

// credentialName reports whether a flag or config key name reads as one that
// carries a credential value.
func credentialName(name string) bool {
	l := strings.ToLower(strings.TrimLeft(name, "-"))
	for _, seg := range strings.FieldsFunc(l, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if credentialSegments[seg] {
			return true
		}
	}
	n := nameSeparators.Replace(l)
	if strings.HasSuffix(n, "header") {
		return true
	}
	for _, f := range credentialFragments {
		if strings.Contains(n, f) {
			return true
		}
	}
	return false
}

// credentialKeyIn reports whether any NAME=… in s has a credential name,
// NAME being the run of name bytes ([A-Za-z0-9._-]) right before an '='. Each
// scan back stops at the previous '=', so the cost is linear in s.
func credentialKeyIn(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '=' {
			continue
		}
		j := i
		for j > 0 && isNameByte(s[j-1]) {
			j--
		}
		if j < i && credentialName(s[j:i]) {
			return true
		}
	}
	return false
}

func isNameByte(c byte) bool {
	return c == '.' || c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// credentialValue reports whether s itself is an HTTP credential: it carries
// an Authorization (or Proxy-Authorization) header, starts with the Bearer or
// Basic scheme, or is a header ("Name: value", or the bare "Name:" of a
// split one) whose hyphenated name reads as a credential (Private-Token:,
// X-Api-Key:), or Cookie.
func credentialValue(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(l, "authorization:") || strings.HasPrefix(l, "bearer ") || strings.HasPrefix(l, "basic ") {
		return true
	}
	// A header name is letters, digits and '-'. It must hold a '-' or be
	// Cookie, so that prose such as "invalid token: expired" in a child's
	// stderr is not read as a header.
	name, _, ok := strings.Cut(l, ":")
	if !ok || name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return false
	}
	return (strings.Contains(name, "-") || name == "cookie") && credentialName(name)
}

// Text returns free text (a child's stderr, an error message) with every
// line that holds a withheld word replaced whole by Marker; the line breaks
// survive, and a line with nothing withheld is kept as is. A word is
// withheld when it is not Plain, when Args would withhold it
// (Authorization:, http.extraHeader=…), or when Args would withhold the word
// after it (--token, -H, Bearer, http.extraHeader).
//
// The unit is the line, not the word (#749). Words are split on whitespace,
// and a credential can hold a space: git sends nothing for
// "http:///U: TOK @h" (curl rejects it), but its error echoes the URL, and a
// per-word rule withheld "http:///U:" and "@h" while "TOK" stayed. Widening
// to the neighbouring words only moves the problem to a credential with two
// spaces. The line is the boundary the credential cannot cross: git refuses
// a URL holding a newline before it sends anything, and a header value
// cannot hold one either. The cost is the diagnostic text sharing a line with
// a withheld word.
func Text(s string) string {
	var b strings.Builder
	changed := false
	for rest := s; ; {
		line, next, more := strings.Cut(rest, "\n")
		if lineWithheld(line) {
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

// lineWithheld reports whether any whitespace-separated word of line is one
// Text withholds.
func lineWithheld(line string) bool {
	for _, w := range strings.FieldsFunc(line, isSpace) {
		if repoWord(w) {
			continue
		}
		if !Plain(w) {
			return true
		}
		if r, next := argWord(w); r != w || next {
			return true
		}
	}
	return false
}

// repoWord reports whether a word of free text is exactly one of Repo's
// shapes once the quotes and punctuation git's messages put around a URL are
// trimmed: fatal: repository 'https://github.com/o/r/' not found. Repo has no
// userinfo, query or other slack, and the trim removes only quote and
// punctuation bytes from the ends plus one trailing '/', so nothing else can
// ride along in the word.
func repoWord(w string) bool {
	w = strings.TrimLeft(w, "'\"`(<[")
	w = strings.TrimRight(w, "'\"`)>],.:;")
	w = strings.TrimSuffix(w, "/")
	_, ok := Repo(w)
	return ok
}

// Plain reports whether s can be written down as is: it holds no "://" and
// no "::" (a URL, or git's transport-helper form http::…), and no '@' other
// than in a shape that cannot carry userinfo. With no ':' in s (so no
// scp-like host:path), an '@' that starts s has no userinfo before it (a tmux
// window id @8, gh's @me), and one followed by '{' is git's reflog syntax
// (@{upstream}, main@{u}), whose "{…}" is no host.
func Plain(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "::") {
		return false
	}
	if !strings.Contains(s, "@") {
		return true
	}
	if strings.Contains(s, ":") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '@' && i > 0 && (i+1 == len(s) || s[i+1] != '{') {
			return false
		}
	}
	return true
}

// RemoteRepoPlaceholder is what LogRepo shows for a non-local repository
// that is not exactly one of the accepted shapes.
const RemoteRepoPlaceholder = "[remote repo]"

// Positive parses for Repo. repoHostPattern is a DNS-style hostname with no
// '@', ':', '[' or '%'. repoPartPattern is one owner or repo path segment.
const (
	repoHostPattern = `([A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?)`
	repoPartPattern = `([A-Za-z0-9._-]{1,100})`
)

// repoShapes are the only forms Repo renders, each anchored at both ends:
// https with no userinfo, ssh as the git user with an optional port, and
// scp-like as the git user.
var repoShapes = []*regexp.Regexp{
	regexp.MustCompile(`^https://` + repoHostPattern + `/` + repoPartPattern + `/` + repoPartPattern + `$`),
	regexp.MustCompile(`^ssh://git@` + repoHostPattern + `(?::[0-9]{1,5})?/` + repoPartPattern + `/` + repoPartPattern + `$`),
	regexp.MustCompile(`^git@` + repoHostPattern + `:` + repoPartPattern + `/` + repoPartPattern + `$`),
}

// Repo renders a remote repository locator as "host/owner/repo" when it is
// exactly one of https://host/owner/repo, ssh://git@host[:port]/owner/repo or
// git@host:owner/repo, rebuilt from the captured fields only, with a
// trailing ".git" dropped. Anything else (userinfo, a query, a fragment,
// "::", percent-escapes, backslashes, whitespace, bracketed IPv6, odd slash
// counts, a third segment, every other scheme) is not a match.
func Repo(s string) (string, bool) {
	for _, shape := range repoShapes {
		if m := shape.FindStringSubmatch(s); m != nil {
			return m[1] + "/" + m[2] + "/" + strings.TrimSuffix(m[3], ".git"), true
		}
	}
	return "", false
}

// LogRepo renders a repository argument (a clone URL or a local path) for a
// log line without any credential it carries (#711, #734). A local path
// (HasLocalPathPrefix) is shown as is; a Repo shape as host/owner/repo;
// anything else as RemoteRepoPlaceholder.
func LogRepo(repo string) string {
	if HasLocalPathPrefix(repo) {
		return repo
	}
	if r, ok := Repo(repo); ok {
		return r
	}
	return RemoteRepoPlaceholder
}

// HasLocalPathPrefix reports whether repo is spelled as a filesystem path:
// absolute, ./ or ../ relative, or ".".
func HasLocalPathPrefix(repo string) bool {
	return strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "./") || strings.HasPrefix(repo, "../") || repo == "."
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// isMarker reports whether s is one of the placeholders this package or exec
// renders in place of a withheld value.
func isMarker(s string) bool {
	return s == Marker || s == ArgMarker || s == UserArgMarker
}
