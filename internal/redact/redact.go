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
package redact

import (
	"regexp"
	"strings"
)

// Marker replaces a withheld word of free text (Text).
const Marker = "[redacted]"

// ArgMarker replaces a withheld argv element or repository argument (Arg).
const ArgMarker = "[redacted-arg]"

// Arg returns one argv element (or a repository argument) as it may be
// written down:
//
//   - verbatim when it is Plain;
//   - as "host/owner/repo" when it is a plain remote repository locator
//     (Repo);
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

// Text returns free text (a child's stderr, an error message) with every
// whitespace-separated word that is not Plain replaced whole by Marker. The
// whitespace itself is kept, so lines and spacing survive.
func Text(s string) string {
	if !strings.Contains(s, "@") && !strings.Contains(s, "://") && !strings.Contains(s, "::") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if isSpace(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && !isSpace(s[j]) {
			j++
		}
		if w := s[i:j]; Plain(w) {
			b.WriteString(w)
		} else {
			b.WriteString(Marker)
		}
		i = j
	}
	return b.String()
}

// tmuxID is a tmux window id such as "@8": digits only, so it cannot hold a
// credential, and tmux argv carries it constantly.
var tmuxID = regexp.MustCompile(`^@[0-9]+$`)

// Plain reports whether s can be written down as is: it holds no "://" and
// no "::" (a URL, or git's transport-helper form http::…), and no '@' other
// than in a shape that cannot carry userinfo: a tmux window id (@8), or git's
// reflog syntax (@{upstream}, main@{u}), where every '@' is followed by '{'
// and there is no ':' for an scp-like host:path.
func Plain(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "::") {
		return false
	}
	if !strings.Contains(s, "@") || tmuxID.MatchString(s) {
		return true
	}
	if strings.Contains(s, ":") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '@' && (i+1 == len(s) || s[i+1] != '{') {
			return false
		}
	}
	return true
}

const (
	hostPattern = `[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?`
	segPattern  = `[A-Za-z0-9._-]{1,100}`
)

// repoLocator is the strict positive parse behind Repo. It admits only a
// scheme from a fixed set, an optional literal "git@" user (the standard ssh
// user, never a credential), a host of letters, digits, '.' and '-', an
// optional numeric port, and exactly two path segments. Any other userinfo,
// a third segment, a query, or a stray byte fails the whole match.
var repoLocator = regexp.MustCompile(
	`^(?:(?:https?|ssh|git)://(?:git@)?(` + hostPattern + `)(?::[0-9]{1,5})?/|git@(` + hostPattern + `):)(` + segPattern + `)/(` + segPattern + `)/?$`)

// Repo renders a remote repository locator as "host/owner/repo" when it is
// exactly one of scheme://[git@]host[:port]/owner/repo[/] (scheme http,
// https, ssh or git) or git@host:owner/repo. The rendering is built only from
// the matched host and segments, so nothing else the string held is shown.
func Repo(s string) (string, bool) {
	m := repoLocator.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	h := m[1]
	if h == "" {
		h = m[2]
	}
	return h + "/" + m[3] + "/" + m[4], true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
