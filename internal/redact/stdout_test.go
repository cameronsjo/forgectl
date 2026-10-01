package redact

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/perftest"
)

// stdoutKept are lines Stdout must show as they are: real output of the
// commands whose stdout forgectl shows (npm outdated -g, brew outdated, brew
// update and upgrade, git pull, gh auth status, go), and near misses of each
// withheld shape. Text withholds several of them (the '@' rows), which is
// why Stdout exists (#952).
var stdoutKept = []string{
	// npm outdated -g.
	"Package                    Current   Wanted   Latest  Location                                Depended by",
	"@anthropic-ai/claude-code   2.1.42  2.1.286  2.1.286  node_modules/@anthropic-ai/claude-code  global",
	"@openai/codex                0.1.0    0.2.0    0.2.0  node_modules/@openai/codex              global",
	"npm                         10.8.2   10.9.0   11.0.0  node_modules/npm                        global",
	"npm warn deprecated inflight@1.0.6: This module is not supported, and leaks memory.",
	"npm_config_prefix=/usr/local",
	// brew outdated, update, upgrade, cleanup.
	"python@3.12 (3.12.1) < 3.12.2",
	"openssl@3 (3.3.1) < 3.3.2",
	"==> Updating Homebrew...",
	"Updated 2 taps (homebrew/core and homebrew/cask).",
	"Updated https://github.com/Homebrew/brew from 1a2b3c4 to 5d6e7f8.",
	"==> Upgrading python@3.12 3.12.1 -> 3.12.2",
	"==> Pouring python@3.12--3.12.2.arm64_sonoma.bottle.tar.gz",
	"\U0001F37A  /opt/homebrew/Cellar/python@3.12/3.12.2: 3,300 files, 65MB",
	"==> Downloading https://ghcr.io/v2/homebrew/core/python/3.12/blobs/sha256:4f2a9c0e",
	"Removing: /Users/me/Library/Caches/Homebrew/node@20--20.1.0.tar.gz... (12MB)",
	"forgectl 0.22.0 -> 0.23.0",
	"Already up-to-date.",
	// git pull.
	"From https://github.com/o/r",
	" * branch            main       -> FETCH_HEAD",
	"Updating 1a2b3c4..5d6e7f8",
	"Fast-forward",
	" README.md | 2 +-",
	"Cloning into 'r'... from git@github.com:o/r.git.",
	"remote: ssh://git@github.com/o/r.git",
	"try main@{u} instead",
	// gh auth status: gh masks the token itself.
	"  \u2713 Logged in to github.com account octocat (keyring)",
	"  - Token: gho_************************************",
	"  - Token scopes: 'gist', 'read:org', 'repo'",
	// go.
	"go version go1.26.0 darwin/arm64",
	"go: downloading golang.org/x/tools v0.30.0",
	// Near misses: below a token's issued length, no left boundary, an
	// empty value, a header word in prose, a mail address.
	"ghp_short",
	"sk-12345",
	"task-1234567890abcdefghijklmnop",
	"AKIAshort",
	"Bearer token",
	"GITHUB_TOKEN=",
	"error validating token: expired",
	"Contact support@example.com for help",
	"-----BEGIN CERTIFICATE-----",
	Marker,
	"",
}

