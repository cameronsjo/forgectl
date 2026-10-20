package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func writeClaudeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClaudeFolderTrust pins the inheritance reading: the physical folder or
// any parent of it trusted counts, the spelling given only as an exact key,
// and anything the check cannot read refuses nothing.
func TestClaudeFolderTrust(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "projects", "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	untrusted := filepath.Join(base, "untrusted")
	if err := os.MkdirAll(untrusted, 0o750); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(repo, "escape")
	if err := os.Symlink(untrusted, escape); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	conf := func(body string) string { return writeClaudeConfig(t, cfgDir, body) }
	q := func(p string) string { return `"` + filepath.ToSlash(p) + `"` }

	cases := []struct {
		name, body, dir string
		want            claudeTrustState
	}{
		{"the folder itself", `{"projects":{` + q(repo) + `:{"hasTrustDialogAccepted":true}}}`, repo, trustTrusted},
		{"a trusted parent", `{"projects":{` + q(filepath.Join(base, "projects")) + `:{"hasTrustDialogAccepted":true}}}`, filepath.Join(repo, "sub"), trustTrusted},
		{"through a symlink", `{"projects":{` + q(repo) + `:{"hasTrustDialogAccepted":true}}}`, link, trustTrusted},
		{"a trusted parent of a link out of it", `{"projects":{` + q(repo) + `:{"hasTrustDialogAccepted":true}}}`, escape, trustUntrusted},
		{"a link itself trusted by its spelling", `{"projects":{` + q(escape) + `:{"hasTrustDialogAccepted":true}}}`, escape, trustTrusted},
		{"an entry not accepted", `{"projects":{` + q(repo) + `:{"hasTrustDialogAccepted":false}}}`, repo, trustUntrusted},
		{"no entry", `{"projects":{"/elsewhere":{"hasTrustDialogAccepted":true}}}`, repo, trustUntrusted},
		{"no projects", `{}`, repo, trustUntrusted},
		{"an unparsable file", `{`, repo, trustUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claudeFolderTrust(conf(c.body), c.dir); got != c.want {
				t.Errorf("trust = %v, want %v", got, c.want)
			}
		})
	}
	if got := claudeFolderTrust(filepath.Join(t.TempDir(), "missing.json"), repo); got != trustUnknown {
		t.Errorf("a missing file = %v, want unknown", got)
	}
	if got := claudeFolderTrust("", repo); got != trustUnknown {
		t.Errorf("no file = %v, want unknown", got)
	}
}

// TestClaudeGlobalConfigFile follows Claude Code's own lookup: the config
// dir's .claude.json, ~/.claude.json for the default account, and a legacy
// .config.json in the config home over either.
func TestClaudeGlobalConfigFile(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	if got := claudeGlobalConfigFile("", home); got != filepath.Join(home, ".claude.json") {
		t.Errorf("default account: %s", got)
	}
	if got := claudeGlobalConfigFile(dir, home); got != filepath.Join(dir, ".claude.json") {
		t.Errorf("config dir account: %s", got)
	}
	if got := claudeGlobalConfigFile("", ""); got != "" {
		t.Errorf("no home: %q", got)
	}
	legacy := filepath.Join(dir, ".config.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeGlobalConfigFile(dir, home); got != legacy {
		t.Errorf("legacy file: %s", got)
	}
}

// TestCheckClaudeFolderTrust_Message names the one command that fixes it,
// with the account's config dir when it is not the default.
func TestCheckClaudeFolderTrust_Message(t *testing.T) {
	cfgDir := t.TempDir()
	writeClaudeConfig(t, cfgDir, `{"projects":{}}`)
	repo := t.TempDir()
	err := checkClaudeFolderTrust(repo, cfgDir)
	if !errors.Is(err, errUntrustedFolder) {
		t.Fatalf("err = %v, want errUntrustedFolder", err)
	}
	if want := "cd " + repo + " && CLAUDE_CONFIG_DIR=" + cfgDir + " claude"; !strings.Contains(err.Error(), want) {
		t.Errorf("message %q lacks %q", err, want)
	}
}

