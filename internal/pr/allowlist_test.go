package pr

// Test plan for allowlist.go
//
// remoteProfile (Classification: deny-by-default security control)
//   [x] Denies every posting/mutation surface (gh pr review/comment/merge,
//       push, commit, WebFetch, Write/Edit)
//   [x] Allows only read-only inspection (Read/Grep/Glob + read-only Bash)
//   [x] Never grants gh pr review/comment/merge under allow

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRemoteProfile(t *testing.T) {
	perms, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatalf("remoteProfile: %v", err)
	}
	s := reviewSettings{Permissions: perms}

	// Deny-by-default posting surfaces must be present.
	mustDeny := []string{
		"Bash(gh pr review:*)", "Bash(gh pr comment:*)", "Bash(gh pr merge:*)",
		"Bash(git push:*)", "Write", "Edit", "WebFetch",
	}
	for _, d := range mustDeny {
		if !contains(s.Permissions.Deny, d) {
			t.Errorf("deny list missing %q", d)
		}
	}

	// Allow list must be strictly read-only — no post/merge/comment.
	for _, a := range s.Permissions.Allow {
		low := strings.ToLower(a)
		for _, banned := range []string{"pr review", "pr comment", "pr merge", "push", "commit", "write", "edit"} {
			if strings.Contains(low, banned) {
				t.Errorf("allow list grants a mutating/posting action: %q", a)
			}
		}
	}
	if len(s.Permissions.Allow) == 0 {
		t.Error("allow list is empty; read-only inspection should be permitted")
	}
	for _, a := range s.Permissions.Allow {
		if strings.HasPrefix(a, "Bash(rg") {
			t.Errorf("written PR-mode allow list grants rg: %q", a)
		}
	}
	for _, want := range []string{
		"Bash(gh pr view 42 --repo github.com/o/r)",
		"Bash(gh pr diff 42 --repo github.com/o/r)",
		"Bash(gh pr checks 42 --repo github.com/o/r)",
	} {
		if !contains(s.Permissions.Allow, want) {
			t.Errorf("allow list missing the generated gh read %q: %v", want, s.Permissions.Allow)
		}
	}
}

// TestDenyPosting_CoversMutatingGhGroups covers the defense-in-depth backstop:
// denyPosting must hard-block the mutating `gh pr` verbs AND every other
// mutating gh command group — so a relaxed permission floor still can't reach
// pr ready/reopen/lock/unlock or workflow/release/secret/variable/ruleset/
// issue/gist/repo/run. None of these may overlap the read-only allow-list
// (that would silently revoke a read).
func TestDenyPosting_CoversMutatingGhGroups(t *testing.T) {
	mustDeny := []string{
		"Bash(gh pr ready:*)",
		"Bash(gh pr reopen:*)",
		"Bash(gh pr lock:*)",
		"Bash(gh pr unlock:*)",
		"Bash(gh workflow:*)",
		"Bash(gh release:*)",
		"Bash(gh secret:*)",
		"Bash(gh variable:*)",
		"Bash(gh ruleset:*)",
		"Bash(gh issue:*)",
		"Bash(gh gist:*)",
		"Bash(gh repo:*)",
		"Bash(gh run:*)",
		"Bash(gh auth:*)",
		"Bash(gh config:*)",
		"Bash(gh label:*)",
		"Bash(gh project:*)",
		"Bash(gh cache:*)",
		"Bash(gh codespace:*)",
		"Bash(gh extension:*)",
		"Bash(gh alias:*)",
	}
	for _, d := range mustDeny {
		if !contains(denyPosting, d) {
			t.Errorf("denyPosting missing mutating gh group %q", d)
		}
		if contains(allowReadOnly, d) {
			t.Errorf("deny entry %q also appears in allowReadOnly — a read would be silently revoked", d)
		}
	}
}

// TestAllowReadOnly_IsExactlyBaseReadOnly covers PR mode's static surface: it
// must equal baseReadOnly, with NO rg and NO gh entry. rg's --pre runs a
// program per searched file, and a quoted spelling (`rg --"pre"=sh …`) slips
// past any deny pattern, so rg cannot be granted in any form (forgectl#673).
// The gh reads are generated per session as exact rules (prGhReadRules); a
// static gh prefix here is the #673 hole coming back.
func TestAllowReadOnly_IsExactlyBaseReadOnly(t *testing.T) {
	if !equalArgs(allowReadOnly, baseReadOnly) {
		t.Errorf("allowReadOnly = %v, want exactly baseReadOnly %v", allowReadOnly, baseReadOnly)
	}
	perms, err := prGhReadRules("github.com", Ref{Owner: "o", Repo: "r", Number: 1})
	if err != nil {
		t.Fatalf("prGhReadRules: %v", err)
	}
	for _, a := range append(append([]string{}, allowReadOnly...), perms...) {
		if strings.HasPrefix(a, "Bash(rg") {
			t.Errorf("PR-mode allow list grants rg (%q); rg --pre is a command-execution primitive", a)
		}
	}
	for _, a := range allowReadOnly {
		if strings.HasPrefix(a, "Bash(gh") {
			t.Errorf("allowReadOnly carries a static gh rule %q; gh reads must be generated exact rules", a)
		}
	}
}

