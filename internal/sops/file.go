package sops

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxRuleBytes bounds a suffix or regex read out of a file's own metadata.
// The value is file content, so it is attacker-influenced in a cloned
// repository; a rule longer than this is not a rule anyone wrote.
const maxRuleBytes = 1024

// metadata is the plaintext half of a SOPS document. The sops: block is never
// encrypted — that is what lets every check in this file run without a key,
// a subprocess, or a decryption.
type metadata struct {
	Sops struct {
		UnencryptedSuffix string `yaml:"unencrypted_suffix"`
		EncryptedSuffix   string `yaml:"encrypted_suffix"`
		EncryptedRegex    string `yaml:"encrypted_regex"`
		UnencryptedRegex  string `yaml:"unencrypted_regex"`
		Mac               string `yaml:"mac"`
		Version           string `yaml:"version"`
	} `yaml:"sops"`
}

// IsSOPSFile reports whether data parses as YAML and carries a top-level
// `sops:` mapping.
//
// It is a NARROWING check, never the whole gate: the caller also requires the
// filename to match the sops shapes below, and both must hold. A name test
// alone would accept a plain YAML file someone called secrets.sops.yaml; a
// content test alone would accept any encrypted document anywhere in the
// repository, which is exactly the widening the env-file allowlist exists to
// prevent.
func IsSOPSFile(data []byte) bool {
	var probe map[string]yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	node, ok := probe["sops"]
	return ok && node.Kind == yaml.MappingNode
}

// sopsNamePatterns is the filename allowlist for the --sops route.
//
// It is a separate list from internal/env's IsEnvFileName and deliberately so:
// a SOPS document is not an env file, and admitting one through the env
// allowlist would have meant widening a rule that exists to stop `env set`
// becoming a writer of arbitrary repository files. The reasoning carries over
// unchanged — repo-containment alone is not a bound worth having, because
// .git/config is inside the repository too.
//
// There is no --any-file escape hatch on this route. A SOPS file under some
// other name is unreachable, which is a deliberate refusal rather than a gap:
// the alternative is a confirmation prompt, and the confirmation path is the
// one that carried a time-of-check/time-of-use defect. Refusing is honest and
// costs a rename.
var sopsNamePatterns = []string{
	"*.sops.yaml",
	"*.sops.yml",
	"*.enc.yaml",
	"*.enc.yml",
	"secrets.yaml",
	"secrets.yml",
	"secrets.*.yaml",
	"secrets.*.yml",
}

// NameShapes renders the allowlist for an error message, so the refusal tells
// the operator what would have been accepted instead of just saying no.
func NameShapes() string { return strings.Join(sopsNamePatterns, ", ") }

// IsSOPSFileName reports whether base — a basename, not a path — matches one
// of the allowed shapes. Matching is byte-exact, which on a case-insensitive
// filesystem means `SECRETS.YAML` is refused. That fails toward refusing a
// legitimate file rather than admitting an unintended one, which is the
// direction this check must err in.
func IsSOPSFileName(base string) bool {
	for _, pattern := range sopsNamePatterns {
		// filepath.Match's only error is a malformed pattern, and every
		// pattern here is a literal in this file.
		if ok, _ := filepath.Match(pattern, base); ok {
			return true
		}
	}
	return false
}

// PlaintextRules decides whether a given key would be stored in CLEARTEXT by
// this file's own encryption rules.
type PlaintextRules struct {
	unencryptedSuffix string
	encryptedSuffix   string
	encryptedRegex    *regexp.Regexp
	unencryptedRegex  *regexp.Regexp
}

