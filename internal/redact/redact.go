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
// Arg and Text serve exec's rendering of argv and stderr. LogRepo serves a
// caller logging one repository argument (internal/sandbox), where a local
// path is also shown as is. Both use Repo's shapes, so there is one
// allowlist.
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

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
