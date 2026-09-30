package redact

import (
	"strings"
	"testing"
)

// secretForms are credential-bearing locators, each of which git 2.43 either
// sends as Basic auth or echoes whole in an error. None may survive Arg or
// Text, in any part.
var secretForms = []string{
	"https://x-access-token:SECRET@github.com/o/r",
	"https://SECRET@github.com/o/r",
	"https://SECRET@github.com",
	"http:/U:SECRET@host/r",
	"http:///U:SECRET@host/r",
	"https:////SECRET@host/r",
	"https://///SECRET@host/r",
	"http::http://U:SECRET@host/r",
	"http:U:SECRET@host/r",
	"https://SECRET@host?x=1",
	"https://SECRET@host#frag",
	"https://host?a@SECRET/x",
	"https://u:SECRET@host/r?q=a@b#f@g",
	"SECRET@github.com:o/r",
	"user:SECRET@host:/p://x",
	"--config=http.proxy=http://u:SECRET@proxy:3128",
	"url.https://SECRET@host/.insteadOf=https://github.com/",
	"ssh://SECRET@host/o/r",
	"git@host:o/r@SECRET",
	"SECRET@{u}:x",
}

func TestArg_WithholdsEveryCredentialForm(t *testing.T) {
	for _, in := range secretForms {
		if got := Arg(in); strings.Contains(got, "SECRET") {
			t.Errorf("Arg(%q) = %q, still carries the secret", in, got)
		}
	}
}

func TestText_WithholdsEveryCredentialForm(t *testing.T) {
	for _, in := range secretForms {
		line := "fatal: unable to access '" + in + "/': URL rejected"
		if got := Text(line); strings.Contains(got, "SECRET") {
			t.Errorf("Text(%q) = %q, still carries the secret", line, got)
		}
	}
}

func TestArg(t *testing.T) {
	cases := []struct{ in, want string }{
		// Plain: verbatim.
		{"clone", "clone"},
		{"--", "--"},
		{"/abs/path", "/abs/path"},
		{"K=" + Marker, "K=" + Marker},
		{"@{upstream}..HEAD", "@{upstream}..HEAD"},
		{"main@{u}", "main@{u}"},
		{"@8", "@8"},
		{"@me", "@me"},
		{"@", "@"},
		{"", ""},
		// A plain repository locator: host/owner/repo, nothing else.
		{"https://github.com/o/r", "github.com/o/r"},
		{"ssh://git@github.com:22/o/r.git", "github.com/o/r"},
		{"git@github.com:o/r.git", "github.com/o/r"},
		{"https://github.com/o/r.git/", ArgMarker},
		{"http://github.com/o/r", ArgMarker},
		// Anything else that holds '@', "://" or "::": withheld whole.
		{"https://SECRET@github.com/o/r", ArgMarker},
		{"https://github.com/group/sub/r", ArgMarker},
		{"https://github.com/o/r?x=1", ArgMarker},
		{"HTTPS_PROXY=http://proxy:3128", ArgMarker},
		{"a@b.org", ArgMarker},
		{"x@{u}:y", ArgMarker},
		{"@a@b", ArgMarker},
		{"@8:x", ArgMarker},
		{"ext::ssh -i key host", ArgMarker},
	}
	for _, c := range cases {
		if got := Arg(c.in); got != c.want {
			t.Errorf("Arg(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := Arg(Arg(c.in)); got != c.want {
			t.Errorf("Arg is not idempotent on %q: %q", c.in, got)
		}
	}
}

func TestText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fatal: unable to access 'https://u:TOK@host/o/r/': 403", "fatal: unable to access " + Marker + " 403"},
		{"remote https://host rejected me@x.org", "remote " + Marker + " rejected " + Marker},
		{"line one TOK@host:o/r\nline  two\t", "line one " + Marker + "\nline  two\t"},
		{"no credentials here", "no credentials here"},
		{"try main@{u} instead", "try main@{u} instead"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := Text(Text(c.in)); got != c.want {
			t.Errorf("Text is not idempotent on %q: %q", c.in, got)
		}
	}
}

