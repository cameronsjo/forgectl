package pr

// Test plan for reviewsandbox.go (Classification: security control, #694)
//
//   [x] reviewSettingsJSON carries the whole profile: permissions, the
//       sandbox block (enabled, fail-closed, no unsandboxed retry, no
//       auto-allow, weakening switches pinned false, no excludedCommands,
//       workspace denied for writes, network limited to the PR's gh hosts,
//       strict), and disableAllHooks
//   [x] a local review gets no network and a writable findings dir
//   [x] a linked worktree's shared git dir is denied too (real git)
//   [x] an unparseable .git file, a relative workspace, or a path the
//       Linux sandbox would skip (glob characters) is refused
//   [x] ghAPIDomains per host shape
//   [x] launchInline passes it with --settings after --setting-sources "",
//       remote and local, and writes nothing into the workspace
//   [x] launchInline refuses, before any window, when the sandbox is
//       unsupported
//   [x] claudeSandboxSupported platform table
//   [x] pinReviewGitEnv drops every ambient indexed pair and appends the pins
//   [x] the pins stop a repo-configured core.fsmonitor from running (real git)

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// decodeSettings decodes a reviewSettingsJSON document twice: typed, and as
// raw maps so a test can see which keys are present at all.
func decodeSettings(t *testing.T, doc string) (reviewSettings, map[string]any) {
	t.Helper()
	var typed reviewSettings
	if err := json.Unmarshal([]byte(doc), &typed); err != nil {
		t.Fatalf("settings are not JSON: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("settings are not JSON: %v", err)
	}
	return typed, raw
}

// assertStrictSettings checks the posture every reviewer settings document
// must carry, whatever its mode.
func assertStrictSettings(t *testing.T, s reviewSettings, raw map[string]any, workspace string) {
	t.Helper()
	sb := s.Sandbox
	if !s.DisableAllHooks {
		t.Error("disableAllHooks must be true")
	}
	if !sb.Enabled || !sb.FailIfUnavailable {
		t.Errorf("sandbox must be enabled and fail closed: %+v", sb)
	}
	if sb.AllowUnsandboxedCommands {
		t.Error("allowUnsandboxedCommands must be false: it is the dangerouslyDisableSandbox escape hatch")
	}
	if sb.AutoAllowBashIfSandboxed {
		t.Error("autoAllowBashIfSandboxed must be false: the allow-list stays the gate")
	}
	if !sb.Network.StrictAllowlist {
		t.Error("network.strictAllowlist must be true")
	}
	if !slices.Contains(sb.Filesystem.DenyWrite, workspace) {
		t.Errorf("denyWrite %v must name the workspace %s", sb.Filesystem.DenyWrite, workspace)
	}
	if s.Permissions.DefaultMode != "plan" || len(s.Permissions.Allow) == 0 || len(s.Permissions.Deny) == 0 {
		t.Errorf("the settings must carry the whole permission profile: %+v", s.Permissions)
	}
	sbRaw := rawSandbox(t, raw)
	// Explicit false, not omitted: the posture must not ride a default, and
	// every weakening switch is pinned off.
	for _, key := range []string{
		"enabled", "failIfUnavailable", "allowUnsandboxedCommands", "autoAllowBashIfSandboxed",
		"enableWeakerNestedSandbox", "enableWeakerNetworkIsolation", "allowAppleEvents",
	} {
		v, ok := sbRaw[key]
		if !ok {
			t.Errorf("sandbox.%s must be written explicitly", key)
		}
		if strings.HasPrefix(key, "enableWeaker") || key == "allowAppleEvents" {
			if v != false {
				t.Errorf("sandbox.%s = %v, want false", key, v)
			}
		}
	}
	network, _ := sbRaw["network"].(map[string]any)
	for _, key := range []string{"allowLocalBinding", "allowAllUnixSockets", "allowMachLookup"} {
		if v, ok := network[key]; !ok || v != false {
			t.Errorf("sandbox.network.%s = %v (present %v), want an explicit false", key, v, ok)
		}
	}
	if _, ok := sbRaw["excludedCommands"]; ok {
		t.Error("sandbox must carry no excludedCommands: an excluded command runs unsandboxed")
	}
}

func rawSandbox(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	sb, ok := raw["sandbox"].(map[string]any)
	if !ok {
		t.Fatalf("settings carry no sandbox object: %v", raw)
	}
	return sb
}

func TestReviewSettingsJSON_RemoteIsStrictAndScopedToThePRHost(t *testing.T) {
	ws := t.TempDir()
	perms, err := remoteProfile("github.com", Ref{Owner: "o", Repo: "r", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := reviewSettingsJSON(ws, "github.com", perms)
	if err != nil {
		t.Fatalf("reviewSettingsJSON: %v", err)
	}
	typed, raw := decodeSettings(t, doc)
	assertStrictSettings(t, typed, raw, ws)
	if want := []string{"github.com", "api.github.com"}; !slices.Equal(typed.Sandbox.Network.AllowedDomains, want) {
		t.Errorf("allowedDomains = %v, want %v", typed.Sandbox.Network.AllowedDomains, want)
	}
	if !slices.Equal(typed.Permissions.Allow, perms.Allow) || !slices.Equal(typed.Permissions.Deny, perms.Deny) {
		t.Errorf("settings permissions = %+v, want the remote profile %+v", typed.Permissions, perms)
	}
}

func TestReviewSettingsJSON_LocalReachesNoNetwork(t *testing.T) {
	ws := t.TempDir()
	findingsDir := filepath.Join(t.TempDir(), "findings")
	doc, err := reviewSettingsJSON(ws, "", localProfile(findingsDir))
	if err != nil {
		t.Fatalf("reviewSettingsJSON: %v", err)
	}
	typed, raw := decodeSettings(t, doc)
	assertStrictSettings(t, typed, raw, ws)
	network, _ := rawSandbox(t, raw)["network"].(map[string]any)
	domains, ok := network["allowedDomains"].([]any)
	if !ok || len(domains) != 0 {
		t.Errorf("a local review's allowedDomains must be an explicit empty list, got %v", network["allowedDomains"])
	}
	for _, p := range typed.Sandbox.Filesystem.DenyWrite {
		if strings.HasPrefix(findingsDir, p) {
			t.Errorf("denyWrite entry %s covers the findings dir, the reviewer's only legitimate write", p)
		}
	}
}

func TestGhAPIDomains(t *testing.T) {
	cases := []struct {
		host string
		want []string
	}{
		{"github.com", []string{"github.com", "api.github.com"}},
		{"acme.ghe.com", []string{"acme.ghe.com", "api.acme.ghe.com"}},
		{"ghe.corp.example", []string{"ghe.corp.example"}},
	}
	for _, tc := range cases {
		if got := ghAPIDomains(tc.host); !slices.Equal(got, tc.want) {
			t.Errorf("ghAPIDomains(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestReviewDenyWrite_LinkedWorktreeDeniesTheSharedGitDir uses a real `git
// worktree add`, the shape `pr local` builds. Claude Code's sandbox opens a
// linked worktree's shared git directory for writes on its own, and that
// directory is the operator's real repository.
func TestReviewDenyWrite_LinkedWorktreeDeniesTheSharedGitDir(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	ws := filepath.Join(base, "ws")
	gitRun := func(args ...string) {
		t.Helper()
		cmd := osexec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun("init", "-q", repo)
	gitRun("-C", repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x")
	gitRun("-C", repo, "worktree", "add", "-q", "--detach", ws)

	got, err := reviewDenyWrite(ws)
	if err != nil {
		t.Fatalf("reviewDenyWrite: %v", err)
	}
	shared := resolvedOrSelf(filepath.Join(repo, ".git"))
	if !slices.Contains(got, shared) {
		t.Errorf("denyWrite %v must name the shared git dir %s", got, shared)
	}
	if !slices.Contains(got, ws) {
		t.Errorf("denyWrite %v must name the workspace %s", got, ws)
	}
}

func TestReviewDenyWrite_CloneDeniesOnlyTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := reviewDenyWrite(ws)
	if err != nil {
		t.Fatalf("reviewDenyWrite: %v", err)
	}
	for _, p := range got {
		if p != ws && p != resolvedOrSelf(ws) {
			t.Errorf("a clone's denyWrite should hold only the workspace, got extra %s", p)
		}
	}
}

func TestReviewDenyWrite_Refusals(t *testing.T) {
	if _, err := reviewDenyWrite("relative/ws"); err == nil {
		t.Error("a relative workspace must be refused")
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("not a pointer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewDenyWrite(ws); err == nil {
		t.Error("an unparseable .git file must be refused, not skipped")
	}
	globbed := filepath.Join(t.TempDir(), "ws[1]")
	if err := os.Mkdir(globbed, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewDenyWrite(globbed); err == nil {
		t.Error("a workspace path holding a glob character must be refused: the Linux sandbox skips such a write deny")
	}
}

// settingsFlagFrom returns the settings document passed with --settings in a
// tmux new-window argv, after asserting --setting-sources "" precedes it.
func settingsFlagFrom(t *testing.T, args []string) (reviewSettings, map[string]any) {
	t.Helper()
	src := slices.Index(args, "--setting-sources")
	if src < 0 || src+1 >= len(args) || args[src+1] != "" {
		t.Fatalf("claude argv must carry --setting-sources with an empty value (load no settings file): %v", args)
	}
	i := slices.Index(args, "--settings")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("claude argv carries no --settings: %v", args)
	}
	return decodeSettings(t, args[i+1])
}

// assertNoWorkspaceClaudeDir: the reviewer's configuration must never be
// written into the workspace, which holds the PR head. A PR-committed
// `.claude` symlink would otherwise redirect that write outside it.
func assertNoWorkspaceClaudeDir(t *testing.T, ws string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(ws, ".claude")); err == nil {
		t.Errorf("forgectl wrote %s/.claude; the reviewer's settings belong on its command line only", ws)
	}
}

func TestLaunchInline_PassesTheWholeProfileAndLoadsNoSettingsFile(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	s, raw := settingsFlagFrom(t, fake.Last().Args)
	assertStrictSettings(t, s, raw, ws)
	if want := []string{"github.com", "api.github.com"}; !slices.Equal(s.Sandbox.Network.AllowedDomains, want) {
		t.Errorf("remote review allowedDomains = %v, want %v", s.Sandbox.Network.AllowedDomains, want)
	}
	if !slices.Contains(s.Permissions.Allow, "Bash(gh pr view 42 --repo github.com/o/r)") {
		t.Errorf("remote settings must carry the generated gh reads: %v", s.Permissions.Allow)
	}
	assertNoWorkspaceClaudeDir(t, ws)

	fake2 := successfulLaunchRunner()
	c2 := New(fake2, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws2 := fakeWorkspace(t)
	findingsDir := t.TempDir()
	local := Session{Ref: mustLocalRef("abc1234", 1), Workspace: ws2, Agent: "claude", FindingsDir: findingsDir}
	if _, err := c2.Launch(context.Background(), local, config.Config{}); err != nil {
		t.Fatalf("Launch local: %v", err)
	}
	s2, raw2 := settingsFlagFrom(t, fake2.Last().Args)
	assertStrictSettings(t, s2, raw2, ws2)
	if len(s2.Sandbox.Network.AllowedDomains) != 0 {
		t.Errorf("a local review must reach no host, got %v", s2.Sandbox.Network.AllowedDomains)
	}
	if !slices.Equal(s2.Permissions.Allow, localProfile(findingsDir).Allow) {
		t.Errorf("local settings allow = %v, want localProfile's", s2.Permissions.Allow)
	}
	assertNoWorkspaceClaudeDir(t, ws2)
}

func TestLaunchInline_RefusesWhenTheSandboxIsUnsupported(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	c.sandboxSupported = func() error { return errors.New("no bubblewrap here") }
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	_, err := c.Launch(context.Background(), sess, config.Config{})
	if err == nil || !strings.Contains(err.Error(), "no bubblewrap here") {
		t.Fatalf("Launch = %v, want a refusal naming the sandbox reason", err)
	}
	for _, call := range fake.Calls {
		if call.Name == "tmux" && len(call.Args) > 0 && call.Args[0] == "new-window" {
			t.Errorf("a refused dispatch must open no window: %v", call.Args)
		}
	}
}

func TestClaudeSandboxSupported(t *testing.T) {
	origGOOS, origLook, origStat := sandboxGOOS, sandboxLookPath, sandboxStat
	t.Cleanup(func() { sandboxGOOS, sandboxLookPath, sandboxStat = origGOOS, origLook, origStat })

	found := func(file string) (string, error) { return "/usr/bin/" + file, nil }
	noSocat := func(file string) (string, error) {
		if file == "socat" {
			return "", osexec.ErrNotFound
		}
		return "/usr/bin/" + file, nil
	}
	statOK := func(string) (fs.FileInfo, error) { return nil, nil }
	statMissing := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }

	cases := []struct {
		name    string
		goos    string
		look    func(string) (string, error)
		stat    func(string) (fs.FileInfo, error)
		wantErr string
	}{
		{"linux with both", "linux", found, statMissing, ""},
		{"linux without socat", "linux", noSocat, statOK, "socat"},
		{"darwin with seatbelt", "darwin", noSocat, statOK, ""},
		{"darwin without seatbelt", "darwin", found, statMissing, seatbeltPath},
		{"windows", "windows", found, statOK, "does not support windows"},
	}
	for _, tc := range cases {
		sandboxGOOS, sandboxLookPath, sandboxStat = tc.goos, tc.look, tc.stat
		err := claudeSandboxSupported()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected refusal %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestPinReviewGitEnv_ReplacesAmbientEntries(t *testing.T) {
	in := []string{"HTTPS_PROXY=http://proxy:3128", "GIT_CONFIG_COUNT=5", "GIT_CONFIG_KEY_0=core.pager", "GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_KEY_4=core.sshCommand", "GIT_CONFIG_VALUE_4=x"}
	got := pinReviewGitEnv(in)
	want := append([]string{"HTTPS_PROXY=http://proxy:3128"}, reviewGitEnv...)
	if !slices.Equal(got, want) {
		t.Errorf("pinReviewGitEnv = %v, want %v", got, want)
	}
	if in[1] != "GIT_CONFIG_COUNT=5" {
		t.Errorf("pinReviewGitEnv mutated its input: %v", in)
	}
}

// TestReviewGitEnv_StopsARepoConfiguredFsmonitor runs real git against a
// repository whose own .git/config names a core.fsmonitor command, the
// configuration an injected reviewer would plant. The control run (no pins)
// proves the command runs at all; the pinned run proves the pins stop it.
func TestReviewGitEnv_StopsARepoConfiguredFsmonitor(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	base := []string{"GIT_CONFIG_GLOBAL=/dev/null", "PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	gitRun := func(env []string, args ...string) {
		t.Helper()
		cmd := osexec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
		cmd.Dir = repo
		cmd.Env = env
		_, _ = cmd.CombinedOutput()
	}
	setup := append([]string{"GIT_CONFIG_NOSYSTEM=1"}, base...)
	gitRun(setup, "init", "-q", ".")
	gitRun(setup, "config", "core.fsmonitor", "touch "+marker+"; false")

	gitRun(setup, "status")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: a repo-configured core.fsmonitor did not run without the pins, so this test proves nothing: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	gitRun(pinReviewGitEnv(base), "status")
	if _, err := os.Stat(marker); err == nil {
		t.Error("core.fsmonitor from the repository's config ran under the review window's git pins")
	}
}