// ccBashRuleMatches models how Claude Code matches one Bash permission rule
// against one (already split) command, per the permissions docs: a rule with
// no `*` matches one exact command; a `*` matches any text, spaces included;
// a trailing `:*` is the same as a trailing ` *`; and a trailing ` *` that is
// the rule's only wildcard also matches the bare command. It exists so the
// injection test below asserts against the rule grammar rather than against
// string equality — and TestPrGhReadRules_RejectInjectedHostSelection proves
// it is not vacuous by checking it DOES admit the injections under the old
// prefix rules.
func ccBashRuleMatches(rule, cmd string) bool {
	inner, ok := strings.CutPrefix(rule, "Bash(")
	if !ok {
		return false
	}
	pattern, ok := strings.CutSuffix(inner, ")")
	if !ok {
		return false
	}
	if p, found := strings.CutSuffix(pattern, ":*"); found {
		pattern = p + " *"
	}
	if strings.Count(pattern, "*") == 1 && strings.HasSuffix(pattern, " *") && cmd == strings.TrimSuffix(pattern, " *") {
		return true
	}
	return globMatch(pattern, cmd)
}

// globMatch is `*`-only glob matching over the whole string.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

func anyRuleMatches(rules []string, cmd string) bool {
	for _, r := range rules {
		if ccBashRuleMatches(r, cmd) {
			return true
		}
	}
	return false
}

// TestPrGhReadRules_RejectInjectedHostSelection is forgectl#673: a
// prompt-injected review agent must not be able to point gh at another host
// (or another repository) through any spelling the allow-list admits. Each
// injected form selects a host by a different route: a leading -R, a second
// --repo after the legitimate one (gh keeps the last), a combined short flag,
// an attached value, and a PR URL operand.
func TestPrGhReadRules_RejectInjectedHostSelection(t *testing.T) {
	ref := Ref{Owner: "base", Repo: "r", Number: 42}
	rules, err := prGhReadRules("ghe.corp.example", ref)
	if err != nil {
		t.Fatalf("prGhReadRules: %v", err)
	}
	injected := []string{
		"gh pr view -R evil.example/o/r 1",
		"gh pr view 42 --repo ghe.corp.example/base/r -R evil.example/o/r",
		"gh pr view 42 --repo ghe.corp.example/base/r --repo evil.example/o/r",
		"gh pr view 42 --repo ghe.corp.example/base/r --comments -Revil.example/o/r",
		"gh pr view 42 --repo=evil.example/o/r",
		"gh pr view https://evil.example/o/r/pull/1",
		"gh pr diff 42 --repo ghe.corp.example/base/r -R evil.example/o/r",
		"gh pr checks 42 --repo ghe.corp.example/base/r --hostname evil.example",
		// Argument-less forms resolve against the workspace clone, which is
		// the head repository and may be a fork, not the PR under review.
		"gh pr view",
		"gh pr view 42",
	}
	for _, cmd := range injected {
		if anyRuleMatches(rules, cmd) {
			t.Errorf("generated rules admit %q: %v", cmd, rules)
		}
	}
	// The permitted reads still work, and they are the ones the prompt names.
	for _, cmd := range prGhReadCommands("ghe.corp.example", ref) {
		if !anyRuleMatches(rules, cmd) {
			t.Errorf("generated rules refuse their own command %q: %v", cmd, rules)
		}
	}
	// Non-vacuity: the pre-#673 prefix rules admit the first injection, so the
	// matcher can tell the two rule shapes apart.
	old := []string{"Bash(gh pr view:*)", "Bash(gh pr diff:*)", "Bash(gh pr checks:*)"}
	if !anyRuleMatches(old, injected[0]) {
		t.Fatalf("matcher does not admit %q under the old prefix rules; the test above proves nothing", injected[0])
	}
	for _, r := range rules {
		if strings.Contains(r, "*") {
			t.Errorf("generated gh rule %q carries a wildcard; it must be an exact command", r)
		}
	}
}