// TestLogRepo is #711's allowlist, lifted from internal/sandbox (#734). A non-local repo reaches the log only as
// host/owner/repo, rebuilt from a strict positive parse, or as the
// placeholder. Every row below is a form one of the three reviews found, or
// a control around one.
func TestLogRepo(t *testing.T) {
	const ph = RemoteRepoPlaceholder
	for _, tc := range []struct{ in, want string }{
		// Accepted shapes.
		{"https://github.com/o/r.git", "github.com/o/r"},
		{"https://ghe.example.test/Org-1/re.po_x", "ghe.example.test/Org-1/re.po_x"},
		{"ssh://git@ghe.example.test/o/r.git", "ghe.example.test/o/r"},
		{"ssh://git@ghe.example.test:2222/o/r.git", "ghe.example.test/o/r"},
		{"git@github.com:o/r.git", "github.com/o/r"},
		// Local paths.
		{"/home/u/src/r@2", "/home/u/src/r@2"},
		{"./r", "./r"},
		{"../r", "../r"},
		{".", "."},
		// Userinfo in any position or spelling.
		{"https://x-access-token:SECRETTOK@github.com/o/r.git", ph},
		{"https://SECRETTOK@github.com/o/r.git", ph},
		{"ssh://SECRETTOK@ghe.example.test:2222/o/r.git", ph},
		{"git+ssh://SECRETTOK@h/o/r", ph},
		{"git+ssh://git@h/o/r", ph},
		{"SECRETTOK@github.com:o/r.git", ph},
		{"SECRETTOK:pw@github.com:o/r.git", ph},
		{"a@SECRETTOK@github.com:o/r@x.git", ph},
		{"user:SECRETTOK@github.com:/o/r://x", ph},
		{"https://SECRETTOK%40x@github.com/o/r", ph},
		{"https://github.com%40SECRETTOK/o/r", ph},
		{"https://x:SECRETTOK@host:notaport/o/r", ph},
		{"https://x:SECRET/TOK@host/o/r", ph},
		// Odd slash counts, 1 through 6.
		{"https:/SECRETTOK@host/o/r", ph},
		{"https:///SECRETTOK@host/o/r", ph},
		{"https:////SECRETTOK@host/o/r", ph},
		{"https://///SECRETTOK@host/o/r", ph},
		{"https://////SECRETTOK@host/o/r", ph},
		{"https:///////SECRETTOK@host/o/r", ph},
		{"http:///USER:SECRETTOK@host:8080/r", ph},
		{"https://github.com//o/r", ph},
		{"https://github.com/o/r/", ph},
		{"https://github.com/o/r/x", ph},
		// Transport-helper forms (git sends U:TOK as Basic auth).
		{"http::http://U:SECRETTOK@host/r", ph},
		{"https::https://U:SECRETTOK@host/o/r", ph},
		{"persistent-https::https://SECRETTOK@host/o/r", ph},
		{"x+y::https://SECRETTOK@host/o/r", ph},
		{"HTTP::http://U:SECRETTOK@host/r", ph},
		{"http::http:///SECRETTOK@h/r", ph},
		{"https::https://github.com/o/r", ph},
		// Query, fragment, other schemes, IPv6, backslashes, whitespace.
		{"https://github.com/o/r?access_token=SECRETTOK", ph},
		{"https://github.com/o/r#SECRETTOK", ph},
		{"http://github.com/o/r", ph},
		{"git://github.com/o/r", ph},
		{"HTTPS://github.com/o/r", ph},
		{"https://[::1]/o/r", ph},
		{"ssh://git@[::1]:22/o/r", ph},
		{"git@[::1]:o/r", ph},
		{"https:\\\\SECRETTOK@host\\o\\r", ph},
		{"https://github.com\\o/r", ph},
		{"https://github.com/o/r ", ph},
		{" https://github.com/o/r", ph},
		{"https://github.com/o/r\nSECRETTOK", ph},
		{"https://git hub.com/o/r", ph},
		// Part charset: a byte outside [A-Za-z0-9._-] in owner or repo
		// fails the whole parse.
		{"https://host/o/r%0A", ph},
		{"https://host/o/r@SECRETTOK", ph},
		{"https://host/o:x/r", ph},
		// A leading '.' is not a local-path prefix unless it is "./", "../"
		// or exactly ".".
		{".x:SECRETTOK@h:o/r", ph},
		// Other non-local forms.
		{"owner/repo", ph},
		{"", ph},
	} {
		if got := LogRepo(tc.in); got != tc.want {
			t.Errorf("LogRepo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
