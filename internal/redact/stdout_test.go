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
	// gh auth status. Its masked "  - Token: gho_***" row reads as a YAML
	// credential key and is withheld (stdoutWithheld).
	"  \u2713 Logged in to github.com account octocat (keyring)",
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
	// #974 item 1 near misses: a JSON or YAML credential key with nothing in
	// its value, a key that is not the line's own, a port and path before
	// an '@', an eyJ word with fewer than two dots or short segments, a
	// flag with no value after it, a color code around a plain word.
	`"token": "",`,
	`"password": null`,
	`  "credentials": {`,
	"password:",
	"  token: |",
	"api_key: ~",
	`"tokens": []`,
	"Downloading http://registry.local:4873/@scope/pkg/-/pkg-1.0.0.tgz",
	"GET https://registry.npmjs.org/@anthropic-ai%2fclaude-code 200",
	"eyJhbGciOiJIUzI1NiJ9",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0",
	"eyJ.abcd.efgh",
	"glcbt-short",
	"pass the token with --token",
	"\x1b[32m==>\x1b[0m \x1b[1mUpgrading 1 outdated package:\x1b[0m",
	// #974 item 2, deliverable false positives Stdout keeps and Text still
	// withholds (TestText_KeepsWithholdingStdoutExemptions).
	"basic authentication disabled",
	"bearer authentication is not configured",
	"pass=1 fail=0 skip=2",
	"auth=ok",
	"Cookie: none",
	"> Cookie: none",
	"  auth: true",
	`  "auth": "ok",`,
	" src/token=x.go | 1 +",
	" src/password=reset.go | 12 ++++++------",
	" assets/token=logo.png | Bin 0 -> 1234 bytes",
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
	// #974 item 1: JSON and YAML credential keys.
	`"token": "SEKRIT"`,
	`{"access_token":"SEKRIT","token_type":"bearer"}`,
	`  "password" : "SEKRIT",`,
	`{"client_secret": 12345678}`,
	"password: SEKRIT",
	"  - api_key: SEKRIT",
	"'secret': SEKRIT",
	"  - Token: gho_************************************",
	// The headers Text catches through credentialHeaderName.
	"X-Auth-Token: SEKRIT",
	"< x-github-token: SEKRIT",
	// A credential flag's value after a space.
	"--password SEKRIT",
	"gh auth login --with-token SEKRIT",
	// JWTs, signed and alg none.
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJTRUtSSVQifQ.c2lnbmF0dXJl",
	"id_token=eyJhbGciOiJub25lIn0.eyJhIjoxfQ.",
	// GitLab CI job token.
	"glcbt-64_" + strings.Repeat("x", 20),
	"glcbt-" + strings.Repeat("y", 20),
	// Userinfo whose password holds a raw '/', '?' or '#'; git 2.43 prints
	// the first form as it is.
	"fatal: unable to access 'http://user:pa/SEKRIT@127.0.0.1:9/r/': URL rejected: Port number was not a decimal number between 0 and 65535",
	"http://user:pa?SEKRIT@host/r",
	"https://user:pa#SEKRIT@host/r",
	"https://user:/SEKRIT@host/r",
	// A credential split by a color code, or wrapped in one.
	"ghp_" + strings.Repeat("A", 18) + "\x1b[0m" + strings.Repeat("A", 18),
	"\x1b[1mAuthorization\x1b[0m: SEKRIT",
	"GITHUB_TOKEN=\x1b[31m",
	// #974 item 2 edges that stay withheld in Stdout: the scheme spelled
	// exactly, or a run with a digit or '+', '/', '='; a status word that
	// is not the whole value; a count of four digits; a line with a
	// diffstat bar that is not a diffstat row.
	"Basic authentication disabled",
	"Bearer authentication",
	"basic dXNlcjpTRUtSSVQ=",
	"bearer abc123defghij",
	"PASSWORD=ok horse battery",
	"pass=1234",
	"Cookie: none; sid=SEKRIT",
	"> Cookie: none; sid=SEKRIT",
	"> Cookie: none SEKRIT",
	`"auth": "ok SEKRIT"`,
	"auth: ok SEKRIT",
	" src/app.go | 1 + GITHUB_TOKEN=SEKRIT",
	"GITHUB_TOKEN=SEKRIT src/app.go | 1 +",
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
//
// For #974: drop jsonKeyIn (the JSON rows show) or yamlKey (the YAML rows);
// drop any keyValueCarries empty case (its kept row is withheld); drop
// credentialHeaderName from isCredentialHeader (X-Auth-Token shows); drop
// flagValueIn (--password SEKRIT shows) or its value-after check (pass the
// token with --token is withheld); drop stdoutJWT (the JWT rows show); drop
// the glcbt- entry; drop the raw-'/' userinfo alternative (the git row
// shows) or its port arm (the registry:4873 row is withheld); drop the
// stripCSI pass (the split ghp_ row shows) or the as-is pass
// (GITHUB_TOKEN=ESC[31m shows); make schemeAt ignore case-exactness
// (Basic authentication disabled shows) or the digit check (basic
// dXNlc…= shows); let benignAssignment accept any following word
// (PASSWORD=ok horse battery shows) or benignValue any digit count
// (pass=1234 shows); drop headerIn's textEnd bound (Cookie: none;
// sid=SEKRIT shows); drop the diffstat anchors (the src/app.go row shows).
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

