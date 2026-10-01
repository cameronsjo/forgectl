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
	// #983: a diffstat row whose path ends in an extension, a pass= count
	// beside a fail= one, an empty TOML or Go value, a comparison, and the
	// deliverable false positives item 3 lists (Text still withholds them).
	" docs/password=reset.md | 3 ++-",
	"PASS=3 FAIL=0",
	`password = ""`,
	`token := ""`,
	"token == x",
	"token: expired",
	"password: required",
	"auth: required",
	"token_expiry: 3600",
	`"token_type": "bearer"`,
	"password_policy: strict",
	"tokenType: bearer",
	"Signature: valid",
	"credential.helper: osxkeychain",
	"credentials: ~/.aws/credentials",
	"use --token to pass it",
	// docker pull.
	"docker.io/library/node:22@sha256:" + strings.Repeat("0f", 32),
	// #992: a repository path, after a registry host with a port.
	"registry.local:5000/team/app:1.2@sha256:" + strings.Repeat("0f", 32),
	"ghcr.io/o/app:v1@sha256:" + strings.Repeat("0f", 32),
	"Digest: sha256:" + strings.Repeat("0f", 32),
	"Status: Downloaded newer image for node:22",
	// #996: a flow mapping with no credential key or an empty value, a
	// credential key after a ',' outside any '{', a word that only starts
	// like a declaration keyword, and escapes around plain text.
	"config: {user: me, region: us-east-1}",
	"{password: null, user: me}",
	"{ token: null, user: 'me' }",
	"- {token: '', name: x}",
	"ran {job}, password: incorrect",
	"constant token = abc",
	"constkey = abc",
	"\x1b(B\x1b[mplain text\x1b7 and \x1b#8more",
	"\x1bPq#0;2\x1b\\done",
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
	"pass=1234 fail=0",
	"Cookie: none; sid=SEKRIT",
	"> Cookie: none; sid=SEKRIT",
	"> Cookie: none SEKRIT",
	`"auth": "ok SEKRIT"`,
	"auth: ok SEKRIT",
	" src/app.go | 1 + GITHUB_TOKEN=SEKRIT",
	"GITHUB_TOKEN=SEKRIT src/app.go | 1 +",
	// #983 item 1: a diffstat-shaped line whose path is no file path, or
	// whose credential NAME= value is a directory or has no extension; a
	// lowercase scheme word before a run that decodes as user:pass or runs 16
	// bytes, and the scheme in capitals; a count that is no pass= beside a
	// fail=; a key whose value is a count.
	"PASSWORD=hunter2 | 3",
	" GITHUB_TOKEN=abc | 1 +",
	" src/GITHUB_TOKEN=abc/x.go | 1 +",
	" src/GITHUB_TOKEN=SEKRIT | 2 +-",
	"basic dXNlcjpwYXNz",
	"bearer abcdefghijklmnop",
	"BEARER abcdefghij",
	"BASIC abcdefghijkl",
	"password=123",
	"pass=1",
	"pass=12 skip=0",
	"DB_PASS=12 fail=0",
	`"auth": 1`,
	"auth: 1",
	// #983 item 2: a Python dict key, TOML and Go assignments, and a token
	// split by an OSC 8 hyperlink (ESC \ or BEL terminated).
	"{'token': 'abc'}",
	"{'password': 'SEKRIT', 'user': 'me'}",
	`password = "x"`,
	"token = 'abcdef'",
	`  api_key = "SEKRIT"`,
	`"token" = "SEKRIT"`,
	`token := "abc"`,
	"\tsecret := SEKRIT",
	"ghp_" + strings.Repeat("A", 18) + "\x1b]8;;\x1b\\" + strings.Repeat("A", 18),
	"ghp_" + strings.Repeat("A", 18) + "\x1b]8;;https://example.com\x07" + strings.Repeat("A", 18),
	// #983 item 3 and 4 edges that stay withheld in Stdout: a status word
	// that is not the whole value, a descriptor that is not the name's last
	// segment, a path outside the known roots or holding a blank, a flag
	// followed by a value, a digest that is not 64 hex, userinfo beside a
	// digest.
	"token: expired SEKRIT",
	"token_type_secret: SEKRIT",
	"credentials: /SEKRIT/x",
	"credentials: ~/x SEKRIT",
	"use --token SEKRIT",
	"user:SEKRIT@sha256:abc",
	// #990 review: Vault AppRole's secret_id is a bearer secret, not the id
	// of one, as a JSON, YAML or TOML key, in camel case, after another word;
	// and a TOML assignment with its blank after the '=' alone.
	`"secret_id": "6a174c20-f6de-a53c-74d2-6018fcceff64",`,
	"secret_id: 6a174c20-f6de-a53c-74d2-6018fcceff64",
	`secret_id = "6a174c20-f6de-a53c-74d2-6018fcceff64"`,
	"secretId: SEKRIT",
	"SECRET_ID: SEKRIT",
	"client_secret_id: SEKRIT",
	`password= "hunter2"`,
	"token= 'abc'",
	"https://u:SEKRIT@host/x@sha256:" + strings.Repeat("0f", 32),
	// #992: a scheme-less digest word with no repository path cannot be
	// told from user:pass, so it is withheld.
	"user:SEKRIT@sha256:" + strings.Repeat("0f", 32),
	"node:22@sha256:" + strings.Repeat("0f", 32),
	// #996 item 1: a credential key inside a YAML flow mapping, first,
	// after a ',', nested, in a sequence entry, with no blank after ':'.
	"config: {password: SEKRIT, user: me}",
	"config: {user: me, api_key: SEKRIT}",
	"outer: {inner: {token: SEKRIT}}",
	"- {secret: SEKRIT}",
	"{password:SEKRIT}",
	// Gate 2: a '}' inside a quoted value does not close the mapping.
	`{note: "}", password: SEKRIT}`,
	"{note: '}', token: SEKRIT}",
	"it's {password: SEKRIT} isn't it",
	// #996 item 2: a token straight after a non-CSI/OSC escape (tput sgr0's
	// ESC ( B, an Fp ESC 7, an nF ESC # 8), one split by a DCS or APC
	// string or an nF escape, and one whose first letter a crafted CSI's
	// final byte takes.
	"\x1b(B\x1b[mghp_" + strings.Repeat("A1", 18),
	"\x1b7ghp_" + strings.Repeat("A1", 18),
	"\x1b#8ghp_" + strings.Repeat("A1", 18),
	"ghp_" + strings.Repeat("A", 18) + "\x1bPq#0\x1b\\" + strings.Repeat("A", 18),
	"ghp_" + strings.Repeat("A", 18) + "\x1b_x\a" + strings.Repeat("A", 18),
	"ghp_" + strings.Repeat("A", 18) + "\x1b(B" + strings.Repeat("A", 18),
	"\x1b[1ghp_" + strings.Repeat("A1", 18),
	// #996 Gate 2: a colored token inside a DCS or APC string body (tmux's
	// passthrough doubles each ESC), or after a lone ESC '\', which main
	// withheld; and one inside an OSC body, which it did not.
	"\x1bPtmux;\x1b\x1b[1mghp_" + strings.Repeat("A1", 18) + "\x1b\x1b[0m\x1b\\",
	"\x1b_ \x1b[1mghp_" + strings.Repeat("A1", 18) + "\x1b[0m\a",
	"\x1bP \x1b[1mghp_" + strings.Repeat("A1", 18) + "\x1b[0m\x1b\\",
	"see\x1b\\\x1b[32mghp_" + strings.Repeat("A1", 18),
	"\x1b] \x1b[1mghp_" + strings.Repeat("A1", 18) + "\a",
	// #996 item 3: a declaration keyword before the assignment.
	`const token = "SEKRIT"`,
	`var password = "SEKRIT"`,
	"let secret = SEKRIT",
	`export const apiKey = "SEKRIT"`,
	"  export token = SEKRIT",
	"\tvar\tpassword := SEKRIT",
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
// For #983: make diffstatPath return true (PASSWORD=hunter2 | 3 shows) or
// drop its fileNameWithExtension check (src/GITHUB_TOKEN=abc/x.go shows);
// drop BEARER or BASIC from schemeAt's exact spellings (the capitals rows
// show); drop bearer's 16-byte arm (bearer abcdefghijklmnop shows) or make
// basicCredential false (basic dXNlcjpwYXNz shows); let benignValue accept a
// count again (password=123 shows), or drop benignAssignment's fail= context
// (pass=1 shows) or its pass name (DB_PASS=12 fail=0 shows); accept only '"'
// in jsonKeyIn (the dict rows show); drop assignKey (the TOML and Go rows),
// its ":=" arm (the Go rows), its "==" guard (token == x is withheld) or its
// spacing rule (pass=1 fail=0 skip=2 is withheld); drop stripEscapes' OSC arm
// (the hyperlink rows show); drop withoutDigestRefs (the docker row is
// withheld) or digestRef's 64-byte check (user:SEKRIT@sha256:abc shows),
// or its repository-path check (#992: user:SEKRIT@sha256:<64 hex> shows);
// drop keyStatusValues (token: expired is withheld) or accept any word in it
// (token: expired SEKRIT shows); drop keyNameCarries' descriptor check
// (token_type rows are withheld) or read the first segment
// (token_type_secret shows); drop pathValue (the ~/.aws row is withheld) or
// accept any '/' start (credentials: /SEKRIT/x shows); drop flagProse (use
// --token to pass it is withheld) or accept any word (use --token SEKRIT
// shows).
//
// For #974: drop jsonKeyIn (the JSON rows show) or yamlKey (the YAML rows);
// drop any keyValueCarries empty case (its kept row is withheld); drop
// credentialHeaderName from isCredentialHeader (X-Auth-Token shows); drop
// flagValueIn (--password SEKRIT shows) or its value-after check (pass the
// token with --token is withheld); drop stdoutJWT (the JWT rows show); drop
// the glcbt- entry; drop the raw-'/' userinfo alternative (the git row
// shows) or its port arm (the registry:4873 row is withheld); drop the
// stripEscapes pass (the split ghp_ row shows) or the as-is pass
// (GITHUB_TOKEN=ESC[31m shows); make schemeAt ignore case-exactness
// (Basic authentication disabled shows) or the digit check (basic
// dXNlc…= shows); let benignAssignment accept any following word
// (PASSWORD=ok horse battery shows) or isCount any digit count
// (pass=1234 fail=0 shows); drop headerIn's textEnd bound (Cookie: none;
// sid=SEKRIT shows); drop the diffstat anchors (the src/app.go row shows).
//
// For #996: drop flowKeyIn (the flow-mapping rows show), its depth check
// (ran {job}, password: incorrect is withheld), its ',' before a key (the
// api_key-after-user row shows) or its keyValueCarries check ({password:
// null, …} is withheld), or read keys without Stdout's descriptor exemption
// ({token_type: bearer} is withheld); drop the declaration keywords (the
// const/var/let rows show) or the export one (export const apiKey shows),
// or the blank required after one (constkey = abc is withheld); drop
// escEnd (tput sgr0's ESC ( B row shows) or its intermediate bytes (the
// same), the DCS/SOS/PM/APC string arm (the DCS- and APC-split rows show),
// or escapeView's keep-final pass or its kept final byte (the crafted
// ESC [ 1 g row shows).
//
// For #996's Gate 2: drop the string-body view from escapeViews (the OSC
// body row shows); check the full strip in place of main's stripEscapes
// (TestText_WithholdsSupersetOfMain fails); let a quoted value's braces count
// toward flowKeyIn's depth (the {note: "}"} rows show) or let any quote
// start one (it's {password: …} isn't shows).
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
	// #983 items 3 and 4.
	"PASS=3 FAIL=0",
	"token: expired",
	"password: required",
	"auth: required",
	"token_expiry: 3600",
	`"token_type": "bearer"`,
	"password_policy: strict",
	"tokenType: bearer",
	"Signature: valid",
	"credential.helper: osxkeychain",
	"credentials: ~/.aws/credentials",
	"use --token to pass it",
	"docker.io/library/node:22@sha256:" + strings.Repeat("0f", 32),
	// #996: a descriptor key in a flow mapping and after a declaration; and
	// (Gate 2) a flow value of undefined, as node prints an object, or a mask.
	"{token_type: bearer}",
	"{ code: 'E401', token: undefined }",
	"{Name:foo, Token:***}",
	`const tokenType = "bearer"`,
}

