package launch

import (
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestCodexSessionArgs_TranslatesModes(t *testing.T) {
	p := Profile{
		ApprovalPolicy: "on-request",
		Sandbox:        "workspace-write",
		AddDir:         []string{"/shared"},
	}
	tests := []struct {
		mode SessionMode
		want []string
	}{
		{
			New,
			[]string{"--ask-for-approval", "on-request", "--sandbox", "workspace-write", "--model", "gpt-5", "--add-dir", "/shared"},
		},
		{
			Resume,
			[]string{"resume", "--last", "--ask-for-approval", "on-request", "--sandbox", "workspace-write", "--model", "gpt-5", "--add-dir", "/shared"},
		},
		{
			Fork,
			[]string{"fork", "--last", "--ask-for-approval", "on-request", "--sandbox", "workspace-write", "--model", "gpt-5", "--add-dir", "/shared"},
		},
	}
	for _, tc := range tests {
		if got := CodexSessionArgs(p, "gpt-5", tc.mode); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CodexSessionArgs(%v) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestCodexSessionArgs_OmitsEmptyModel(t *testing.T) {
	p := Profile{ApprovalPolicy: "on-request", Sandbox: "workspace-write"}
	got := CodexSessionArgs(p, "", New)
	for _, arg := range got {
		if arg == "--model" {
			t.Fatalf("empty model should use the Codex default: %v", got)
		}
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

func TestSessionArgs_New_FullPosture(t *testing.T) {
	p := Profile{PermissionMode: "plan", AllowDanger: true, AddDir: []string{"/x", "/y"}}
	got := SessionArgs(p, "opus", New)
	want := []string{"--permission-mode", "plan", "--allow-dangerously-skip-permissions", "--ide", "--exclude-dynamic-system-prompt-sections", "--model", "opus", "--add-dir", "/x", "--add-dir", "/y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionArgs(New) =\n  %v\nwant\n  %v", got, want)
	}
}

func TestSessionArgs_AllowDangerOff_OmitsFlag(t *testing.T) {
	p := Profile{PermissionMode: "plan", AllowDanger: false}
	got := SessionArgs(p, "opus", New)
	if containsStr(got, "--allow-dangerously-skip-permissions") {
		t.Errorf("SessionArgs with AllowDanger=false should omit the danger flag, got %v", got)
	}
}

func TestSessionArgs_Resume(t *testing.T) {
	p := Profile{PermissionMode: "plan"}
	got := SessionArgs(p, "opus", Resume)
	if !containsSeq(got, []string{"--model", "opus", "--resume"}) {
		t.Errorf("SessionArgs(Resume) missing expected seq, got %v", got)
	}
	if containsStr(got, "--fork-session") {
		t.Errorf("SessionArgs(Resume) should not include --fork-session, got %v", got)
	}
}

func TestSessionArgs_Fork_ImpliesResume(t *testing.T) {
	p := Profile{PermissionMode: "plan"}
	got := SessionArgs(p, "opus", Fork)
	if !containsSeq(got, []string{"--resume", "--fork-session"}) {
		t.Errorf("SessionArgs(Fork) missing expected seq, got %v", got)
	}
}

func TestResumeArgs_TargetsTheGivenSession(t *testing.T) {
	p := Profile{PermissionMode: "plan", AllowDanger: true, AddDir: []string{"/x"}}
	got := ResumeArgs(p, "opus", "abc-123", false)
	want := []string{
		"--permission-mode", "plan", "--allow-dangerously-skip-permissions",
		"--ide", "--exclude-dynamic-system-prompt-sections", "--model", "opus",
		"--resume", "abc-123", "--add-dir", "/x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResumeArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// The distinction that earned ResumeArgs its own function: SessionArgs appends
// a BARE --resume, which opens Claude Code's own picker. `forgectl resume` has
// already picked, so the id must follow the flag.
func TestResumeArgs_PassesTheIDUnlikeSessionArgs(t *testing.T) {
	p := Profile{PermissionMode: "plan"}
	if got := SessionArgs(p, "opus", Resume); containsStr(got, "abc-123") {
		t.Fatalf("SessionArgs unexpectedly carries a session id: %v", got)
	}
	if got := ResumeArgs(p, "opus", "abc-123", false); !containsSeq(got, []string{"--resume", "abc-123"}) {
		t.Errorf("ResumeArgs must pass the id straight after --resume, got %v", got)
	}
}

func TestResumeArgs_ForkAppendsForkSession(t *testing.T) {
	p := Profile{PermissionMode: "plan"}
	got := ResumeArgs(p, "opus", "abc-123", true)
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