// stdoutWithheld are lines Stdout must withhold, one per shape, each carrying
// SEKRIT or a token of its issued shape.
var stdoutWithheld = []string{
	// URL userinfo, every slash count, the transport helper, no scheme.
	"remote: https://x-access-token:SEKRIT@github.com/o/r",
	"https://SEKRIT@github.com/o/r",
	"fatal: unable to access 'http:///U:SEKRIT@host/r/': URL rejected",
	"http:/U:SEKRIT@host/r",
	"https:////SEKRIT@host/r",
	"http::http://U:SEKRIT@host/r",
	"user:SEKRIT@host",
	"ssh://gitx:SEKRIT@host/o/r",
	"ssh://SEKRIT@host/o/r",
	"proxy=http://u:SEKRIT@proxy:3128",
	// Headers and schemes.
	"Authorization: token SEKRIT",
	"> proxy-authorization: Basic SEKRIT",
	"remote: Private-Token: SEKRIT",
	"X-Api-Key: SEKRIT",
	"Cookie: session=SEKRIT",
	"< Set-Cookie: sid=SEKRIT; Path=/",
	"curl -H 'Bearer SEKRIT12'",
	"Basic dXNlcjpTRUtSSVQ=",
	// Token prefixes at their issued length.
	"token ghp_" + strings.Repeat("A1", 18),
	"gho_" + strings.Repeat("b", 36),
	"(ghu_" + strings.Repeat("C", 36) + ")",
	"ghs_" + strings.Repeat("d", 36),
	"ghr_" + strings.Repeat("e", 36),
	"github_pat_11ABCDEFG0123456789_abcdefghij",
	"glpat-" + strings.Repeat("x", 20),
	"glptt-" + strings.Repeat("0", 40),
	"key: sk-" + strings.Repeat("Z", 48),
	"sk-ant-api03-" + strings.Repeat("q", 40),
	"sk-proj-" + strings.Repeat("r", 40),
	"sk_live_" + strings.Repeat("s", 24),
	"xoxb-123456789012-abcdef",
	"AKIAIOSFODNN7EXAMPLE",
	"id=ASIAIOSFODNN7EXAMPLE",
	"npm_" + strings.Repeat("t", 36),
	"AIza" + strings.Repeat("u", 35),
	"hf_" + strings.Repeat("v", 34),
	"pypi-" + strings.Repeat("w", 60),
	"shpat_" + strings.Repeat("a", 32),
	// Credential assignments.
	"GITHUB_TOKEN=SEKRIT",
	"export NPM_AUTH=SEKRIT",
	"--password=SEKRIT",
	"HOMEBREW_GITHUB_API_TOKEN=SEKRIT",
}

// TestStdout: every stdoutKept line survives, every stdoutWithheld line reads
// Marker whole, on its own and between two kept lines, and Stdout is
// idempotent.
//
// Mutations that turn it red, one per shape: drop the stdoutUserinfo match
// (the URL rows show); replace its "not git" alternation with a plain
// [^\s/?#@]+ (ssh://git@github.com/o/r.git is withheld); drop headerIn (the
// header rows show) or its word-start check (none today; see
// TestStdout_HeaderStartsAWord); drop the word-start skip in stdoutShape
// (task-1234… is withheld); lower a token's minimum to 1 (sk-12345 or
// ghp_short is withheld); drop any one stdoutTokens entry (its row shows);
// drop schemeAt (the Bearer and Basic rows show); drop credentialAssignment
// (the NAME=value rows show), or its empty-value skip (GITHUB_TOKEN= is
// withheld).
func TestStdout(t *testing.T) {
	for _, line := range stdoutKept {
		if got := Stdout(line); got != line {
			t.Errorf("Stdout(%q) = %q, want it kept", line, got)
		}
	}
	for _, line := range stdoutWithheld {
		if got := Stdout(line); got != Marker {
			t.Errorf("Stdout(%q) = %q, want %q", line, got, Marker)
		}
		in := "before\n" + line + "\nafter\n"
		want := "before\n" + Marker + "\nafter\n"
		if got := Stdout(in); got != want {
			t.Errorf("Stdout(%q) = %q, want %q", in, got, want)
		}
		if got := Stdout(Stdout(in)); got != want {
			t.Errorf("Stdout is not idempotent on %q: %q", in, got)
		}
	}
	if got := Stdout(Marker); got != Marker {
		t.Errorf("Stdout(Marker) = %q", got)
	}
}