// stdoutExempt are the #974 item-2 lines: deliverable false positives that
// Stdout keeps (they are also stdoutKept rows) and that Text went on
// withholding before #974 and must still withhold.
var stdoutExempt = []string{
	"basic authentication disabled",
	"bearer authentication is not configured",
	"pass=1 fail=0 skip=2",
	"auth=ok",
	"Cookie: none",
	"> Cookie: none",
	" src/token=x.go | 1 +",
	" src/password=reset.go | 12 ++++++------",
	" assets/token=logo.png | Bin 0 -> 1234 bytes",
}

// TestText_KeepsWithholdingStdoutExemptions: Stdout's exemptions for
// deliverable output (#974) are Stdout's alone. Text withholds every line
// they keep, as it did before them, and the Text-mode shape check
// (pemScan{}) still reports each of them.
//
// Mutations that turn it red: make Text's pemScan deliverable (the basic
// and bearer prose rows come through Text, since its word rule does not see
// them); drop the deliverable guard in front of any one exemption
// (schemeAt's, benignAssignment's, benignRest's, diffstatRow's), and its
// rows come through the Text-mode check.
func TestText_KeepsWithholdingStdoutExemptions(t *testing.T) {
	for _, line := range stdoutExempt {
		if got := Stdout(line); got != line {
			t.Errorf("Stdout(%q) = %q, want it kept", line, got)
		}
		if got := Text(line); got != Marker {
			t.Errorf("Text(%q) = %q, want %q as before #974", line, got, Marker)
		}
		var sc pemScan
		if !sc.stdoutWithheld(line) {
			t.Errorf("the Text-mode shape check keeps %q; want it withheld, so Text's own shapes do not lean on lineWithheld", line)
		}
	}
}

// TestText_KeepsEmptyCredentialKeys: a JSON or YAML credential key whose
// value is empty carries nothing, in Text as in Stdout, so the key shape
// (#974) does not add withheld lines to Text that hold no value.
//
// Mutation that turns it red: drop any one keyValueCarries empty case
// ("null", "~", "{", "[]", the empty quoted string, the block-scalar
// indicator); Stdout still keeps a status word such as null, Text does not.
func TestText_KeepsEmptyCredentialKeys(t *testing.T) {
	for _, line := range []string{
		`"token": "",`,
		`"password": null`,
		`  "credentials": {`,
		"password:",
		"  token: |",
		"  token: >-",
		"api_key: ~",
		`"tokens": []`,
	} {
		if got := Text(line); got != line {
			t.Errorf("Text(%q) = %q, want it kept", line, got)
		}
		if got := Stdout(line); got != line {
			t.Errorf("Stdout(%q) = %q, want it kept", line, got)
		}
	}
}

