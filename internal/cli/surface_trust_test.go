package cli

import (
	"errors"
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

// TestClaudeFolderTrust pins the inheritance reading: the folder or any
// parent trusted counts, under its own spelling or its physical path, and
// anything the check cannot read refuses nothing.
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
// worker runs as: --profile's config dir, then the profile's env, then the
// inherited CLAUDE_CONFIG_DIR. A worker that is not claude is not checked.
func TestCheckWorkerFolderTrust(t *testing.T) {
	top := t.TempDir()
	trusted, untrusted, inherited := t.TempDir(), t.TempDir(), t.TempDir()
	writeClaudeConfig(t, trusted, `{"projects":{"`+filepath.ToSlash(top)+`":{"hasTrustDialogAccepted":true}}}`)
	writeClaudeConfig(t, untrusted, `{"projects":{}}`)
	writeClaudeConfig(t, inherited, `{"projects":{}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", inherited)

	codex := config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"}}
	withEnv := config.LaunchConfig{Defaults: config.LaunchDefaults{Env: map[string]string{"CLAUDE_CONFIG_DIR": trusted}}}
	cases := []struct {
		name    string
		lc      config.LaunchConfig
		spec    workerSpec
		refused bool
	}{
		{"inherited account, untrusted", config.LaunchConfig{}, workerSpec{name: "w"}, true},
		{"--profile account, trusted", config.LaunchConfig{}, workerSpec{name: "w", configDir: trusted}, false},
		{"--profile beats the profile env", withEnv, workerSpec{name: "w", configDir: untrusted}, true},
		{"the profile env beats the inherited value", withEnv, workerSpec{name: "w"}, false},
		{"a codex worker is not checked", codex, workerSpec{name: "w"}, false},
		{"--harness codex is not checked", config.LaunchConfig{}, workerSpec{name: "w", harness: "codex"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkWorkerFolderTrust(c.lc, top, c.spec)
			if c.refused != errors.Is(err, errUntrustedFolder) || (!c.refused && err != nil) {
				t.Errorf("err = %v, refused want %v", err, c.refused)
			}
		})
	}
}