// TestCheckWorkerFolderTrust checks the repository root for the account the
// worker's own build gives it: --profile's config dir, then the profile's env,
// then the injected value, then the inherited CLAUDE_CONFIG_DIR. A worker
// that is not claude is not checked. Each case also builds the worker
// invocation itself and asserts the account checked is the one it carries.
func TestCheckWorkerFolderTrust(t *testing.T) {
	top := t.TempDir()
	trusted, untrusted, inherited := t.TempDir(), t.TempDir(), t.TempDir()
	writeClaudeConfig(t, trusted, `{"projects":{"`+filepath.ToSlash(top)+`":{"hasTrustDialogAccepted":true}}}`)
	writeClaudeConfig(t, untrusted, `{"projects":{}}`)
	writeClaudeConfig(t, inherited, `{"projects":{}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", inherited)

	bin := stubHarnessBinary(t)
	claude := config.LaunchConfig{Defaults: config.LaunchDefaults{BinaryPath: bin}}
	codex := config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex", CodexBinaryPath: bin}}
	withEnv := config.LaunchConfig{Defaults: config.LaunchDefaults{BinaryPath: bin, Env: map[string]string{"CLAUDE_CONFIG_DIR": trusted}}}
	cases := []struct {
		name     string
		lc       config.LaunchConfig
		spec     workerSpec
		injected map[string]string
		unset    []string
		refused  bool
	}{
		{name: "inherited account, untrusted", lc: claude, spec: workerSpec{name: "w"}, refused: true},
		{name: "--profile account, trusted", lc: claude, spec: workerSpec{name: "w", configDir: trusted}},
		{name: "--profile beats the profile env", lc: withEnv, spec: workerSpec{name: "w", configDir: untrusted}, refused: true},
		{name: "the profile env beats the inherited value", lc: withEnv, spec: workerSpec{name: "w"}},
		{name: "an injected value beats the inherited value", lc: claude, spec: workerSpec{name: "w"},
			injected: map[string]string{"CLAUDE_CONFIG_DIR": trusted}},
		{name: "a codex worker is checked against its own home", lc: codex, spec: workerSpec{name: "w"}},
		{name: "--harness codex is checked against its own home", lc: codex, spec: workerSpec{name: "w", harness: "codex"}},
	}
	// The codex cases run as an account that trusts top, so the claude
	// accounts above say nothing about them.
	codexHome := t.TempDir()
	writeCodexConfig(t, codexHome, "[projects.\""+filepath.ToSlash(top)+"\"]\ntrust_level = \"trusted\"\n")
	t.Setenv("CODEX_HOME", codexHome)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkWorkerFolderTrust(c.lc, top, c.spec, c.injected, c.unset)
			if c.refused != errors.Is(err, errUntrustedFolder) || (!c.refused && err != nil) {
				t.Errorf("err = %v, refused want %v", err, c.refused)
			}
			built, berr := buildWorkerInvocation(c.spec.request(c.lc, filepath.Join(top, ".claude", "worktrees", c.spec.name), c.injected, c.unset),
				"", func() (string, error) { return "00000000-0000-4000-8000-000000000000", nil }, io.Discard)
			if berr != nil {
				t.Fatalf("build: %v", berr)
			}
			if built.Invocation.Harness == "claude" {
				got := envValue(built.Invocation.Env, "CLAUDE_CONFIG_DIR")
				if refusedFor := claudeFolderTrust(claudeGlobalConfigFile(got, ""), top) == trustUntrusted; refusedFor != c.refused {
					t.Errorf("the worker runs as %s, whose trust disagrees with the check", got)
				}
			}
		})
	}

	// A codex account with no entry for top refuses the worker.
	writeCodexConfig(t, codexHome, "")
	if err := checkWorkerFolderTrust(codex, top, workerSpec{name: "w"}, nil, nil); !errors.Is(err, errUntrustedFolder) {
		t.Errorf("untrusted codex home: err = %v, want errUntrustedFolder", err)
	}
}

func writeCodexConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCodexFolderTrust pins codex's reading: an entry for the folder's exact
// spelling or physical path counts, any recorded trust_level counts, and a
// trusted parent does not (codex-cli 0.163.0 asked again for a new repository
// under a trusted home directory).
func TestCodexFolderTrust(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "projects", "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfgDir := t.TempDir()
	conf := func(body string) string { return writeCodexConfig(t, cfgDir, body) }
	entry := func(p, level string) string {
		return "[projects.\"" + filepath.ToSlash(p) + "\"]\ntrust_level = \"" + level + "\"\n"
	}

	cases := []struct {
		name, body string
		dirs       []string
		want       claudeTrustState
	}{
		{"the folder itself", entry(repo, "trusted"), []string{repo}, trustTrusted},
		{"through a symlink", entry(repo, "trusted"), []string{link}, trustTrusted},
		{"the link's own spelling", entry(link, "trusted"), []string{link}, trustTrusted},
		{"a recorded untrusted decision", entry(repo, "untrusted"), []string{repo}, trustTrusted},
		{"a trusted parent does not count", entry(base, "trusted"), []string{repo}, trustUntrusted},
		{"the repository root counts for a folder in it", entry(repo, "trusted"), []string{filepath.Join(repo, "sub"), repo}, trustTrusted},
		{"an entry with no trust_level", "[projects.\"" + filepath.ToSlash(repo) + "\"]\n", []string{repo}, trustUntrusted},
		{"no projects", "model = \"x\"\n", []string{repo}, trustUntrusted},
		{"an unparsable file", "[projects", []string{repo}, trustUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexFolderTrust(conf(c.body), c.dirs...); got != c.want {
				t.Errorf("trust = %v, want %v", got, c.want)
			}
		})
	}
	if got := codexFolderTrust(filepath.Join(t.TempDir(), "config.toml"), repo); got != trustUnknown {
		t.Errorf("a missing file = %v, want unknown", got)
	}
	if got := codexFolderTrust("", repo); got != trustUnknown {
		t.Errorf("no file = %v, want unknown", got)
	}
}

// TestCodexConfigFile follows codex's lookup: $CODEX_HOME/config.toml, else
// ~/.codex/config.toml.
func TestCodexConfigFile(t *testing.T) {
	home, codexHome := t.TempDir(), t.TempDir()
	if got := codexConfigFile("", home); got != filepath.Join(home, ".codex", "config.toml") {
		t.Errorf("default account: %s", got)
	}
	if got := codexConfigFile(codexHome, home); got != filepath.Join(codexHome, "config.toml") {
		t.Errorf("CODEX_HOME account: %s", got)
	}
	if got := codexConfigFile("", ""); got != "" {
		t.Errorf("no home: %q", got)
	}
}

// TestCheckCodexFolderTrust_Message names the repository root, where codex
// applies trust, and the account's CODEX_HOME when it is not the default.
func TestCheckCodexFolderTrust_Message(t *testing.T) {
	codexHome := t.TempDir()
	writeCodexConfig(t, codexHome, "")
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	err := checkCodexFolderTrust(sub, root, codexHome)
	if !errors.Is(err, errUntrustedFolder) {
		t.Fatalf("err = %v, want errUntrustedFolder", err)
	}
	if want := "cd " + root + " && CODEX_HOME=" + codexHome + " codex"; !strings.Contains(err.Error(), want) {
		t.Errorf("message %q lacks %q", err, want)
	}
	if err := checkCodexFolderTrust(sub, "", codexHome); err == nil || !strings.Contains(err.Error(), "cd "+sub+" ") {
		t.Errorf("outside a repository the fix names the folder: %v", err)
	}
}
