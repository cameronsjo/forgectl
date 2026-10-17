package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"unicode"
	"unicode/utf8"
)

const (
	codexSupportedModelProvider = "openai"
	codexRemoteMaxRoots         = 64
	codexRemoteMaxRootBytes     = 4096
)

// CodexRemoteSandbox is the effective sandbox posture reported by Codex for a
// prepared remote view.
type CodexRemoteSandbox struct {
	Type                string   `json:"type"`
	NetworkAccess       bool     `json:"network_access"`
	WritableRoots       []string `json:"writable_roots,omitempty"`
	ExcludeTmpdirEnvVar bool     `json:"exclude_tmpdir_env_var"`
	ExcludeSlashTmp     bool     `json:"exclude_slash_tmp"`
}

// CodexRemoteView is the effective, bounded posture prepared for a Codex
// remote UI attachment. It contains no transcript or prompt content.
type CodexRemoteView struct {
	NativeSessionID       string             `json:"native_session_id"`
	Provider              string             `json:"provider"`
	Model                 string             `json:"model"`
	CWD                   string             `json:"cwd"`
	RuntimeWorkspaceRoots []string           `json:"runtime_workspace_roots"`
	ApprovalPolicy        string             `json:"approval_policy"`
	ApprovalsReviewer     string             `json:"approvals_reviewer"`
	Sandbox               CodexRemoteSandbox `json:"sandbox"`
}

type codexThreadResumeResponse struct {
	ActivePermissionProfile json.RawMessage      `json:"activePermissionProfile"`
	ApprovalPolicy          json.RawMessage      `json:"approvalPolicy"`
	ApprovalsReviewer       string               `json:"approvalsReviewer"`
	CWD                     string               `json:"cwd"`
	Model                   string               `json:"model"`
	ModelProvider           string               `json:"modelProvider"`
	RuntimeWorkspaceRoots   []string             `json:"runtimeWorkspaceRoots"`
	Sandbox                 codexSandboxResponse `json:"sandbox"`
	Thread                  codexThread          `json:"thread"`
}

type codexSandboxResponse struct {
	ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
	ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
	NetworkAccess       bool     `json:"networkAccess"`
	Type                string   `json:"type"`
	WritableRoots       []string `json:"writableRoots"`
}

func (response *codexSandboxResponse) UnmarshalJSON(payload []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err
	}
	allowed := map[string]struct{}{
		"excludeSlashTmp": {}, "excludeTmpdirEnvVar": {}, "networkAccess": {}, "type": {}, "writableRoots": {},
	}
	for key, raw := range fields {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("codex sandbox returned unsupported field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("codex sandbox field %q was null", key)
		}
	}
	type plain codexSandboxResponse
	var parsed plain
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return err
	}
	*response = codexSandboxResponse(parsed)
	return nil
}

// PrepareRemoteView performs the explicit native preparation needed before a
// Codex remote UI attaches. Unlike Inspect, this resumes the exact enrolled
// thread and therefore is a mutating operation.
func (adapter CodexAdapter) PrepareRemoteView(ctx context.Context, worker Worker) (CodexRemoteView, error) {
	var view CodexRemoteView
	err := adapter.withCodexRPC(ctx, worker, func(ctx context.Context, client *codexRPCClient) error {
		if err := codexInitialize(ctx, client); err != nil {
			return err
		}
		thread, err := codexReadThread(ctx, client, worker)
		if err != nil {
			return err
		}
		if thread.ModelProvider != codexSupportedModelProvider {
			return errors.New("codex thread uses an unsupported model provider")
		}
		resumeResult, err := client.call(ctx, "thread/resume", map[string]any{
			"threadId":     worker.ThreadID,
			"excludeTurns": true,
		})
		if err != nil {
			return err
		}
		var response codexThreadResumeResponse
		if err := json.Unmarshal(resumeResult, &response); err != nil {
			return errors.New("codex thread/resume returned malformed metadata")
		}
		view, err = validateCodexRemoteView(worker, thread, response)
		return err
	})
	return view, err
}

func validateCodexRemoteView(worker Worker, read codexThread, response codexThreadResumeResponse) (CodexRemoteView, error) {
	if read.ModelProvider != codexSupportedModelProvider || response.ModelProvider != codexSupportedModelProvider {
		return CodexRemoteView{}, errors.New("codex remote view uses an unsupported model provider")
	}
	if response.Thread.ID != worker.ThreadID || response.Thread.ID != read.ID || response.Thread.CWD != worker.Worktree || response.Thread.CWD != read.CWD || response.Thread.Model != read.Model || response.Thread.ModelProvider != read.ModelProvider {
		return CodexRemoteView{}, errors.New("codex thread/resume metadata does not match the enrolled worker")
	}
	if response.CWD != worker.Worktree || response.CWD != read.CWD || response.Model != read.Model || response.ModelProvider != read.ModelProvider || !safeCodexToken(response.Model) {
		return CodexRemoteView{}, errors.New("codex remote view metadata does not match the enrolled worker")
	}
	approvalPolicy, err := codexApprovalPolicy(response.ApprovalPolicy)
	if err != nil {
		return CodexRemoteView{}, err
	}
	if response.ApprovalsReviewer != "user" {
		return CodexRemoteView{}, errors.New("codex remote view uses an unsupported approvals reviewer")
	}
	if len(response.ActivePermissionProfile) != 0 && string(response.ActivePermissionProfile) != "null" {
		return CodexRemoteView{}, errors.New("codex remote view uses an active permission profile")
	}
	if err := validateCodexRoots(response.RuntimeWorkspaceRoots, response.CWD, true, "runtime workspace roots"); err != nil {
		return CodexRemoteView{}, err
	}
	sandbox, err := validateCodexSandbox(response.Sandbox, response.CWD)
	if err != nil {
		return CodexRemoteView{}, err
	}
	return CodexRemoteView{
		NativeSessionID:       response.Thread.ID,
		Provider:              response.ModelProvider,
		Model:                 response.Model,
		CWD:                   response.CWD,
		RuntimeWorkspaceRoots: cloneCodexStrings(response.RuntimeWorkspaceRoots),
		ApprovalPolicy:        approvalPolicy,
		ApprovalsReviewer:     response.ApprovalsReviewer,
		Sandbox:               sandbox,
	}, nil
}

