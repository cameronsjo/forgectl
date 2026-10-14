package pr

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// credentialPath is one operator credential location: home-relative in the
// `~/` spelling both Claude Code's permission rules and its sandbox paths
// resolve against the home directory, or absolute when an environment
// variable relocated it. dir says whether it is a directory, whose whole
// subtree is denied, or a single file.
type credentialPath struct {
	path string
	dir  bool
}

// credentialPaths are the credential stores a reviewer has no reason to read
// (forgectl#715 item 2). The sandbox lets a command read anything the operator
// can, and whatever it reads goes to the model provider as tool output, so a
// prompt-injected review could otherwise lift a key. It is a list of the
// common stores, not a proof that nothing else on the host is secret.
//
// Claude Code's and Codex's own logins are on it: forgectl drives both, and
// neither reviewer needs to read them. Claude Code reads its login in its own
// process, which no permission rule or sandbox path touches.
//
// gh's own config dir is not here: a PR review's gh reads authenticate from
// its hosts.yml, and a Read deny rule is also applied to the sandbox's reads
// (measured on Claude Code 2.1.296: `Read(~/.config/gh/**)` alone made a
// sandboxed `gh config get` fall back to its defaults). ghConfigPath adds it
// for a local review, which runs no gh at all.
var credentialPaths = []credentialPath{
	{"~/.ssh", true},
	{"~/.gnupg", true},
	{"~/.aws", true},
	{"~/.azure", true},
	{"~/.config/gcloud", true},
	{"~/.kube", true},
	{"~/.docker", true},
	{"~/.config/sops", true},
	{"~/.config/hub", true},
	{"~/.config/rclone", true},
	{"~/.config/op", true},
	{"~/.password-store", true},
	{"~/.local/share/keyrings", true},
	{"~/.netrc", false},
	{"~/.git-credentials", false},
	{"~/.config/git/credentials", false},
	{"~/.npmrc", false},
	{"~/.pypirc", false},
	{"~/.pgpass", false},
	{"~/.vault-token", false},
	{"~/.cargo/credentials", false},
	{"~/.cargo/credentials.toml", false},
	{"~/.terraform.d/credentials.tfrc.json", false},
	{"~/.m2/settings.xml", false},
	{"~/.gem/credentials", false},
	{"~/.claude/.credentials.json", false},
	{"~/.codex/auth.json", false},
	{"~/Library/Application Support/sops/age/keys.txt", false},
}

// ghConfigPath is gh's config dir, holding its stored tokens. Only a local
// review denies it; see credentialPaths.
var ghConfigPath = credentialPath{"~/.config/gh", true}

// relocation is one environment variable that moves a credential store, and
// the paths under its value a review denies: suffix "" is the value itself.
// list marks a PATH-style list (KUBECONFIG), each element of which is
// denied. localOnly marks gh's config, which only a local review denies (see
// credentialPaths).
type relocation struct {
	variable  string
	targets   []credentialPath
	list      bool
	localOnly bool
}

// relocations are the variables reviewCredentialPaths follows. Every one of
// them is also pinned into the review window (reviewRelocationEnv), so the
// value the denies were built from is the value the reviewer's process tree
// sees.
var relocations = []relocation{
	{variable: "XDG_CONFIG_HOME", targets: []credentialPath{
		{"gcloud", true}, {"sops", true}, {"hub", true}, {"rclone", true}, {"op", true}, {"git/credentials", false},
	}},
	{variable: "XDG_CONFIG_HOME", targets: []credentialPath{{"gh", true}}, localOnly: true},
	{variable: "XDG_DATA_HOME", targets: []credentialPath{{"keyrings", true}}},
	{variable: "GH_CONFIG_DIR", targets: []credentialPath{{"", true}}, localOnly: true},
	{variable: "CLOUDSDK_CONFIG", targets: []credentialPath{{"", true}}},
	{variable: "AWS_SHARED_CREDENTIALS_FILE", targets: []credentialPath{{"", false}}},
	{variable: "AWS_CONFIG_FILE", targets: []credentialPath{{"", false}}},
	{variable: "AZURE_CONFIG_DIR", targets: []credentialPath{{"", true}}},
	{variable: "KUBECONFIG", targets: []credentialPath{{"", false}}, list: true},
	{variable: "GNUPGHOME", targets: []credentialPath{{"", true}}},
	{variable: "DOCKER_CONFIG", targets: []credentialPath{{"", true}}},
	{variable: "PASSWORD_STORE_DIR", targets: []credentialPath{{"", true}}},
	{variable: "PGPASSFILE", targets: []credentialPath{{"", false}}},
	{variable: "NPM_CONFIG_USERCONFIG", targets: []credentialPath{{"", false}}},
	{variable: "CARGO_HOME", targets: []credentialPath{{"credentials", false}, {"credentials.toml", false}}},
	{variable: "TF_CLI_CONFIG_FILE", targets: []credentialPath{{"", false}}},
	{variable: "SOPS_AGE_KEY_FILE", targets: []credentialPath{{"", false}}},
	{variable: "CLAUDE_CONFIG_DIR", targets: []credentialPath{{".credentials.json", false}}},
	{variable: "CODEX_HOME", targets: []credentialPath{{"auth.json", false}}},
}

