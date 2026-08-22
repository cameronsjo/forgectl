package launch

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestCodexSessionArgs_NativePosture(t *testing.T) {
	p := Profile{
		Model:          "gpt-5",
		ApprovalPolicy: "on-request",
		Sandbox:        "workspace-write",
		AddDir:         []string{"/shared"},
	}
	got := CodexSessionArgs(p)
	want := []string{"--ask-for-approval", "on-request", "--sandbox", "workspace-write", "--model", "gpt-5", "--add-dir", "/shared"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CodexSessionArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestCodexSessionArgs_OmitsEmptyModel(t *testing.T) {
	p := Profile{ApprovalPolicy: "on-request", Sandbox: "workspace-write"}
	got := CodexSessionArgs(p)
	for _, arg := range got {
		if arg == "--model" {
			t.Fatalf("empty model should use the Codex default: %v", got)
		}
	}
}

// TestCodexSessionArgs_NeverEmitsEffort pins the harness boundary: --effort is
// Claude Code's flag and Codex would reject it, so a resolved Effort (which a
// Codex profile still carries whenever its model happens to be a mapped alias,
// or whenever [launch.defaults] effort is set outright) must not leak across.
func TestCodexSessionArgs_NeverEmitsEffort(t *testing.T) {
	p := Profile{Model: "gpt-5", Effort: "high", ApprovalPolicy: "on-request", Sandbox: "read-only"}
	if got := CodexSessionArgs(p); containsStr(got, "--effort") {
		t.Errorf("CodexSessionArgs must not emit --effort, got %v", got)
	}
	if got := CodexExecArgs(p, []string{"hi"}); containsStr(got, "--effort") {
		t.Errorf("CodexExecArgs must not emit --effort, got %v", got)
	}
}

func TestCodexExecArgs_UsesNativeSandboxAndApprovalConfig(t *testing.T) {
	p := Profile{
		Model:          "gpt-5",
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
	}
	got := CodexExecArgs(p, []string{"review this"})
	want := []string{
		"exec", "--config", `approval_policy="never"`,
		"--sandbox", "read-only", "--model", "gpt-5", "review this",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CodexExecArgs = %v, want %v", got, want)
	}
}

func TestPiArgs_ProfileFirstUserArgsLast(t *testing.T) {
	p := Profile{Provider: "lm-studio", Model: "qwen/qwen3-coder-next"}
	got := PiArgs(p, []string{"--provider", "ollama", "-p", "review this"})
	want := []string{
		"--provider", "lm-studio", "--model", "qwen/qwen3-coder-next",
		"--provider", "ollama", "-p", "review this",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PiArgs = %v, want %v", got, want)
	}
}

func TestPiArgs_OmitsUnconfiguredSelectors(t *testing.T) {
	got := PiArgs(Profile{}, []string{"--resume"})
	if !reflect.DeepEqual(got, []string{"--resume"}) {
		t.Errorf("PiArgs = %v, want only the operator's args", got)
	}
}

func TestSessionArgs_FullPosture(t *testing.T) {
	p := Profile{Model: "opus", Effort: "medium", PermissionMode: "plan", AllowDanger: true, AddDir: []string{"/x", "/y"}}
	got := SessionArgs(p)
	want := []string{"--permission-mode", "plan", "--allow-dangerously-skip-permissions", "--ide", "--exclude-dynamic-system-prompt-sections", "--model", "opus", "--effort", "medium", "--add-dir", "/x", "--add-dir", "/y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestSessionArgs_AllowDangerOff_OmitsFlag(t *testing.T) {
	p := Profile{Model: "opus", PermissionMode: "plan", AllowDanger: false}
	got := SessionArgs(p)
	if containsStr(got, "--allow-dangerously-skip-permissions") {
		t.Errorf("SessionArgs with AllowDanger=false should omit the danger flag, got %v", got)
	}
}

// TestSessionArgs_NeverResumes pins what removing the interview means for the
// bare launcher: `forgectl launch` starts a NEW session, always. The old
// SessionArgs took a mode and could append a bare --resume (opening Claude
// Code's own picker); resume/fork now belong to `forgectl resume` alone, and
// this is the assertion that would fail if a mode were quietly reintroduced.
func TestSessionArgs_NeverResumes(t *testing.T) {
	got := SessionArgs(Profile{Model: "opus", PermissionMode: "plan"})
	for _, bad := range []string{"--resume", "--fork-session"} {
		if containsStr(got, bad) {
			t.Errorf("SessionArgs must not include %q — resume/fork is `forgectl resume`'s job, got %v", bad, got)
		}
	}
}

// TestEffortIsOmittedWhenUnresolved covers every Claude argv builder at once:
// an empty Effort emits NO flag, rather than "--effort" with an empty value.
// That is the whole contract of an unmapped model — the user's settings.json
// effortLevel stays in charge, exactly as it did before the flag existed.
func TestEffortIsOmittedWhenUnresolved(t *testing.T) {
	p := Profile{Model: "haiku", PermissionMode: "plan"}
	builders := map[string][]string{
		"SessionArgs": SessionArgs(p),
		"ResumeArgs":  ResumeArgs(p, "abc-123", false),
		"BuilderArgs": BuilderArgs(p, []string{"-p", "hi"}),
		"AgentsArgs":  AgentsArgs(p, []string{"agents"}),
	}
	for name, got := range builders {
		if containsStr(got, "--effort") {
			t.Errorf("%s emitted --effort for an unresolved level: %v", name, got)
		}
	}
}

// TestEffortFollowsModelInEveryBuilder pins the POSITION, not just the
// presence. --effort has to sit in the injected block immediately after
// --model so a user's trailing override still wins under Claude Code's
// last-flag-wins parsing; emitted after the user's args it would silently
// outrank them.
func TestEffortFollowsModelInEveryBuilder(t *testing.T) {
	p := Profile{Model: "sonnet", Effort: "high", PermissionMode: "plan"}
	builders := map[string][]string{
		"SessionArgs": SessionArgs(p),
		"ResumeArgs":  ResumeArgs(p, "abc-123", false),
		"BuilderArgs": BuilderArgs(p, []string{"-p", "hi"}),
		"AgentsArgs":  AgentsArgs(p, []string{"agents"}),
	}
	for name, got := range builders {
		if !containsSeq(got, []string{"--model", "sonnet", "--effort", "high"}) {
			t.Errorf("%s must emit --effort right after --model, got %v", name, got)
		}
	}
}

func TestResumeArgs_TargetsTheGivenSession(t *testing.T) {
	p := Profile{Model: "opus", Effort: "medium", PermissionMode: "plan", AllowDanger: true, AddDir: []string{"/x"}}
	got := ResumeArgs(p, "abc-123", false)
	want := []string{
		"--permission-mode", "plan", "--allow-dangerously-skip-permissions",
		"--ide", "--exclude-dynamic-system-prompt-sections", "--model", "opus",
		"--effort", "medium", "--resume", "abc-123", "--add-dir", "/x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// The distinction that earned ResumeArgs its own function: SessionArgs starts a
// new session and never carries an id, so `forgectl resume` — which has already
// picked one — needs a builder that puts the id straight after --resume.
func TestResumeArgs_PassesTheIDUnlikeSessionArgs(t *testing.T) {
	p := Profile{Model: "opus", PermissionMode: "plan"}
	if got := SessionArgs(p); containsStr(got, "abc-123") {
		t.Fatalf("SessionArgs unexpectedly carries a session id: %v", got)
	}
	if got := ResumeArgs(p, "abc-123", false); !containsSeq(got, []string{"--resume", "abc-123"}) {
		t.Errorf("ResumeArgs must pass the id straight after --resume, got %v", got)
	}
}

func TestResumeArgs_ForkAppendsForkSession(t *testing.T) {
	p := Profile{Model: "opus", PermissionMode: "plan"}
	got := ResumeArgs(p, "abc-123", true)
	if !containsSeq(got, []string{"--resume", "abc-123", "--fork-session"}) {
		t.Errorf("ResumeArgs(fork) missing expected seq, got %v", got)
	}
}

func TestBuilderArgs_ProfileFirst_UserArgsLast(t *testing.T) {
	p := Profile{PermissionMode: "plan", AllowDanger: true, Model: "sonnet", AddDir: []string{"/s"}}
	got := BuilderArgs(p, []string{"-p", "hi"})
	want := []string{"--permission-mode", "plan", "--allow-dangerously-skip-permissions", "--model", "sonnet", "--add-dir", "/s", "-p", "hi"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuilderArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestBuilderArgs_NoIdeOrExcludeFlags(t *testing.T) {
	p := Profile{PermissionMode: "plan", Model: "sonnet"}
	got := BuilderArgs(p, nil)
	for _, bad := range []string{"--ide", "--exclude-dynamic-system-prompt-sections", "--resume"} {
		if containsStr(got, bad) {
			t.Errorf("BuilderArgs should not include %q, got %v", bad, got)
		}
	}
}

// TestBuilderArgs_StrictMCPOnlyWhenSet pins BOTH directions, and the negative
// case is the one that matters. --strict-mcp-config makes Claude Code ignore
// every discovered MCP configuration — correct for the clean-room review,
// where the workspace is a third party's checkout, and a silent functional
// regression for the operator's ordinary `forgectl launch`, which shares this
// function and must keep its MCP servers. Emitted unconditionally, every user
// would lose their tools with no error message.
func TestBuilderArgs_StrictMCPOnlyWhenSet(t *testing.T) {
	on := BuilderArgs(Profile{PermissionMode: "plan", Model: "opus", StrictMCP: true}, []string{"-p", "hi"})
	if !containsStr(on, "--strict-mcp-config") {
		t.Errorf("StrictMCP profile must emit --strict-mcp-config, got %v", on)
	}
	// The flag must precede the user's args so a caller-supplied override still
	// wins under last-flag-wins parsing.
	if !containsSeq(on, []string{"--strict-mcp-config", "--model", "opus"}) {
		t.Errorf("--strict-mcp-config must sit in the injected posture block, got %v", on)
	}

	off := BuilderArgs(Profile{PermissionMode: "plan", Model: "opus"}, []string{"-p", "hi"})
	if containsStr(off, "--strict-mcp-config") {
		t.Errorf("an ordinary launch must keep its discovered MCP servers, got %v", off)
	}
}

// TestResolve_NeverSetsStrictMCP is the other half of the same control: the
// flag has no config surface at all, so no [launch.defaults] or project block
// can turn it on for an ordinary launch. The review dispatch sets it directly
// on the Profile, and that is the only writer.
func TestResolve_NeverSetsStrictMCP(t *testing.T) {
	allowDanger := true
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{
			Harness: "claude", Model: "sonnet", PermissionMode: "acceptEdits",
			AllowDanger: &allowDanger, AddDir: []string{"/s"},
		},
		Projects: []config.LaunchProject{{Match: "/proj", Model: "haiku"}},
	}
	for name, p := range map[string]Profile{
		"resolved": resolve(lc, "/proj", "/home/u"),
		"defaults": DefaultsProfile(lc),
	} {
		if p.StrictMCP {
			t.Errorf("%s profile set StrictMCP; config must not be able to strip an operator's MCP servers", name)
		}
	}
}

func TestAgentsArgs_InjectsSubsetAfterAgents(t *testing.T) {
	p := Profile{PermissionMode: "plan", AllowDanger: true, Model: "opus"}
	got := AgentsArgs(p, []string{"agents", "--cwd", "/proj"})
	want := []string{"agents", "--permission-mode", "plan", "--allow-dangerously-skip-permissions", "--model", "opus", "--cwd", "/proj"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentsArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestAgentsArgs_AllowDangerOff_OmitsFlag(t *testing.T) {
	p := Profile{PermissionMode: "plan", AllowDanger: false, Model: "opus"}
	got := AgentsArgs(p, []string{"agents"})
	if containsStr(got, "--allow-dangerously-skip-permissions") {
		t.Errorf("AgentsArgs with AllowDanger=false should omit the danger flag, got %v", got)
	}
}

func TestIsAgentsPassthrough(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"agents", "--json"}, true},
		{[]string{"agents", "--help"}, true},
		{[]string{"agents", "-h"}, true},
		{[]string{"agents", "--all", "--json"}, true},
		{[]string{"agents"}, false},
		{[]string{"agents", "--cwd", "/x"}, false},
	}
	for _, tc := range cases {
		got := IsAgentsPassthrough(tc.args)
		if got != tc.want {
			t.Errorf("IsAgentsPassthrough(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestMergeEnv_ExtraOverridesAndAppends(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/h"}
	extra := map[string]string{"FOO": "bar", "HOME": "/override"}
	got := MergeEnv(base, extra)
	want := []string{"PATH=/bin", "FOO=bar", "HOME=/override"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeEnv =\n  %v\nwant\n  %v", got, want)
	}
}

func TestMergeEnv_EmptyExtra_ReturnsBase(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/h"}
	got := MergeEnv(base, nil)
	if !reflect.DeepEqual(got, base) {
		t.Errorf("MergeEnv with nil extra = %v, want unchanged base %v", got, base)
	}
}

func TestMergeMaps_OverWins(t *testing.T) {
	base := map[string]string{"A": "1", "B": "2"}
	over := map[string]string{"B": "override", "C": "3"}
	got := MergeMaps(base, over)
	want := map[string]string{"A": "1", "B": "override", "C": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeMaps = %v, want %v", got, want)
	}
}

func TestMergeMaps_BothEmpty_ReturnsNil(t *testing.T) {
	if got := MergeMaps(nil, nil); got != nil {
		t.Errorf("MergeMaps(nil, nil) = %v, want nil", got)
	}
}

func TestMergeMaps_NilBase_ReturnsOverContents(t *testing.T) {
	over := map[string]string{"A": "1"}
	got := MergeMaps(nil, over)
	if !reflect.DeepEqual(got, over) {
		t.Errorf("MergeMaps(nil, over) = %v, want %v", got, over)
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsSeq(haystack, seq []string) bool {
	for i := 0; i+len(seq) <= len(haystack); i++ {
		if reflect.DeepEqual(haystack[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// hostileArgs is an argv whose config-derived values carry the three classes of
// control byte a terminal acts on: a C0 escape sequence (SGR color), DEL, and
// the single-byte C1 CSI that encoding-aware layers routinely miss.
func hostileArgs() []string {
	return []string{"--model", "sonnet\x1b[31m", "--permission-mode", "plan\x7f", "--add-dir", "/work\x9bA"}
}

// assertBannerInert asserts no control rune except tab survived into a rendered
// banner — the newline Fprintln adds is excepted for the same reason.
func assertBannerInert(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			t.Errorf("banner carries control rune %U: %q", r, s)
			return
		}
	}
}

// TestBanner_SanitizesControlBytes pins the fix for #243: the banner renders
// config-derived argv, and an escape sequence in config.toml must not reach the
// terminal live. The visible text still has to survive — a sanitizer that ate
// the posture would defeat the banner's only purpose.
func TestBanner_SanitizesControlBytes(t *testing.T) {
	var buf bytes.Buffer
	Banner(&buf, hostileArgs())
	got := buf.String()
	assertBannerInert(t, got)
	for _, want := range []string{"→ claude", "--model", "sonnet", "plan", "/work"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner dropped visible text %q: %q", want, got)
		}
	}
}

// TestHarnessBanner_SanitizesControlBytes is the Codex-side half: the harness
// name is joined into the same line, so the whole assembled string is what gets
// sanitized, not just the argv.
func TestHarnessBanner_SanitizesControlBytes(t *testing.T) {
	var buf bytes.Buffer
	HarnessBanner(&buf, "codex\x1b[2K", hostileArgs())
	got := buf.String()
	assertBannerInert(t, got)
	for _, want := range []string{"→ codex", "--model", "sonnet", "/work"} {
		if !strings.Contains(got, want) {
			t.Errorf("harness banner dropped visible text %q: %q", want, got)
		}
	}
}

func TestHarnessBanner_EmptyArgsHasNoTrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	HarnessBanner(&buf, "pi", nil)
	if got, want := buf.String(), "→ pi\n"; got != want {
		t.Errorf("HarnessBanner = %q, want %q", got, want)
	}
}
