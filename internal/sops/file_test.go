package sops

// Test plan for file.go
//
// IsSOPSFile
//   [x] True for a document with a top-level sops: mapping
//   [x] False for plain YAML, for invalid YAML, for a `sops:` that is a
//       scalar or a sequence rather than a mapping, and for a nested one
//
// IsSOPSFileName
//   [x] Accepted: the eight allowed shapes
//   [x] Refused: .env names, a bare .yaml, a different case, a path rather
//       than a basename
//
// ReadPlaintextRules / WouldStoreCleartext
//   [x] unencrypted_suffix refuses a matching key and admits others
//   [x] The _unencrypted DEFAULT applies when the file configures no rule
//   [x] encrypted_suffix refuses a key that does NOT match
//   [x] encrypted_regex refuses a key that does NOT match
//   [x] unencrypted_regex refuses a key that DOES match
//   [x] A reason names the rule, never the key
//   [x] An uncompilable or implausibly long rule refuses

import (
	"strings"
	"testing"
)

// segmentEchoed reports whether reason contains any path segment. Every
// segment is a candidate secret — the sops path grammar admits hyphens and so
// matches more real credential shapes than internal/env's ValidKey — so a
// reason must name the RULE and never the input.
func segmentEchoed(reason string, path []string) bool {
	for _, segment := range path {
		if strings.Contains(reason, segment) {
			return true
		}
	}
	return false
}

