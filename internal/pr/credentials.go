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
}

// ghConfigPath is gh's config dir, holding its stored tokens. Only a local
// review denies it; see credentialPaths.
var ghConfigPath = credentialPath{"~/.config/gh", true}

// xdgConfigCredentials are the credentialPaths entries under ~/.config, as
// paths relative to it, so a relocated XDG_CONFIG_HOME is followed.
var xdgConfigCredentials = []credentialPath{
	{"gcloud", true},
	{"sops", true},
	{"hub", true},
	{"rclone", true},
	{"op", true},
	{"git/credentials", false},
}

// credentialEnv is how the relocation variables are read: forgectl's own
// environment, which the review window's is resolved from. A seam for tests.
var credentialEnv = os.LookupEnv

// permissionPathMeta are the characters a permission rule or sandbox path
// reads as a pattern, or that would end a `Read(...)` rule early. A path
// holding one is not denied as spelled, so it is left out rather than
// written as a rule that means something else.
const permissionPathMeta = "*?[]{}()!\\"

// reviewCredentialPaths are the credential paths a review denies reads of:
// credentialPaths, the same stores where an environment variable moved
// them, and gh's config dir for a local review (ghHost "").
func reviewCredentialPaths(ghHost string) []credentialPath {
	local := ghHost == ""
	paths := append([]credentialPath{}, credentialPaths...)
	if local {
		paths = append(paths, ghConfigPath)
	}
	add := func(variable, suffix string, dir bool) {
		v, ok := credentialEnv(variable)
		if !ok || v == "" {
			return
		}
		if !filepath.IsAbs(v) || strings.ContainsAny(v, permissionPathMeta) {
			slog.Warn("Not denying the reviewer reads of a relocated credential store: the path is not absolute or holds a pattern character.",
				"variable", variable)
			return
		}
		p := filepath.Clean(v)
		if suffix != "" {
			p = filepath.Join(p, suffix)
		}
		paths = append(paths, credentialPath{p, dir})
	}
	for _, c := range xdgConfigCredentials {
		add("XDG_CONFIG_HOME", c.path, c.dir)
	}
	if local {
		add("XDG_CONFIG_HOME", "gh", true)
		add("GH_CONFIG_DIR", "", true)
	}
	add("CLOUDSDK_CONFIG", "", true)
	add("AWS_SHARED_CREDENTIALS_FILE", "", false)
	add("CLAUDE_CONFIG_DIR", ".credentials.json", false)
	add("CODEX_HOME", "auth.json", false)
	return paths
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