// TestText_KeepsWithholdingStdoutExemptions: Stdout's exemptions for
// deliverable output (#974) are Stdout's alone. Text withholds every line
// they keep, as it did before them, and the Text-mode shape check
// (pemScan{}) still reports each of them.
//
// Mutations that turn it red: make Text's pemScan deliverable (the basic
// and bearer prose rows come through Text, since its word rule does not see
// them); drop the deliverable guard in front of any one exemption
// (schemeAt's, benignAssignment's, benignRest's, diffstatRow's, and #983's
// keyStatusValues/pathValue in keyValueCarries, keyNameCarries'
// descriptors, flagValueIn's flagProse, userinfoIn's withoutDigestRefs), and
// its rows come through the Text-mode check.
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

// TestStdout_YAMLValueOnNextLine: a YAML credential key with nothing after
// its ':', or a block scalar's indicator, holds its value on the lines after
// it (#983). Those lines are withheld while they are indented past the key,
// or start a sequence entry at the key's indentation under a key with no
// "- " before it; a blank line keeps the value going and stays as it is; the
// first line at the key's indentation or less ends it. Text and Stdout agree
// on every case.
//
// Mutations that turn it red: drop openValue from stdoutWithheld (every
// value line shows); compare indent >= valueIndent (the next key, "other:",
// is withheld); drop the valueSeq arm (the "- a" rows under password: show);
// accept no '\r' after the ':' (the CRLF value shows); drop openerValue
// (the # db, !!binary, &pw rows' values show), or its '#' arm (# db), or
// its property loop (!!binary, &pw), or read a '#' with no blank before it
// as a comment (|#x's next line is withheld); set valueSeq for a dashed key
// too (the "- other" sibling is withheld); end
// the value on a blank line (the line after it shows); drop the stripEscapes
// retry in openValue (the colored key's value shows); open on any key
// (token_type: under Stdout withholds its next line, where Text alone
// should).
func TestStdout_YAMLValueOnNextLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"password: |\n  SEKRIT\n  more\nother: 1", "password: |\n" + Marker + "\n" + Marker + "\nother: 1"},
		{"password: >-\n    SEKRIT\nnext", "password: >-\n" + Marker + "\nnext"},
		{"password:\n  SEKRIT\nother: x", "password:\n" + Marker + "\nother: x"},
		{"  token:\n    SEKRIT\n  other: x", "  token:\n" + Marker + "\n  other: x"},
		{"password:\n- SEKRIT\n- two\nnext", "password:\n" + Marker + "\n" + Marker + "\nnext"},
		{"- password:\n  SEKRIT\n- other", "- password:\n" + Marker + "\n- other"},
		{"password: |\n  SEKRIT\n\n  more\nx", "password: |\n" + Marker + "\n\n" + Marker + "\nx"},
		{"\x1b[1mpassword\x1b[0m:\n  SEKRIT", "\x1b[1mpassword\x1b[0m:\n" + Marker},
		{"password:\nnext: 1", "password:\nnext: 1"},
		{"password: x\n  SEKRIT", Marker + "\n  SEKRIT"},
		// #990 review: a CRLF line, and a value whose node properties or
		// comment set aside leave it empty or a block indicator. The key line
		// itself is withheld when its value as written carries something.
		{"password:\r\n  SEKRIT\r\nx", "password:\r\n" + Marker + "\nx"},
		{"password: # db\n  SEKRIT\nx", Marker + "\n" + Marker + "\nx"},
		{"password: !!binary |\n  SEKRIT\nx", Marker + "\n" + Marker + "\nx"},
		{"password: &pw\n  SEKRIT\nx", Marker + "\n" + Marker + "\nx"},
		{"password: !secret &pw | # c\n  SEKRIT\nx", Marker + "\n" + Marker + "\nx"},
		{"password: !secret\n  SEKRIT\nx", Marker + "\n" + Marker + "\nx"},
		{"password: |#x\n  SEKRIT", Marker + "\n  SEKRIT"},
	} {
		if got := Stdout(c.in); got != c.want {
			t.Errorf("Stdout(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A descriptor key (#983 item 3) opens no value in Stdout; Text, with no
	// exemptions, withholds the value line.
	in := "token_type:\n  bearer"
	if got := Stdout(in); got != in {
		t.Errorf("Stdout(%q) = %q, want it kept", in, got)
	}
	if got := Text(in); got != "token_type:\n"+Marker {
		t.Errorf("Text(%q) = %q, want the value line withheld", in, got)
	}
}

// TestStdout_MultiLineString: a credential key whose value opens a string
// that does not close on the key's line holds the credential on the lines
// after it (#991): a TOML multi-line basic ("""…""") or literal (three single quotes)
// string, or a YAML double-quoted scalar. Every line through the one that
// closes the string is withheld; an empty line inside one carries nothing and
// stays empty; with no closing delimiter the rest of the text is withheld.
// Text and Stdout agree on every case.
//
// Mutations that turn it red: drop openString from stdoutWithheld (the lines
// after every opener show); drop the inString arm (the same); drop the
// stripEscapes retry in openString (the colored key's value shows); read
// '\' as no escape in a basic string (SEKRIT2 after the escaped """ shows,
// and the YAML escaped-quote row's "more" line shows); read '\' as an escape
// in a literal string (the line after a backslash and the closer is withheld); skip the escaped
// '\' itself (the line after a\\""" is withheld); search for the closer from
// the opener rather than past it (password = """ closes on its own line,
// so SEKRIT shows); drop the YAML arm (the YAML rows show) or its node
// property skip (the !!str row shows); open on any key (the description
// row is withheld) or ignore Stdout's descriptor exemption (token_type's
// string is withheld by Stdout); drop Stdout's scan from Text (the
// token_type-then-password case: Text keeps SEKRIT, which Stdout withholds);
// accept no bare '=' before the delimiter (password=""" shows SEKRIT); end the
// string on a closer the line as it is shows but its escape-stripped views do
// not (the OSC-body closer row's SEKRIT2 shows).
func TestStdout_MultiLineString(t *testing.T) {
	m := Marker
	for _, c := range []struct{ in, want string }{
		{"password = \"\"\"\n  SEKRIT\n  more\"\"\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"token = '''\nSEKRIT\n'''\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"password = \"\"\"SEKRIT\nmore\n\"\"\"", m + "\n" + m + "\n" + m},
		{"  \"api_key\" = \"\"\"\nSEKRIT\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"password = \"\"\"\nSEKRIT\n\nmore\n\"\"\"\nafter", m + "\n" + m + "\n\n" + m + "\n" + m + "\nafter"},
		{"password = \"\"\"\nSEK\\\"\"\"RIT\nSEKRIT2\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\n" + m + "\nafter"},
		{"password = \"\"\"\na\\\\\"\"\"\nafter", m + "\n" + m + "\nafter"},
		{"token = '''\na\\'''\nafter", m + "\n" + m + "\nafter"},
		{"password = \"\"\"x\"\"\"\nafter", m + "\nafter"},
		{"password = \"\"\"\nSEKRIT\nmore", m + "\n" + m + "\n" + m},
		{"password = \"\"\"\r\nSEKRIT\r\n\"\"\"\r\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"\x1b[1mpassword\x1b[0m = \"\"\"\nSEKRIT\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"private_key = '''\n-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n'''\nafter", m + "\n" + m + "\n" + m + "\n" + m + "\n" + m + "\nafter"},
		{"password: \"a\n  SEKRIT\"\nafter", m + "\n" + m + "\nafter"},
		{"  - token: \"a\n    SEK\\\"RIT\n    more\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"password: \"a\\\\\"\nafter", m + "\nafter"},
		{"password: \"abc\"\nafter", m + "\nafter"},
		{"password: !!str \"a\n  SEKRIT\"\nafter", m + "\n" + m + "\nafter"},
		{"password: \"a\n  b\n  c", m + "\n" + m + "\n" + m},
		// Gate 2: no blank around the '=' (valid TOML), or one before it
		// only, and Python's PASSWORD = """ spelled bare.
		{"password=\"\"\"\nSEKRIT\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"password='''\nSEKRIT\n'''\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"password =\"\"\"\nSEKRIT\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		{"PASSWORD=\"\"\"\nSEKRIT\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\nafter"},
		// Gate 2: a closer seen only as the line is written, inside an OSC
		// body a terminal does not show, does not end the string; nor does
		// one seen only once the escapes are stripped.
		{"password = \"\"\"\n\x1b]8;;\"\"\"\aSEKRIT\nSEKRIT2\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\n" + m + "\nafter"},
		{"password = \"\"\"\nSEKRIT\"\"\x1b[0m\"\nSEKRIT2\n\"\"\"\nafter", m + "\n" + m + "\n" + m + "\n" + m + "\nafter"},
		{"description = \"\"\"\ntext\n\"\"\"", "description = \"\"\"\ntext\n\"\"\""},
		{"summary: \"a\n  text\"", "summary: \"a\n  text\""},
	} {
		if got := Stdout(c.in); got != c.want {
			t.Errorf("Stdout(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A descriptor key opens no string in Stdout; Text, with no exemptions,
	// withholds through the closer.
	in := "token_type = \"\"\"\nbearer\n\"\"\"\nafter"
	if got := Stdout(in); got != in {
		t.Errorf("Stdout(%q) = %q, want it kept", in, got)
	}
	if got, want := Text(in), m+"\n"+m+"\n"+m+"\nafter"; got != want {
		t.Errorf("Text(%q) = %q, want %q", in, got, want)
	}
	// Text reads the second line as the descriptor's string closing, Stdout
	// as a key opening one. Text must still withhold every line Stdout does.
	checkStdoutWithinText(t, "token_type = \"\"\"\npassword = \"\"\"\nSEKRIT\n\"\"\"")
	checkStdoutWithinText(t, "token_type:\n  a\n  password: \"x\nSEKRIT\"")
}

// descriptorRows are a key per keyDescriptors entry, each a credential word
// then the descriptor, with a value that carries something: Stdout keeps
// each (#983 item 3) and Text withholds it.
var descriptorRows = map[string]string{
	"type": "token_type: abc", "expiry": "token_expiry: abc", "expires": "token_expires: abc",
	"expiration": "password_expiration: abc", "policy": "password_policy: abc", "helper": "credential.helper: abc",
	"ttl": "token_ttl: abc", "url": "auth_url: abc", "uri": "token_uri: abc", "endpoint": "token_endpoint: abc",
	"file": "password_file: abc", "path": "secret_path: abc", "id": "key_id: abc", "name": "secret_name: abc",
	"scope": "token_scope: abc", "scopes": "tokenScopes: abc", "length": "password_length: abc",
	"format": "key_format: abc", "prefix": "token_prefix: abc",
}

// TestStdout_KeyDescriptors: every keyDescriptors entry has a descriptorRows
// row, which Stdout keeps and Text withholds, and every row names an entry,
// so adding or dropping a descriptor turns it red.
//
// Mutations that turn it red: drop any one keyDescriptors entry (its row is
// withheld); add one (no row names it); drop the secret exception in
// keyNameCarries (the secret_id rows in stdoutWithheld show).
func TestStdout_KeyDescriptors(t *testing.T) {
	for d := range keyDescriptors {
		if _, ok := descriptorRows[d]; !ok {
			t.Errorf("keyDescriptors has %q and descriptorRows has no row for it", d)
		}
	}
	for d, line := range descriptorRows {
		if !keyDescriptors[d] {
			t.Errorf("descriptorRows has %q, which keyDescriptors does not", d)
		}
		if got := Stdout(line); got != line {
			t.Errorf("Stdout(%q) = %q, want it kept", line, got)
		}
		if got := Text(line); got != Marker {
			t.Errorf("Text(%q) = %q, want %q", line, got, Marker)
		}
		checkStdoutWithinText(t, line)
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
		// #983: unterminated and terminated OSC runs, single-quoted keys,
		// TOML and Go assignments with long blank runs, YAML values on the
		// next lines, basic runs to decode, digest words, flag prose,
		// diffstat paths of many NAME= segments, a pass= run beside one
		// fail=, descriptor keys, and a long path value.
		strings.Repeat("\x1b]", n/2),
		"\x1b]" + strings.Repeat("a", n) + "\x1b[0m",
		strings.Repeat("\x1b]8;;\x07x", n/6),
		strings.Repeat("{'token':''}", n/12),
		"token" + strings.Repeat(" ", n) + "= x",
		"token :=" + strings.Repeat(" ", n),
		strings.Repeat("password:\n  x\n", n/14),
		"password:\n" + strings.Repeat("  hunter2\n", n/10),
		"basic " + strings.Repeat("dXNl", n/4),
		strings.Repeat("basic abcdefghijkl ", n/19),
		strings.Repeat("a:1@sha256:"+strings.Repeat("0f", 32)+" ", n/76),
		"a:1@sha256:" + strings.Repeat("a", n),
		strings.Repeat("--token to ", n/11),
		" " + strings.Repeat("a/token=", n/8) + " | 1 +",
		strings.Repeat("pass=1 ", n/7) + "fail=0",
		strings.Repeat(`"token_type": `, n/14),
		"credentials: ~/" + strings.Repeat("a", n),
		// #990 review: node properties and comment runs in an opener's value,
		// a "= " assignment, and descriptor names with many segments.
		strings.Repeat("password: !a &b # c\n  x\n", n/22),
		"password: " + strings.Repeat("!a ", n/3),
		"password: " + strings.Repeat("a#", n/2),
		strings.Repeat("password= \"x\"\n", n/14),
		strings.Repeat("a_secret_id: x\n", n/15),
		"token" + strings.Repeat("_secret", n/7) + "_id: x",
		// #991: an unclosed multi-line string over many lines, many short
		// ones, an opener line of escapes, a YAML scalar of backslashes, and
		// YAML openers each closed by the next.
		"password = \"\"\"\n" + strings.Repeat("a\n", n/2),
		strings.Repeat("password = \"\"\"\nx\n\"\"\"\n", n/22),
		"password = \"\"\"" + strings.Repeat("\\\"", n/2),
		"password: \"" + strings.Repeat("\\", n),
		strings.Repeat("token: \"a\n", n/10),
		"token = '''\n" + strings.Repeat("''", n/2),
		"password = \"\"\"" + strings.Repeat("\"\"a", n/3),
		"password = \"\"\"\n" + strings.Repeat("\"\"a", n/3),
		"password: \"" + strings.Repeat("a", n),
		// #992: digest words with a repository path, and one long name.
		strings.Repeat("r/a:1@sha256:"+strings.Repeat("0f", 32)+" ", n/78),
		"a:1" + strings.Repeat("x", n) + "/b@sha256:" + strings.Repeat("0f", 32),
		// #996: nested and long flow mappings, a run of keys with no value,
		// non-CSI/OSC escapes and unterminated DCS and APC strings, crafted
		// CSI finals, and runs of declaration keywords and blanks.
		strings.Repeat("{password: ", n/11),
		"{" + strings.Repeat("password: x,", n/12),
		"{" + strings.Repeat("a: ", n/3),
		"{" + strings.Repeat(" ", n) + "token:",
		strings.Repeat("}{", n/2) + "a:",
		strings.Repeat("\x1b(", n/2),
		strings.Repeat("\x1b(B", n/3),
		"\x1b" + strings.Repeat(" ", n),
		strings.Repeat("\x1bP", n/2),
		"\x1b_" + strings.Repeat("a", n),
		strings.Repeat("\x1b[1g", n/4) + "x",
		strings.Repeat("export ", n/7),
		"const" + strings.Repeat(" ", n) + "token = x",
		"export" + strings.Repeat("\t", n) + "let token = x",
		// Gate 2: unclosed quotes that each start a value in a flow
		// mapping, blank runs before quotes, and quoted values holding
		// braces; escape strings, lone terminators and BELs for the string
		// body view; and closers split by escapes inside a string.
		"{a:" + strings.Repeat(",\"", n/2),
		"{a:" + strings.Repeat(", '", n/3),
		"{" + strings.Repeat(" ", n) + "\"",
		strings.Repeat("{a: \"}\", ", n/8),
		strings.Repeat("\x1bP\x1b[1m", n/6),
		strings.Repeat("\x1b\\\a", n/3),
		"password = \"\"\"\n" + strings.Repeat("\"\x1b[0m\"\"\n", n/8),
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
		// Amortize: the cheapest inputs cost under Floor alone.
		smallRun, largeRun := perftest.Amortize(
			func() { _ = Stdout(small[i]) },
			func() { _ = Stdout(large[i]) })
		perftest.Linear(t, "Stdout on adversarial input "+strconv.Itoa(i), k, smallRun, largeRun)
		textSmall, textLarge := perftest.Amortize(
			func() { _ = Text(small[i]) },
			func() { _ = Text(large[i]) })
		perftest.Linear(t, "Text on adversarial input "+strconv.Itoa(i), k, textSmall, textLarge)
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
