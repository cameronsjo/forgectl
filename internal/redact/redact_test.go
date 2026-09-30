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

// TestText: the unit is the line (#749). A line holding any withheld word is
// replaced whole; the line breaks and every other line survive.
//
// Mutation: in Text, write Marker for the withheld word only (the old
// per-word rule) and the space-split credential keeps "TOKSP".
func TestText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fatal: unable to access 'https://u:SEKRIT@host/o/r/': 403", Marker},
		{"remote https://host rejected me@x.org", Marker},
		{"line one SEKRIT@host:o/r\nline  two\t", Marker + "\nline  two\t"},
		{"a\nfatal: unable to access 'http:///U: TOKSP @h/r/': URL rejected\nb\n", "a\n" + Marker + "\nb\n"},
		{"error: git -c http.extraHeader=Authorization: Bearer SEKRIT fetch\nhint: x", Marker + "\nhint: x"},
		{"curl -H X-Api-Key: SEKRIT", Marker},
		{"usage: --token SEKRIT", Marker},
		{"GET /repos?per_page=1&access_token=SEKRIT", Marker},
		{"Authorization: token SEKRIT", Marker},
		{"no credentials here", "no credentials here"},
		{"try main@{u} instead", "try main@{u} instead"},
		{"error: open token.txt: no such file", "error: open token.txt: no such file"},
		{"HTTP 401: Bad credentials", "HTTP 401: Bad credentials"},
		{"\n\n", "\n\n"},
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

// credentialArgvs are argv credentials with no URL marker, each carrying SEKRIT
// (#749). None may survive Args, in any element.
var credentialArgvs = [][]string{
	{"git", "-c", "http.extraHeader=Authorization: Bearer SEKRIT", "fetch"},
	{"git", "-c", "http.https://host/.extraheader=AUTHORIZATION: basic SEKRIT", "fetch"},
	{"git", "--config=http.extraHeader=Authorization: Bearer SEKRIT", "clone"},
	{"git", "config", "--add", "http.extraHeader", "X-Custom: SEKRIT"},
	{"curl", "--header", "X-Api-Key: SEKRIT"},
	{"curl", "--header=Authorization: token SEKRIT"},
	{"curl", "-H", "Private-Token: SEKRIT"},
	{"curl", "-HPrivate-Token: SEKRIT"},
	{"curl", "--proxy-header", "Proxy-Authorization: SEKRIT"},
	{"tool", "Authorization: Bearer SEKRIT"},
	{"tool", "bearer SEKRIT"},
	{"gh", "auth", "login", "--with-token", "SEKRIT"},
	{"tool", "--password", "SEKRIT"},
	{"tool", "--PASSWORD=SEKRIT"},
	{"tool", "--api-key", "SEKRIT"},
	{"tool", "--api_key=SEKRIT"},
	{"tool", "--client-secret", "SEKRIT"},
	{"tool", "--auth", "SEKRIT"},
	{"tool", "-e", "GITHUB_TOKEN=SEKRIT"},
	{"tool", "access_token=SEKRIT"},
	{"gh", "api", "repos/o/r?per_page=1&access_token=SEKRIT"},
	{"tool", "--data=user=u&password=SEKRIT"},
}

// Mutation: make argWord return Arg(a), false for every element (the
// URL-only rule of #734) and every row renders SEKRIT.
func TestArgs_WithholdsCredentialsOutsideURLs(t *testing.T) {
	for _, argv := range credentialArgvs {
		got := Args(argv)
		if strings.Contains(strings.Join(got, " "), "SEKRIT") {
			t.Errorf("Args(%q) = %q, still carries the credential", argv, got)
		}
		if again := Args(got); strings.Join(again, "\x00") != strings.Join(got, "\x00") {
			t.Errorf("Args is not idempotent on %q: %q then %q", argv, got, again)
		}
		if line := strings.Join(argv, " "); strings.Contains(Text(line), "SEKRIT") {
			t.Errorf("Text(%q) = %q, still carries the credential", line, Text(line))
		}
	}
}

func TestArgs(t *testing.T) {
	cases := []struct{ in, want []string }{
		// What stays shown: the flag or key, never the value.
		{[]string{"-c", "http.extraHeader=Authorization: Bearer X"}, []string{"-c", "http.extraHeader=" + ArgMarker}},
		{[]string{"--header", "X-Api-Key: X", "https://github.com/o/r"}, []string{"--header", ArgMarker, "github.com/o/r"}},
		{[]string{"--token=X", "next"}, []string{"--token=" + ArgMarker, "next"}},
		{[]string{"-H", "X"}, []string{"-H", ArgMarker}},
		{[]string{"--config=http.extraheader=X"}, []string{"--config=http.extraheader=" + ArgMarker}},
		// exec's masked entries keep their marker.
		{[]string{"-e", "GITHUB_TOKEN=" + Marker}, []string{"-e", "GITHUB_TOKEN=" + Marker}},
		{[]string{"--token", Marker}, []string{"--token", Marker}},
		// A URL marker in a withheld flag name or key withholds it whole.
		{[]string{"--header@x=X"}, []string{ArgMarker}},
		{[]string{"url.https://u:X@h/.token=Y"}, []string{ArgMarker}},
		// Controls: nothing credential-shaped, nothing changes.
		{[]string{"rev-list", "--count", "@{upstream}..HEAD", "main@{u}", "@8"}, []string{"rev-list", "--count", "@{upstream}..HEAD", "main@{u}", "@8"}},
		{[]string{"auth", "token", "--hostname", "github.com"}, []string{"auth", "token", "--hostname", "github.com"}},
		{[]string{"get", "pods", "--no-headers", "-o", "wide"}, []string{"get", "pods", "--no-headers", "-o", "wide"}},
		{[]string{"-c", "user.name=Me", "commit", "-m", "fix token refresh"}, []string{"-c", "user.name=Me", "commit", "-m", "fix token refresh"}},
		{[]string{"new-window", "-e", "PATH=/usr/bin"}, []string{"new-window", "-e", "PATH=/usr/bin"}},
		{[]string{"--hostname=github.com", "repos/o/r?per_page=1"}, []string{"--hostname=github.com", "repos/o/r?per_page=1"}},
	}
	for _, c := range cases {
		if got := Args(c.in); strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("Args(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Args copies only when an element changes, and never writes its input.
	in := []string{"--token", "X"}
	_ = Args(in)
	if in[1] != "X" {
		t.Error("Args wrote its input")
	}
	plain := []string{"status", "--short"}
	if got := Args(plain); &got[0] != &plain[0] {
		t.Error("Args copied an argv with nothing to withhold")
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