// adversarialStdout are inputs built to cost a regex engine more than a
// pass: a URL marker with no '@', a run of token prefixes each one byte
// short, a long run of '@', a PEM armor run, and a NAME= run; and for #974's
// shapes, runs that make each per-':' or per-word check re-read a shared
// tail: JSON keys with trailing blanks, quotes or digits, header values
// with trailing blanks, eyJ words with no second dot, credential flags with
// no value, userinfo with a port and no '@', lowercase scheme words with a
// letters-only run, status-word assignments, and CSI escapes.
func adversarialStdout(n int) []string {
	return []string{
		strings.Repeat("a://", n/4),
		strings.Repeat("sk-"+strings.Repeat("a", 19)+" ", n/23),
		strings.Repeat("@", n),
		"a:" + strings.Repeat("@", n),
		strings.Repeat("-----BEGIN PRIVATE KEY-----", n/27),
		strings.Repeat("x=", n/2),
		strings.Repeat("Bearer ", n/7),
		strings.Repeat(`"token":""`, n/20) + strings.Repeat(" ", n/2),
		strings.Repeat(`"abcdefgh"   :`, n/14),
		strings.Repeat(`"token":null,`, n/13),
		"x " + strings.Repeat("xcookie: ", n/9),
		strings.Repeat("a:", n/2),
		"Cookie: none" + strings.Repeat(" ", n),
		strings.Repeat("eyJaaaaaaaa.aaa ", n/16),
		"eyJ" + strings.Repeat("a", n),
		strings.Repeat("eyJ-", n/4),
		"--token" + strings.Repeat(" ", n),
		strings.Repeat("--tok ", n/6),
		strings.Repeat("-", n),
		strings.Repeat("h://u:1/", n/8),
		"h://u:" + strings.Repeat("a/", n/2),
		strings.Repeat("basic "+strings.Repeat("a", 12)+" ", n/20),
		"basic " + strings.Repeat("a", n),
		strings.Repeat("pass=1 ", n/7),
		strings.Repeat("auth=ok&", n/8),
		strings.Repeat("\x1b[", n/2),
		strings.Repeat("\x1b[0m", n/4) + "x",
		"password: |" + strings.Repeat("-", n),
		" " + strings.Repeat("a", n) + " | 1 +",
	}
}

// TestStdout_LinearOnAdversarialInput: Stdout's cost stays linear on each
// 1 MiB adversarialStdout input against the same input at a quarter of the
// size, timed as a CPU-time ratio (perftest.Linear, forgectl#879) rather than
// against a wall-clock bound, so a loaded host does not fail it. RE2 is
// linear; the rest of the scan must stay so too.
//
// Text runs the same shapes without Stdout's exemptions, so each family is
// timed through Text too.
//
// Mutations that turn it red: in credentialAssignment, scan back from each
// '=' over every earlier byte, '=' included (isNameByte(c) || c == '='), and
// the NAME= row goes quadratic; in headerIn, check the benign value with
// benignValue(strings.TrimSpace(line[i+1:])) in place of benignRest (the
// Cookie: none row goes quadratic).
func TestStdout_LinearOnAdversarialInput(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const n, k = 1 << 20, 4
	small, large := adversarialStdout(n/k), adversarialStdout(n)
	for i := range small {
		perftest.Linear(t, "Stdout on adversarial input "+strconv.Itoa(i), k,
			func() { _ = Stdout(small[i]) },
			func() { _ = Stdout(large[i]) })
		perftest.Linear(t, "Text on adversarial input "+strconv.Itoa(i), k,
			func() { _ = Text(small[i]) },
			func() { _ = Text(large[i]) })
	}
}

// TestStdout_HeaderStartsAWord: a header name matches only as a whole
// hyphenated word, so a longer word that ends in one is kept.
//
// Mutation that turns it red: drop the word-start check in headerIn.
func TestStdout_HeaderStartsAWord(t *testing.T) {
	// Each starts after a word, so the YAML key shape does not apply.
	for _, line := range []string{"x fortunecookie: yes", "x preauthorization: pending"} {
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