func codexApprovalPolicy(raw json.RawMessage) (string, error) {
	var policy string
	if len(raw) == 0 || json.Unmarshal(raw, &policy) != nil {
		return "", errors.New("codex remote view returned an unsupported approval policy")
	}
	switch policy {
	case "never", "on-request":
		return policy, nil
	default:
		return "", errors.New("codex remote view returned an unsupported approval policy")
	}
}

func validateCodexSandbox(response codexSandboxResponse, cwd string) (CodexRemoteSandbox, error) {
	if response.NetworkAccess {
		return CodexRemoteSandbox{}, errors.New("codex remote view enables network access")
	}
	sandbox := CodexRemoteSandbox{
		Type:                response.Type,
		NetworkAccess:       response.NetworkAccess,
		ExcludeTmpdirEnvVar: response.ExcludeTmpdirEnvVar,
		ExcludeSlashTmp:     response.ExcludeSlashTmp,
	}
	switch response.Type {
	case "readOnly":
		if len(response.WritableRoots) != 0 || response.ExcludeTmpdirEnvVar || response.ExcludeSlashTmp {
			return CodexRemoteSandbox{}, errors.New("codex read-only sandbox has write posture")
		}
		sandbox.Type = "read-only"
	case "workspaceWrite":
		if err := validateCodexRoots(response.WritableRoots, cwd, false, "sandbox writable roots"); err != nil {
			return CodexRemoteSandbox{}, err
		}
		sandbox.Type = "workspace-write"
		sandbox.WritableRoots = cloneCodexStrings(response.WritableRoots)
	case "dangerFullAccess", "externalSandbox":
		return CodexRemoteSandbox{}, errors.New("codex remote view uses an unsafe sandbox")
	default:
		return CodexRemoteSandbox{}, errors.New("codex remote view returned an unsupported sandbox")
	}
	return sandbox, nil
}

func validateCodexRoots(roots []string, cwd string, requireCWD bool, label string) error {
	if len(roots) > codexRemoteMaxRoots {
		return fmt.Errorf("codex remote view returned too many %s", label)
	}
	if len(roots) == 0 && requireCWD {
		return fmt.Errorf("codex remote view returned no %s", label)
	}
	seen := make(map[string]struct{}, len(roots))
	containsCWD := false
	for _, root := range roots {
		if root == "" || len(root) > codexRemoteMaxRootBytes || !utf8.ValidString(root) || !filepath.IsAbs(root) || filepath.Clean(root) != root || hasCodexPathControl(root) {
			return fmt.Errorf("codex remote view returned an invalid %s", label)
		}
		if _, ok := seen[root]; ok {
			return fmt.Errorf("codex remote view returned duplicate %s", label)
		}
		seen[root] = struct{}{}
		if root == cwd {
			containsCWD = true
		}
	}
	if requireCWD && !containsCWD {
		return fmt.Errorf("codex remote view %s do not include the enrolled cwd", label)
	}
	return nil
}

func hasCodexPathControl(path string) bool {
	for _, character := range path {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return true
		}
	}
	return false
}

// Equal reports strict equality, including root order and sandbox exclusions.
func (view CodexRemoteView) Equal(other CodexRemoteView) bool {
	return view.NativeSessionID == other.NativeSessionID && view.Provider == other.Provider && view.Model == other.Model && view.CWD == other.CWD && view.ApprovalPolicy == other.ApprovalPolicy && view.ApprovalsReviewer == other.ApprovalsReviewer && equalCodexStrings(view.RuntimeWorkspaceRoots, other.RuntimeWorkspaceRoots) && view.Sandbox.Equal(other.Sandbox)
}

// EqualCodexRemoteViews reports strict equality of two prepared remote views.
func EqualCodexRemoteViews(left CodexRemoteView, right CodexRemoteView) bool {
	return left.Equal(right)
}

// Equal reports strict equality, including root order and exclusions.
func (sandbox CodexRemoteSandbox) Equal(other CodexRemoteSandbox) bool {
	return sandbox.Type == other.Type && sandbox.NetworkAccess == other.NetworkAccess && sandbox.ExcludeTmpdirEnvVar == other.ExcludeTmpdirEnvVar && sandbox.ExcludeSlashTmp == other.ExcludeSlashTmp && equalCodexStrings(sandbox.WritableRoots, other.WritableRoots)
}

func equalCodexStrings(left []string, right []string) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneCodexStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}