// TestPrGhReadRules_RefusesValuesOutsideTheCharsets: a host, owner, or repo
// carrying a space or `*` would turn an exact rule into a wider pattern, so the
// generator fails closed instead of writing it.
func TestPrGhReadRules_RefusesValuesOutsideTheCharsets(t *testing.T) {
	good := Ref{Owner: "o", Repo: "r", Number: 1}
	cases := map[string]struct {
		host string
		ref  Ref
	}{
		"wildcard host":  {"ghe.*", good},
		"spaced host":    {"ghe corp", good},
		"empty host":     {"", good},
		"wildcard owner": {"github.com", Ref{Owner: "o*", Repo: "r", Number: 1}},
		"spaced repo":    {"github.com", Ref{Owner: "o", Repo: "r *", Number: 1}},
		"zero number":    {"github.com", Ref{Owner: "o", Repo: "r", Number: 0}},
	}
	for name, tc := range cases {
		if rules, err := prGhReadRules(tc.host, tc.ref); err == nil {
			t.Errorf("%s: prGhReadRules accepted it and produced %v", name, rules)
		}
	}
	if _, err := remoteProfile("ghe.*", good); err == nil {
		t.Error("remoteProfile built a permission set for an invalid host")
	}
}

// TestLocalAllowReadOnly_IsExactlyBaseReadOnly covers the other extraction
// consumer: local mode grants no rg (no approval-gate backstop) and no gh
// entries at all, i.e. localAllowReadOnly must equal baseReadOnly exactly
// (allowReadOnly is the same set; PR mode adds only its generated gh reads).
func TestLocalAllowReadOnly_IsExactlyBaseReadOnly(t *testing.T) {
	if len(localAllowReadOnly) != len(baseReadOnly) {
		t.Fatalf("localAllowReadOnly has %d entries, want %d (== baseReadOnly)", len(localAllowReadOnly), len(baseReadOnly))
	}
	for i, want := range baseReadOnly {
		if localAllowReadOnly[i] != want {
			t.Errorf("localAllowReadOnly[%d] = %q, want %q", i, localAllowReadOnly[i], want)
		}
	}
	for _, a := range localAllowReadOnly {
		if strings.Contains(a, "gh") {
			t.Errorf("localAllowReadOnly must grant no gh entries; found %q", a)
		}
		if strings.Contains(a, "rg") {
			t.Errorf("localAllowReadOnly must grant no rg (command-execution primitive); found %q", a)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestDenyLists_CarryNoMultiEdit: Claude Code reports a MultiEdit rule as
// matching no known tool (forgectl#715 item 5).
func TestDenyLists_CarryNoMultiEdit(t *testing.T) {
	remote, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string][]string{
		"remote": remote.Deny,
		"local":  mustLocalProfile(t, "/tmp/forgectl-findings-x").Deny,
	} {
		for _, rule := range list {
			if strings.HasPrefix(rule, "MultiEdit") {
				t.Errorf("%s deny list carries %q, which matches no tool", name, rule)
			}
		}
	}
}

// TestProfiles_DenyReadsOfCredentialStores covers forgectl#715 item 2: both
// modes deny reads of the operator's credential stores, and only a local
// review, which runs no gh, denies gh's own config dir. A PR review's gh reads
// authenticate from it, and a Read deny also reaches the sandbox.
func TestProfiles_DenyReadsOfCredentialStores(t *testing.T) {
	remote, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	local := mustLocalProfile(t, "/tmp/forgectl-findings-x")
	for name, perms := range map[string]permissions{"remote": remote, "local": local} {
		for _, want := range []string{
			"Read(~/.ssh/**)", "Read(~/.aws/**)", "Read(~/.gnupg/**)", "Read(~/.netrc)", "Read(~/.config/sops/**)",
			"Read(~/.claude/.credentials.json)", "Read(~/.codex/auth.json)", "Read(~/.config/hub/**)",
			"Read(~/.cargo/credentials.toml)", "Read(~/.terraform.d/credentials.tfrc.json)", "Read(~/.vault-token)",
			"Read(~/.terraformrc)", "Read(~/terraform.rc)",
			"Read(~/.pgpass)", "Read(~/.password-store/**)", "Read(~/.local/share/keyrings/**)",
			"Read(~/.config/rclone/**)", "Read(~/.m2/settings.xml)", "Read(~/.gem/credentials)", "Read(~/.config/op/**)",
			"Read(//proc/**)",
		} {
			if !contains(perms.Deny, want) {
				t.Errorf("%s deny list missing %q", name, want)
			}
		}
		for _, rule := range perms.Deny {
			if strings.HasPrefix(rule, "Read(") && !strings.HasPrefix(rule, "Read(~/") && !strings.HasPrefix(rule, "Read(//") {
				t.Errorf("%s credential deny %q is not home-rooted", name, rule)
			}
		}
		for _, rule := range perms.Allow {
			for _, p := range reviewCredentialPaths("") {
				if strings.Contains(rule, strings.TrimPrefix(p.path, "~/")) {
					t.Errorf("%s allow rule %q reaches the credential path %s", name, rule, p.path)
				}
			}
		}
	}
	const ghRule = "Read(~/.config/gh/**)"
	if contains(remote.Deny, ghRule) {
		t.Errorf("remote deny list carries %q, which would cut gh off from its stored token", ghRule)
	}
	if !contains(local.Deny, ghRule) {
		t.Errorf("local deny list missing %q", ghRule)
	}
}

// TestReviewCredentialPaths_FollowRelocations: a store an environment
// variable moved is denied where it now lives, absolute paths //-rooted in a
// rule, and gh's relocated config only for a local review. A relative or
// pattern-bearing value is not turned into a rule.
func TestReviewCredentialPaths_FollowRelocations(t *testing.T) {
	orig := credentialEnv
	t.Cleanup(func() { credentialEnv = orig })
	env := map[string]string{ //nolint:gosec // G101: credential-path variable names and fake paths, no secret
		"XDG_CONFIG_HOME":             "/srv/xdg",
		"GH_CONFIG_DIR":               "/srv/gh",
		"CLOUDSDK_CONFIG":             "/srv/gcloud",
		"AWS_SHARED_CREDENTIALS_FILE": "/srv/aws/credentials",
		"CLAUDE_CONFIG_DIR":           "relative/claude",
		"CODEX_HOME":                  "/srv/co*dex",
		"GNUPGHOME":                   "/srv/link/../gnupg",
	}
	credentialEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	remote, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	local := mustLocalProfile(t, "/tmp/forgectl-findings-x")
	for name, perms := range map[string]permissions{"remote": remote, "local": local} {
		for _, want := range []string{
			"Read(//srv/xdg/gcloud/**)", "Read(//srv/xdg/sops/**)", "Read(//srv/xdg/git/credentials)",
			"Read(//srv/gcloud/**)", "Read(//srv/aws/credentials)",
		} {
			if !contains(perms.Deny, want) {
				t.Errorf("%s deny list missing the relocated %q", name, want)
			}
		}
		for _, rule := range perms.Deny {
			if strings.Contains(rule, "relative/claude") || strings.Contains(rule, "co*dex") || strings.Contains(rule, "/srv/gnupg") || strings.Contains(rule, "/srv/link") {
				t.Errorf("%s deny list carries an unusable relocation: %q", name, rule)
			}
		}
	}
	for _, rule := range []string{"Read(//srv/gh/**)", "Read(//srv/xdg/gh/**)"} {
		if contains(remote.Deny, rule) {
			t.Errorf("remote deny list carries %q, which would cut gh off from its stored token", rule)
		}
		if !contains(local.Deny, rule) {
			t.Errorf("local deny list missing %q", rule)
		}
	}
	sb, err := reviewSandbox(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sb.Filesystem.DenyRead, "/srv/gh") || !slices.Contains(sb.Filesystem.DenyRead, "/srv/aws/credentials") {
		t.Errorf("sandbox denyRead misses the relocated stores: %v", sb.Filesystem.DenyRead)
	}
}

func TestReviewCredentialPaths_FollowTheWiderRelocations(t *testing.T) {
	orig := credentialEnv
	t.Cleanup(func() { credentialEnv = orig })
	sep := string(filepath.ListSeparator)
	env := map[string]string{ //nolint:gosec // G101: credential-path variable names and fake paths, no secret
		"KUBECONFIG":            "/k/one" + sep + "/k/two",
		"AWS_CONFIG_FILE":       "/a/config",
		"GNUPGHOME":             "/g",
		"DOCKER_CONFIG":         "/d",
		"AZURE_CONFIG_DIR":      "/az",
		"PASSWORD_STORE_DIR":    "/ps",
		"PGPASSFILE":            "/pg/pass",
		"NPM_CONFIG_USERCONFIG": "/n/npmrc",
		"npm_config_userconfig": "/n/lower-npmrc",
		"CARGO_HOME":            "/c",
		"TF_CLI_CONFIG_FILE":    "/tf/rc",
		"SOPS_AGE_KEY_FILE":     "/s/keys.txt",
		"XDG_DATA_HOME":         "/xd",
	}
	credentialEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	remote, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Read(//k/one)", "Read(//k/two)", "Read(//a/config)", "Read(//g/**)", "Read(//d/**)", "Read(//az/**)",
		"Read(//ps/**)", "Read(//pg/pass)", "Read(//n/npmrc)", "Read(//n/lower-npmrc)", "Read(//c/credentials)", "Read(//c/credentials.toml)",
		"Read(//tf/rc)", "Read(//s/keys.txt)", "Read(//xd/keyrings/**)",
		"Read(~/Library/Application Support/sops/age/keys.txt)",
	} {
		if !contains(remote.Deny, want) {
			t.Errorf("deny list missing %q", want)
		}
	}
}