// ReadPlaintextRules extracts the encryption rules from a document's sops
// metadata.
//
// # Why this check exists at all
//
// sops applies these rules per key, and a key the rules exclude is written to
// the file IN THE CLEAR next to its encrypted siblings. Measured live on
// 3.13.3: with `unencrypted_suffix: _unencrypted` in force, a key named
// `foo_unencrypted` sat in plaintext while its neighbour read
// `ENC[AES256_GCM,...]`. So without this check, `env set` would cheerfully
// accept a secret and store it unencrypted while reporting success — and the
// decrypt-round-trip verification would pass, because a cleartext value
// round-trips perfectly.
//
// Every encrypted file in the estate this feature targets carries
// `unencrypted_suffix: _unencrypted`, so this is live on the exact files the
// feature is for, not a hypothetical.
func ReadPlaintextRules(data []byte) (PlaintextRules, error) {
	var meta metadata
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return PlaintextRules{}, errors.New("file does not parse as YAML")
	}

	rules := PlaintextRules{
		unencryptedSuffix: meta.Sops.UnencryptedSuffix,
		encryptedSuffix:   meta.Sops.EncryptedSuffix,
	}

	// sops' own default when a file configures no other rule. Applying it
	// here rather than treating "no rule" as "everything is encrypted" is
	// the fail-closed direction: a file that relies on the default would
	// otherwise have its _unencrypted keys accepted.
	if rules.unencryptedSuffix == "" && rules.encryptedSuffix == "" &&
		meta.Sops.EncryptedRegex == "" && meta.Sops.UnencryptedRegex == "" {
		rules.unencryptedSuffix = "_unencrypted"
	}

	var err error
	if rules.encryptedRegex, err = compileRule(meta.Sops.EncryptedRegex, "encrypted_regex"); err != nil {
		return PlaintextRules{}, err
	}
	if rules.unencryptedRegex, err = compileRule(meta.Sops.UnencryptedRegex, "unencrypted_regex"); err != nil {
		return PlaintextRules{}, err
	}
	if len(rules.unencryptedSuffix) > maxRuleBytes || len(rules.encryptedSuffix) > maxRuleBytes {
		return PlaintextRules{}, errors.New("the file's encryption-suffix rule is implausibly long")
	}

	return rules, nil
}

// compileRule compiles a regex read from file metadata.
//
// Go's regexp is RE2, which is linear-time and has no catastrophic
// backtracking, so an adversarial pattern from a cloned repository cannot
// turn this into a denial of service. The length bound is about plausibility
// rather than safety.
func compileRule(pattern, field string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	if len(pattern) > maxRuleBytes {
		return nil, fmt.Errorf("the file's %s is implausibly long", field)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		// Names the field, never the pattern: the pattern is file content and
		// the field is what an operator needs in order to go fix it.
		return nil, fmt.Errorf("the file's %s does not compile", field)
	}
	return re, nil
}

// WouldStoreCleartext reports whether key falls outside this file's encryption
// rules, and why.
//
// # Why it takes the whole path and not just the leaf
//
// sops applies these rules to a key AND ITS WHOLE SUBTREE, so an ancestor
// decides the outcome for everything beneath it. Measured on 3.13.3:
//
//	unencrypted_suffix: _unencrypted  →  notes_unencrypted.token   CLEARTEXT
//	encrypted_regex: ^app$            →  app.token, app.inner.deep  both ENCRYPTED
//
// Testing the leaf alone gets both cases wrong, in opposite directions. A path
// whose PARENT carries the suffix passes the check and the secret lands in
// plaintext, reported as success — the exact failure this function exists to
// prevent. And an ancestor-scoped encrypted_regex falsely refuses every key
// beneath the block it matches, which makes the feature unusable on such a
// file.
//
// The parameter is []string rather than a string so the leaf-only call cannot
// be written again by accident. That is the real fix; the walk is its
// consequence.
//
// Every reason names the RULE and the field it came from — never a segment,
// which may itself be a secret pasted into the wrong slot.
func (r PlaintextRules) WouldStoreCleartext(path []string) (bool, string) {
	// An unencrypted rule matching ANY segment wins: everything beneath that
	// segment is excluded from encryption, and this key is beneath it.
	for _, segment := range path {
		if r.unencryptedSuffix != "" && strings.HasSuffix(segment, r.unencryptedSuffix) {
			return true, fmt.Sprintf("a key on this path ends with the file's unencrypted_suffix (%q)", r.unencryptedSuffix)
		}
		if r.unencryptedRegex != nil && r.unencryptedRegex.MatchString(segment) {
			return true, "a key on this path matches the file's unencrypted_regex"
		}
	}

	// An encrypted rule is an allowlist, and a match at an ANCESTOR covers the
	// subtree — so it is satisfied when any segment matches, and violated only
	// when none does.
	if r.encryptedSuffix != "" && !anySegment(path, func(s string) bool {
		return strings.HasSuffix(s, r.encryptedSuffix)
	}) {
		return true, fmt.Sprintf("the file encrypts only keys ending with its encrypted_suffix (%q), and no key on this path does", r.encryptedSuffix)
	}
	if r.encryptedRegex != nil && !anySegment(path, r.encryptedRegex.MatchString) {
		return true, "no key on this path matches the file's encrypted_regex"
	}

	return false, ""
}

// anySegment reports whether pred holds for at least one segment.
func anySegment(path []string, pred func(string) bool) bool {
	for _, segment := range path {
		if pred(segment) {
			return true
		}
	}
	return false
}