// TestStdout_PEMBlock: a private key is withheld from its BEGIN line through
// its END line, the lines around it kept; a block with no END is withheld to
// the end; an empty line, which carries nothing, stays empty; a certificate
// is kept.
//
// Mutations that turn it red: keep inKey false after a BEGIN (the body
// lines show); clear inKey on any armor line rather than on the last END
// (the reopened block's body shows); withhold nothing after the END line
// (keep inKey after an END; the trailing kept line reads Marker).
func TestStdout_PEMBlock(t *testing.T) {
	body := "MIIEowIBAAKCAQEA" + strings.Repeat("b", 48)
	for _, c := range []struct{ in, want string }{
		{
			"key:\n-----BEGIN PRIVATE KEY-----\n" + body + "\n" + body + "\n-----END PRIVATE KEY-----\nafter",
			"key:\n" + Marker + "\n" + Marker + "\n" + Marker + "\n" + Marker + "\nafter",
		},
		{
			"-----BEGIN EC PRIVATE KEY-----\n" + body + "\nstill key\n",
			Marker + "\n" + Marker + "\n" + Marker + "\n",
		},
		{
			"-----BEGIN OPENSSH PRIVATE KEY-----\n" + body + "\n\n-----END OPENSSH PRIVATE KEY-----\n",
			Marker + "\n" + Marker + "\n\n" + Marker + "\n",
		},
		{
			"-----BEGIN PGP PRIVATE KEY BLOCK-----\n" + body + "\n-----END PGP PRIVATE KEY BLOCK-----",
			Marker + "\n" + Marker + "\n" + Marker,
		},
		{
			"x -----BEGIN RSA PRIVATE KEY----- " + body + " -----END RSA PRIVATE KEY----- y\nafter",
			Marker + "\nafter",
		},
		{
			"-----END RSA PRIVATE KEY----- -----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\nafter",
			Marker + "\n" + Marker + "\n" + Marker + "\nafter",
		},
		{
			"-----BEGIN CERTIFICATE-----\n" + body + "\n-----END CERTIFICATE-----",
			"-----BEGIN CERTIFICATE-----\n" + body + "\n-----END CERTIFICATE-----",
		},
	} {
		if got := Stdout(c.in); got != c.want {
			t.Errorf("Stdout(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStdout_TextWithholdsEverythingStdoutDoes is the property the design
// rests on (#952): Text withholds a superset of Stdout's lines, so a caller
// deduplicating a Text copy against a Stdout copy (failedCommandOutput) never
// writes a line Stdout withheld. It runs every pair of the kept and withheld
// rows joined by a space and by a newline, and FuzzStdoutWithinText runs the
// same check over arbitrary text.
//
// Mutation that turns it red: make Text lineWithheld alone (drop the
// stdoutWithheld call) and the token, PEM-body and header rows Text's word
// rule does not see come through Text.
func TestStdout_TextWithholdsEverythingStdoutDoes(t *testing.T) {
	armor := []string{"MIIEowIBAAKCAQEA", "-----END PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----", "-----BEGIN OPENSSH PRIVATE KEY-----"}
	rows := append(append(armor, stdoutKept...), stdoutWithheld...)
	for _, a := range rows {
		for _, b := range rows {
			for _, sep := range []string{" ", "\n"} {
				checkStdoutWithinText(t, a+sep+b)
			}
		}
	}
}

func FuzzStdoutWithinText(f *testing.F) {
	for _, s := range stdoutWithheld {
		f.Add(s)
	}
	for _, s := range stdoutKept {
		f.Add(s)
	}
	f.Add("-----BEGIN PRIVATE KEY-----\nMII\n-----END PRIVATE KEY-----\nx")
	f.Fuzz(func(t *testing.T, s string) {
		checkStdoutWithinText(t, s)
		if got := Stdout(Stdout(s)); got != Stdout(s) {
			t.Errorf("Stdout is not idempotent on %q", s)
		}
	})
}

// checkStdoutWithinText fails t when Stdout withholds a line of s that Text
// keeps. Both keep the line count, so the lines line up.
func checkStdoutWithinText(t *testing.T, s string) {
	t.Helper()
	in := strings.Split(s, "\n")
	so := strings.Split(Stdout(s), "\n")
	tx := strings.Split(Text(s), "\n")
	if len(so) != len(in) || len(tx) != len(in) {
		t.Fatalf("line count changed on %q: Stdout %d, Text %d, input %d", s, len(so), len(tx), len(in))
	}
	for i := range in {
		if so[i] == Marker && in[i] != Marker && tx[i] != Marker {
			t.Errorf("Stdout withholds line %d of %q, and Text keeps it: %q", i, s, tx[i])
		}
	}
}

// TestText_WithholdsStdoutShapes: Text now withholds the shapes its word rule
// did not see (#952): a bare token, a header name outside Args' reach, and a
// PEM block's body.
//
// Mutation that turns it red: the same as
// TestStdout_TextWithholdsEverythingStdoutDoes.
func TestText_WithholdsStdoutShapes(t *testing.T) {
	for _, in := range []string{
		"ghp_" + strings.Repeat("A1", 18),
		"key: sk-ant-api03-" + strings.Repeat("q", 40),
		"-----BEGIN PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END PRIVATE KEY-----",
	} {
		if got := Text(in); strings.Contains(got, "A1A1") || strings.Contains(got, "qqqq") || strings.Contains(got, "MIIE") {
			t.Errorf("Text(%q) = %q, still carries the credential", in, got)
		}
	}
}

// adversarialStdout are inputs built to cost a regex engine more than a
// pass: a URL marker with no '@', a run of token prefixes each one byte
// short, a long run of '@', a PEM armor run, and a NAME= run.
func adversarialStdout(n int) []string {
	return []string{
		strings.Repeat("a://", n/4),
		strings.Repeat("sk-"+strings.Repeat("a", 19)+" ", n/23),
		strings.Repeat("@", n),
		"a:" + strings.Repeat("@", n),
		strings.Repeat("-----BEGIN PRIVATE KEY-----", n/27),
		strings.Repeat("x=", n/2),
		strings.Repeat("Bearer ", n/7),
	}
}

// TestStdout_LinearOnAdversarialInput: Stdout's cost stays linear on each
// 1 MiB adversarialStdout input against the same input at a quarter of the
// size, timed as a CPU-time ratio (perftest.Linear, forgectl#879) rather than
// against a wall-clock bound, so a loaded host does not fail it. RE2 is
// linear; the rest of the scan must stay so too.
//
// Mutation that turns it red: in credentialAssignment, scan back from each
// '=' over every earlier byte, '=' included (isNameByte(c) || c == '='), and
// the NAME= row goes quadratic.
func TestStdout_LinearOnAdversarialInput(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const n, k = 1 << 20, 4
	small, large := adversarialStdout(n/k), adversarialStdout(n)
	for i := range small {
		// Amortize: the cheapest inputs cost under Floor alone.
		smallRun, largeRun := perftest.Amortize(
			func() { _ = Stdout(small[i]) },
			func() { _ = Stdout(large[i]) })
		perftest.Linear(t, "Stdout on adversarial input "+strconv.Itoa(i), k, smallRun, largeRun)
	}
}

// TestStdout_HeaderStartsAWord: a header name matches only as a whole
// hyphenated word, so a longer word that ends in one is kept.
//
// Mutation that turns it red: drop the word-start check in headerIn.
func TestStdout_HeaderStartsAWord(t *testing.T) {
	for _, line := range []string{"fortunecookie: yes", "preauthorization: pending"} {
		if got := Stdout(line); got != line {
			t.Errorf("Stdout(%q) = %q, want it kept", line, got)
		}
	}
	if got := Stdout("Set-Cookie: sid=1"); got != Marker {
		t.Errorf("Stdout(Set-Cookie) = %q", got)
	}
}

// TestStdoutTokens_CoverTokenPrefixes: every credential format tokenPrefixes
// names for a flag name has a Stdout shape (#952), so the two lists do not
// drift apart.
//
// Mutation that turns it red: drop the hf_ entry from stdoutTokens.
func TestStdoutTokens_CoverTokenPrefixes(t *testing.T) {
	for _, p := range tokenPrefixes {
		if p == "xox" {
			continue // tokenAt's own arm
		}
		covered := false
		for _, tok := range stdoutTokens {
			if strings.HasPrefix(strings.ToLower(tok.prefix), p) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("tokenPrefixes has %q and stdoutTokens has no format starting with it", p)
		}
	}
}

func BenchmarkStdout(b *testing.B) {
	var sb strings.Builder
	for sb.Len() < 1<<20 {
		for _, l := range stdoutKept {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	in := sb.String()
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for b.Loop() {
		_ = Stdout(in)
	}
}