// credentialEnv is how the relocation variables are read: forgectl's own
// environment. reviewRelocationEnv pins the same values into the review
// window, which would otherwise inherit the tmux server's, so the two cannot
// disagree. A seam for tests.
var credentialEnv = os.LookupEnv

// permissionPathMeta are the characters a permission rule or sandbox path
// reads as a pattern, or that would end a `Read(...)` rule early. A path
// holding one is not denied as spelled, so it is left out rather than
// written as a rule that means something else.
const permissionPathMeta = "*?[]{}()!\\"

// relocationValues are the usable values of variable in forgectl's
// environment: one, or a list's elements, each absolute and free of
// pattern characters. ok is false when the variable is unset or empty, or
// when any value is unusable; the window then gets it unset (see
// reviewRelocationEnv), so nothing reads a store the denies do not name.
func relocationValues(r relocation) (values []string, ok bool) {
	v, set := credentialEnv(r.variable)
	if !set || v == "" {
		return nil, false
	}
	parts := []string{v}
	if r.list {
		parts = filepath.SplitList(v)
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		if !filepath.IsAbs(part) || strings.ContainsAny(part, permissionPathMeta) {
			slog.Warn("Unsetting a credential-store variable in the review window: its path is not absolute or holds a pattern character, so it cannot be denied.",
				"variable", r.variable)
			return nil, false
		}
		values = append(values, filepath.Clean(part))
	}
	return values, len(values) > 0
}

// reviewCredentialPaths are the credential paths a review denies reads of:
// credentialPaths, the same stores where an environment variable moved
// them, and gh's config dir for a local review (ghHost "").
func reviewCredentialPaths(ghHost string) []credentialPath {
	local := ghHost == ""
	paths := append([]credentialPath{}, credentialPaths...)
	if local {
		paths = append(paths, ghConfigPath)
	}
	for _, r := range relocations {
		if r.localOnly && !local {
			continue
		}
		values, ok := relocationValues(r)
		if !ok {
			continue
		}
		for _, v := range values {
			for _, t := range r.targets {
				p := v
				if t.path != "" {
					p = filepath.Join(v, t.path)
				}
				paths = append(paths, credentialPath{p, t.dir})
			}
		}
	}
	return paths
}

// reviewRelocationEnv is how the review window carries every relocation
// variable: set holds `KEY=VALUE` for each one forgectl has a usable value
// for, and unset names the rest. A tmux window otherwise inherits the tmux
// server's environment, which can name a store forgectl never saw: with
// CODEX_HOME=/data/codex in the server and unset in forgectl, only
// ~/.codex/auth.json would be denied and /data/codex/auth.json read freely.
//
// unset is a real unset (launchInline runs claude under `env -u`), not an
// empty value: an empty CLAUDE_CONFIG_DIR makes Claude Code use the working
// directory, the PR head, as its config dir (measured on 2.1.296).
func reviewRelocationEnv() (set, unset []string) {
	seen := map[string]bool{}
	for _, r := range relocations {
		if seen[r.variable] {
			continue
		}
		seen[r.variable] = true
		v, isSet := credentialEnv(r.variable)
		if _, ok := relocationValues(r); ok && isSet {
			set = append(set, r.variable+"="+v)
		} else {
			unset = append(unset, r.variable)
		}
	}
	return set, unset
}

// rulePath is p as a permission rule spells it: `~/` paths as given, and an
// absolute path `//`-rooted, since a rule path with one leading `/` is read
// against the settings file.
func (p credentialPath) rulePath() string {
	if strings.HasPrefix(p.path, "/") {
		return "/" + p.path
	}
	return p.path
}

// credentialReadDeny renders paths as Read deny rules. Claude Code applies a
// Read rule to the Read, Grep and Glob tools and to the paths an allowed Bash
// reader such as `cat` or `ls` names; measured on 2.1.296, `cat` and `ls` of a
// denied path were refused. The sandbox's denyRead (reviewSandbox) is the
// OS-enforced half, for a read no command text shows.
//
// `Read(//proc/**)` rides along. With CLAUDE_CODE_SUBPROCESS_ENV_SCRUB set
// (pinReviewClaudeEnv), Claude Code itself refuses a command naming
// /proc/*/environ; this extends the refusal to the Read tool and to every
// other per-process file. It is not mirrored into the sandbox: a denyRead of
// /proc/self/environ did not stop a sandboxed read of it on 2.1.296, because
// the sandbox mounts its own /proc.
func credentialReadDeny(paths []credentialPath) []string {
	rules := make([]string, 0, len(paths)+1)
	for _, p := range paths {
		if p.dir {
			rules = append(rules, "Read("+p.rulePath()+"/**)")
		} else {
			rules = append(rules, "Read("+p.rulePath()+")")
		}
	}
	return append(rules, "Read(//proc/**)")
}

// credentialDenyRead renders paths as the sandbox's denyRead entries. A
// directory entry covers its subtree. Measured on 2.1.296, the `~/x`, `/x`
// and `//x` spellings of one directory each made a sandboxed read in it fail.
func credentialDenyRead(paths []credentialPath) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.path)
	}
	return out
}
