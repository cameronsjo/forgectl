package sops

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cameronsjo/forgectl/internal/yamlsafe"
)

// maxRuleBytes bounds a suffix or regex read out of a file's own metadata.
// The value is file content, so it is attacker-influenced in a cloned
// repository; a rule longer than this is not a rule anyone wrote.
const maxRuleBytes = 1024

// MaxDocumentBytes is the largest SOPS document forgectl reads (#959). sops
// sets no limit of its own, and real files hold certificate bundles,
// kubeconfigs and whole Helm values files. Encryption adds about 1.4x plus
// about 80 bytes per value, so 4 MiB holds about 2.8 MiB of plaintext, far
// beyond any file in the estate. The cap bounds the parse, which is linear
// (0.17 s for a sops-shaped 4 MiB file, 2 s for the worst shape). It is not
// what keeps a hostile document cheap: the readers below never decode the
// whole document into a map, which goes quadratic far inside any cap a real
// file needs (see internal/yamlsafe).
const MaxDocumentBytes = 4 << 20

// MaxRewrittenBytes bounds a document forgectl has edited: one that passed
// MaxDocumentBytes before the edit, plus one value of at most maxValueBytes
// once sops encrypted it. The 1 MiB of headroom keeps a write that was
// accepted from being refused after sops ran.
const MaxRewrittenBytes = MaxDocumentBytes + 1<<20

// errTooLarge is the refusal for a document over MaxDocumentBytes.
var errTooLarge = fmt.Errorf("the file is larger than %d MiB, the limit for a SOPS document", MaxDocumentBytes>>20)

// CheckSize refuses a document over MaxDocumentBytes, so a caller can say
// that, not "not a SOPS document", before IsSOPSFile.
func CheckSize(data []byte) error {
	if len(data) > MaxDocumentBytes {
		return errTooLarge
	}
	return nil
}

// metadata is the plaintext half of a SOPS document. The sops: block is never
// encrypted — that is what lets every check in this file run without a key,
// a subprocess, or a decryption.
type metadata struct {
	Sops sopsBlock `yaml:"sops"`
}

// sopsBlock is the sops: block's fields that the checks here read.
type sopsBlock struct {
	UnencryptedSuffix string `yaml:"unencrypted_suffix"`
	EncryptedSuffix   string `yaml:"encrypted_suffix"`
	EncryptedRegex    string `yaml:"encrypted_regex"`
	UnencryptedRegex  string `yaml:"unencrypted_regex"`
	Mac               string `yaml:"mac"`
	Version           string `yaml:"version"`
}

// sopsBlockKeys are sopsBlock's yaml names, the keys keepKeys keeps.
var sopsBlockKeys = []string{"unencrypted_suffix", "encrypted_suffix", "encrypted_regex", "unencrypted_regex", "mac", "version"}

// parseTop parses data, at most MaxDocumentBytes, into a document whose
// top-level mapping keeps only the `sops` pair (see keepKeys). A document
// whose top level is not a mapping comes back whole: yaml.v3 refuses it, or
// decodes it to nothing, without walking its content.
func parseTop(data []byte) (*yaml.Node, error) {
	if err := CheckSize(data); err != nil {
		return nil, err
	}
	doc, err := yamlsafe.Parse(data, MaxDocumentBytes)
	if err != nil {
		return nil, errors.New("file does not parse as YAML")
	}
	root := yamlsafe.Root(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return doc, nil
	}
	kept, err := keepKeys(root, "sops")
	if err != nil {
		return nil, err
	}
	pruned := *doc
	pruned.Content = []*yaml.Node{kept}
	return &pruned, nil
}

// keepKeys returns a copy of mapping m that holds only the pairs whose key
// decodes to one of names, after checking m's keys as yaml.v3's map and
// struct decode checks them (yamlsafe.CheckKeys), plus a refusal of merge
// keys. Decoding the copy is then what decoding m was, minus the keys
// nobody reads, and costs nothing for those: yaml.v3 compares every key of a
// mapping with every later key, so a document with many keys made the old
// whole-document decode quadratic (#959).
//
// A key is matched by its decoded value, as the struct decode matches it,
// so an alias key or a tagged one is kept when it names a field. A merge
// key is refused rather than applied, which fails closed: a rule merged into
// the sops: block from elsewhere would otherwise go unread.
func keepKeys(m *yaml.Node, names ...string) (*yaml.Node, error) {
	if err := yamlsafe.CheckKeys(m); err != nil {
		return nil, fmt.Errorf("the file's YAML is refused: %w", err)
	}
	kept := *m
	kept.Content = nil
	for i := 0; i+1 < len(m.Content); i += 2 {
		var name string
		if err := m.Content[i].Decode(&name); err != nil {
			return nil, errors.New("file does not parse as YAML")
		}
		if slices.Contains(names, name) {
			kept.Content = append(kept.Content, m.Content[i], m.Content[i+1])
		}
	}
	return &kept, nil
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
//
// A document over MaxDocumentBytes, or one with a merge key at its top level,
// is not one (see CheckSize and keepKeys).
func IsSOPSFile(data []byte) bool {
	doc, err := parseTop(data)
	if err != nil {
		return false
	}
	var probe map[string]yaml.Node
	if err := doc.Decode(&probe); err != nil {
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
//
// The document is read as a node, and only the sops: pair and that block's
// own keys are decoded (keepKeys), so the decode costs the same however
// many keys the rest of the file has. A merge key in either mapping is
// refused.
func ReadPlaintextRules(data []byte) (PlaintextRules, error) {
	meta, err := readMetadata(data)
	if err != nil {
		return PlaintextRules{}, err
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

// readMetadata decodes the sops: block's rule fields as a decode of the
// whole document into metadata would, but over the pruned node: the top
// level keeps only `sops`, and the block keeps only sopsBlockKeys.
func readMetadata(data []byte) (metadata, error) {
	doc, err := parseTop(data)
	if err != nil {
		return metadata{}, err
	}
	// The block is taken as a node first, so it can be pruned before its
	// fields are decoded.
	var top struct {
		Sops yaml.Node `yaml:"sops"`
	}
	if err := doc.Decode(&top); err != nil {
		return metadata{}, errors.New("file does not parse as YAML")
	}
	block := &top.Sops
	if block.Kind == 0 {
		return metadata{}, nil
	}
	// A struct field decoded from an alias reads the anchored node. Prune
	// that node; its own aliases still resolve in the decode below.
	if block.Kind == yaml.AliasNode && block.Alias != nil {
		block = block.Alias
	}
	if block.Kind == yaml.MappingNode {
		if block, err = keepKeys(block, sopsBlockKeys...); err != nil {
			return metadata{}, err
		}
	}
	var meta metadata
	if err := block.Decode(&meta.Sops); err != nil {
		return metadata{}, errors.New("file does not parse as YAML")
	}
	return meta, nil
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
