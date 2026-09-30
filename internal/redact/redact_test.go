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
		{"", ""},
		// A plain repository locator: host/owner/repo, nothing else.
		{"https://github.com/o/r", "github.com/o/r"},
		{"https://github.com/o/r.git/", "github.com/o/r.git"},
		{"ssh://git@github.com:22/o/r.git", "github.com/o/r.git"},
		{"git@github.com:o/r.git", "github.com/o/r.git"},
		// Anything else that holds '@', "://" or "::": withheld whole.
		{"https://SECRET@github.com/o/r", ArgMarker},
		{"https://github.com/group/sub/r", ArgMarker},
		{"https://github.com/o/r?x=1", ArgMarker},
		{"HTTPS_PROXY=http://proxy:3128", ArgMarker},
		{"a@b.org", ArgMarker},
		{"@", ArgMarker},
		{"x@{u}:y", ArgMarker},
		{"@8x", ArgMarker},
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
