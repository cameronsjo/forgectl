package launch

import (
	"slices"
	"testing"
)

func validCodexRemoteRequest() CodexRemoteRequest {
	return CodexRemoteRequest{
		BinaryPath:            "/opt/homebrew/bin/codex",
		Home:                  "/private/tmp/codex-home",
		CodexHome:             "/private/tmp/codex-home/codex",
		Socket:                "/private/tmp/codex-home/codex.sock",
		NativeSessionID:       "01234567-89ab-cdef-0123-456789abcdef",
		Model:                 "gpt-5-codex",
		Provider:              "openai",
		CWD:                   "/private/tmp/workspace",
		RuntimeWorkspaceRoots: []string{"/private/tmp/workspace", "/private/tmp/workspace/tools"},
		ApprovalPolicy:        "on-request",
		ApprovalsReviewer:     "user",
		Sandbox: CodexRemoteSandbox{
			Type:                "workspace-write",
			NetworkAccess:       false,
			WritableRoots:       []string{"/private/tmp/workspace"},
			ExcludeTmpdirEnvVar: true,
			ExcludeSlashTmp:     true,
		},
	}
}

func TestBuildCodexRemoteInvocation(test *testing.T) {
	built, err := BuildCodexRemoteInvocation(validCodexRemoteRequest())
	if err != nil {
		test.Fatalf("BuildCodexRemoteInvocation: %v", err)
	}
	wantArgs := []string{
		"resume", "01234567-89ab-cdef-0123-456789abcdef",
		"--remote", "unix:///private/tmp/codex-home/codex.sock",
		"--cd", "/private/tmp/workspace",
		"--sandbox", "workspace-write",
		"--ask-for-approval", "on-request",
		"--config", `approvals_reviewer="user"`,
		"--config", "sandbox_workspace_write.network_access=false",
		"--config", `sandbox_workspace_write.writable_roots=["/private/tmp/workspace"]`,
		"--config", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
		"--config", "sandbox_workspace_write.exclude_slash_tmp=true",
		"--add-dir", "/private/tmp/workspace/tools",
	}
	if !slices.Equal(built.Invocation.Args, wantArgs) {
		test.Fatalf("args = %#v, want %#v", built.Invocation.Args, wantArgs)
	}
	if built.Invocation.Harness != "codex" || built.Invocation.CWD != "/private/tmp/workspace" {
		test.Fatalf("invocation = %+v", built.Invocation)
	}
	if built.Invocation.Binary != (ResolvedBinary{Path: "/opt/homebrew/bin/codex", Source: BinaryCodexConfig}) {
		test.Fatalf("binary = %+v", built.Invocation.Binary)
	}
	wantEnv := []string{
		"PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C.UTF-8",
		"TERM=xterm-256color",
		"HOME=/private/tmp/codex-home",
		"CODEX_HOME=/private/tmp/codex-home/codex",
		"XDG_CACHE_HOME=/private/tmp/codex-home/.cache",
		"XDG_CONFIG_HOME=/private/tmp/codex-home/.config",
		"XDG_DATA_HOME=/private/tmp/codex-home/.local/share",
	}
	if !slices.Equal(built.Invocation.Env, wantEnv) {
		test.Fatalf("env = %#v, want %#v", built.Invocation.Env, wantEnv)
	}
	if built.Posture != PostureCodexSession || built.Worker {
		test.Fatalf("posture/worker = %q/%t", built.Posture, built.Worker)
	}
	if built.Profile.Sandbox != "workspace-write" || built.Profile.ApprovalPolicy != "on-request" || built.Profile.Model != "gpt-5-codex" {
		test.Fatalf("profile = %+v", built.Profile)
	}
}

func TestBuildCodexRemoteInvocationSupportsNativePolicies(test *testing.T) {
	policyCases := []struct {
		name          string
		approval      string
		sandbox       string
		writableRoots []string
		wantSandbox   string
	}{
		{name: "never workspace write", approval: "never", sandbox: "workspaceWrite", writableRoots: []string{"/private/tmp"}, wantSandbox: "workspace-write"},
		{name: "on request read only", approval: "on-request", sandbox: "readOnly", wantSandbox: "read-only"},
	}
	for _, policyCase := range policyCases {
		test.Run(policyCase.name, func(test *testing.T) {
			request := validCodexRemoteRequest()
			request.ApprovalPolicy = policyCase.approval
			request.Sandbox.Type = policyCase.sandbox
			request.Sandbox.WritableRoots = policyCase.writableRoots
			request.Sandbox.ExcludeTmpdirEnvVar = policyCase.sandbox == "workspaceWrite"
			request.Sandbox.ExcludeSlashTmp = policyCase.sandbox == "workspaceWrite"
			built, err := BuildCodexRemoteInvocation(request)
			if err != nil {
				test.Fatalf("BuildCodexRemoteInvocation: %v", err)
			}
			if built.Worker || built.Profile.ApprovalPolicy != policyCase.approval || built.Profile.Sandbox != policyCase.wantSandbox {
				test.Fatalf("worker/profile = %t/%+v", built.Worker, built.Profile)
			}
			for argumentIndex := range built.Invocation.Args {
				if built.Invocation.Args[argumentIndex] == "--sandbox" && argumentIndex+1 < len(built.Invocation.Args) && built.Invocation.Args[argumentIndex+1] != policyCase.wantSandbox {
					test.Fatalf("sandbox argument = %q, want %q", built.Invocation.Args[argumentIndex+1], policyCase.wantSandbox)
				}
			}
		})
	}
}

func TestBuildCodexRemoteInvocationRejectsUnsafeRequests(test *testing.T) {
	tests := map[string]func(*CodexRemoteRequest){
		"provider": func(req *CodexRemoteRequest) { req.Provider = "azure" },
		"reviewer": func(req *CodexRemoteRequest) { req.ApprovalsReviewer = "auto" },
		"approval": func(req *CodexRemoteRequest) { req.ApprovalPolicy = "untrusted" },
		"network":  func(req *CodexRemoteRequest) { req.Sandbox.NetworkAccess = true },
		"posture":  func(req *CodexRemoteRequest) { req.Sandbox.Type = "danger-full-access" },
		"model":    func(req *CodexRemoteRequest) { req.Model = "bad model" },
		"duplicate root": func(req *CodexRemoteRequest) {
			req.RuntimeWorkspaceRoots = append(req.RuntimeWorkspaceRoots, req.CWD)
		},
		"unclean root": func(req *CodexRemoteRequest) {
			req.RuntimeWorkspaceRoots = append(req.RuntimeWorkspaceRoots, "/private/tmp/workspace/../other")
		},
		"cwd absent": func(req *CodexRemoteRequest) {
			req.RuntimeWorkspaceRoots = []string{"/private/tmp/workspace/tools"}
		},
		"codex home escapes home": func(req *CodexRemoteRequest) {
			req.CodexHome = "/private/tmp/other"
		},
	}
	for name, mutate := range tests {
		test.Run(name, func(test *testing.T) {
			req := validCodexRemoteRequest()
			mutate(&req)
			if _, err := BuildCodexRemoteInvocation(req); err == nil {
				test.Fatal("BuildCodexRemoteInvocation accepted unsafe request")
			}
		})
	}
}