func TestIsSOPSFile(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want bool
	}{
		{
			name: "a real sops document",
			doc:  "key: ENC[AES256_GCM,data:abc]\nsops:\n    mac: ENC[x]\n    version: 3.13.3\n",
			want: true,
		},
		{
			name: "plain yaml",
			doc:  "key: value\nother: thing\n",
			want: false,
		},
		{
			name: "invalid yaml",
			doc:  "key: [unclosed\n",
			want: false,
		},
		{
			name: "sops as a scalar",
			doc:  "sops: not-a-mapping\n",
			want: false,
		},
		{
			name: "sops as a sequence",
			doc:  "sops:\n    - one\n    - two\n",
			want: false,
		},
		{
			name: "sops nested rather than top level",
			doc:  "outer:\n    sops:\n        mac: ENC[x]\n",
			want: false,
		},
		{
			name: "empty",
			doc:  "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSOPSFile([]byte(c.doc)); got != c.want {
				t.Errorf("IsSOPSFile = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsSOPSFileName(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"secrets.sops.yaml", true},
		{"secrets.sops.yml", true},
		{"anything.sops.yaml", true},
		{"prod.enc.yaml", true},
		{"prod.enc.yml", true},
		{"secrets.yaml", true},
		{"secrets.yml", true},
		{"secrets.prod.yaml", true},
		{"secrets.staging.yml", true},

		{".env", false},
		{".env.prod", false},
		{"prod.env", false},
		{"values.yaml", false},
		{"config.yml", false},
		{"secrets", false},
		{"secretsyaml", false},
		{"Makefile", false},
		{".git/config", false},
		// Byte-exact on purpose. APFS is case-insensitive, so this names the
		// same file as secrets.yaml — refusing it errs toward refusing a
		// legitimate file rather than admitting an unintended one.
		{"SECRETS.YAML", false},
		// A path, not a basename. The caller passes filepath.Base; this
		// pins that a full path does not sneak through a wildcard.
		{"nested/secrets.yaml", false},
	}
	for _, c := range cases {
		t.Run(c.base, func(t *testing.T) {
			if got := IsSOPSFileName(c.base); got != c.want {
				t.Errorf("IsSOPSFileName(%q) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}

func TestWouldStoreCleartext(t *testing.T) {
	cases := []struct {
		name       string
		doc        string
		path       []string
		wantClear  bool
		wantReason string
	}{
		// The ANCESTOR cases are the reason this takes a path rather than a
		// leaf, and they were a live defect: a secret landed in plaintext and
		// the command reported success. Measured against sops 3.13.3 —
		// `unencrypted_suffix: _unencrypted` leaves the whole subtree under
		// `notes_unencrypted` in the clear, and `encrypted_regex: ^app$`
		// encrypts everything under `app` however deep.
		{
			name:       "an ancestor carries the unencrypted_suffix",
			doc:        "sops:\n    unencrypted_suffix: _unencrypted\n    mac: ENC[x]\n",
			path:       []string{"notes_unencrypted", "token"},
			wantClear:  true,
			wantReason: "unencrypted_suffix",
		},
		{
			name:       "a middle segment carries the unencrypted_suffix",
			doc:        "sops:\n    unencrypted_suffix: _unencrypted\n    mac: ENC[x]\n",
			path:       []string{"app", "notes_unencrypted", "deep", "token"},
			wantClear:  true,
			wantReason: "unencrypted_suffix",
		},
		{
			name:      "an ancestor satisfies the encrypted_regex",
			doc:       "sops:\n    encrypted_regex: '^app$'\n    mac: ENC[x]\n",
			path:      []string{"app", "token"},
			wantClear: false,
		},
		{
			name:      "a distant ancestor satisfies the encrypted_regex",
			doc:       "sops:\n    encrypted_regex: '^app$'\n    mac: ENC[x]\n",
			path:      []string{"app", "inner", "deep"},
			wantClear: false,
		},
		{
			name:       "no segment satisfies the encrypted_regex",
			doc:        "sops:\n    encrypted_regex: '^app$'\n    mac: ENC[x]\n",
			path:       []string{"other", "token"},
			wantClear:  true,
			wantReason: "encrypted_regex",
		},
		{
			name:      "an ancestor satisfies the encrypted_suffix",
			doc:       "sops:\n    encrypted_suffix: _secret\n    mac: ENC[x]\n",
			path:      []string{"api_secret", "token"},
			wantClear: false,
		},
		{
			name:       "an ancestor matches the unencrypted_regex",
			doc:        "sops:\n    unencrypted_regex: '^public_'\n    mac: ENC[x]\n",
			path:       []string{"public_block", "token"},
			wantClear:  true,
			wantReason: "unencrypted_regex",
		},
		{
			name:       "unencrypted_suffix matches",
			doc:        "sops:\n    unencrypted_suffix: _unencrypted\n    mac: ENC[x]\n",
			path:       []string{"foo_unencrypted"},
			wantClear:  true,
			wantReason: "unencrypted_suffix",
		},
		{
			name:      "unencrypted_suffix does not match",
			doc:       "sops:\n    unencrypted_suffix: _unencrypted\n    mac: ENC[x]\n",
			path:      []string{"llm_key_hermes"},
			wantClear: false,
		},
		{
			// sops' own default when a file configures nothing else. Treating
			// "no rule" as "everything is encrypted" would accept a key the
			// real sops would write in the clear.
			name:       "the _unencrypted default applies with no rule configured",
			doc:        "sops:\n    mac: ENC[x]\n    version: 3.13.3\n",
			path:       []string{"token_unencrypted"},
			wantClear:  true,
			wantReason: "unencrypted_suffix",
		},
		{
			name:      "the default admits an ordinary key",
			doc:       "sops:\n    mac: ENC[x]\n",
			path:      []string{"ordinary_key"},
			wantClear: false,
		},
		{
			name:       "encrypted_suffix refuses a non-matching key",
			doc:        "sops:\n    encrypted_suffix: _secret\n    mac: ENC[x]\n",
			path:       []string{"plain_key"},
			wantClear:  true,
			wantReason: "encrypted_suffix",
		},
		{
			name:      "encrypted_suffix admits a matching key",
			doc:       "sops:\n    encrypted_suffix: _secret\n    mac: ENC[x]\n",
			path:      []string{"api_secret"},
			wantClear: false,
		},
		{
			name:       "encrypted_regex refuses a non-matching key",
			doc:        "sops:\n    encrypted_regex: '^(data|token)$'\n    mac: ENC[x]\n",
			path:       []string{"other"},
			wantClear:  true,
			wantReason: "encrypted_regex",
		},
		{
			name:      "encrypted_regex admits a matching key",
			doc:       "sops:\n    encrypted_regex: '^(data|token)$'\n    mac: ENC[x]\n",
			path:      []string{"token"},
			wantClear: false,
		},
		{
			name:       "unencrypted_regex refuses a matching key",
			doc:        "sops:\n    unencrypted_regex: '^public_'\n    mac: ENC[x]\n",
			path:       []string{"public_url"},
			wantClear:  true,
			wantReason: "unencrypted_regex",
		},
		{
			name:      "unencrypted_regex admits a non-matching key",
			doc:       "sops:\n    unencrypted_regex: '^public_'\n    mac: ENC[x]\n",
			path:      []string{"private_token"},
			wantClear: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rules, err := ReadPlaintextRules([]byte(c.doc))
			if err != nil {
				t.Fatalf("ReadPlaintextRules: %v", err)
			}
			isClear, reason := rules.WouldStoreCleartext(c.path)
			if isClear != c.wantClear {
				t.Fatalf("WouldStoreCleartext(%v) = %v (%q), want %v", c.path, isClear, reason, c.wantClear)
			}
			if !isClear {
				return
			}
			if !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason = %q, want it to name %q", reason, c.wantReason)
			}
			// The reason names the RULE, never the key — a key slot holds a
			// pasted secret often enough that this has to hold here too.
			if segmentEchoed(reason, c.path) {
				t.Errorf("reason %q echoed the key", reason)
			}
		})
	}
}

func TestReadPlaintextRules_Refusals(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "invalid yaml",
			doc:  "sops: [unclosed\n",
			want: "does not parse",
		},
		{
			name: "an uncompilable encrypted_regex",
			doc:  "sops:\n    encrypted_regex: '('\n",
			want: "encrypted_regex does not compile",
		},
		{
			name: "an uncompilable unencrypted_regex",
			doc:  "sops:\n    unencrypted_regex: '[z-a]'\n",
			want: "unencrypted_regex does not compile",
		},
		{
			name: "an implausibly long regex",
			doc:  "sops:\n    encrypted_regex: '" + strings.Repeat("a", maxRuleBytes+1) + "'\n",
			want: "implausibly long",
		},
		{
			name: "an implausibly long suffix",
			doc:  "sops:\n    unencrypted_suffix: '" + strings.Repeat("a", maxRuleBytes+1) + "'\n",
			want: "implausibly long",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadPlaintextRules([]byte(c.doc))
			if err == nil {
				t.Fatal("ReadPlaintextRules returned nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.want)
			}
			// The rule is file content, which in a cloned repository is
			// attacker-supplied. The message names the FIELD, not the value.
			if strings.Contains(err.Error(), strings.Repeat("a", 32)) {
				t.Errorf("error %q echoed the rule's value", err.Error())
			}
		})
	}
}
