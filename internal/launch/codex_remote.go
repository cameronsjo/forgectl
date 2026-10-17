package launch

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/config"
)

// CodexRemoteSandbox is the native sandbox policy attached to a remote Codex
// session. It is deliberately separate from the mail surface's policy type.
type CodexRemoteSandbox struct {
	Type                string
	NetworkAccess       bool
	WritableRoots       []string
	ExcludeTmpdirEnvVar bool
	ExcludeSlashTmp     bool
}

// CodexRemoteRequest describes one already-authenticated native Codex
// session. The builder starts no process and makes no model request.
type CodexRemoteRequest struct {
	BinaryPath            string
	Home                  string
	CodexHome             string
	Socket                string
	NativeSessionID       string
	Model                 string
	Provider              string
	CWD                   string
	RuntimeWorkspaceRoots []string
	ApprovalPolicy        string
	ApprovalsReviewer     string
	Sandbox               CodexRemoteSandbox
}

const (
	remotePathMax  = 4096
	remoteRootsMax = 64
)

func cleanRemotePath(name, value string) (string, error) {
	if value == "" || len(value) > remotePathMax || !filepath.IsAbs(value) {
		return "", fmt.Errorf("launch: %s must be a bounded absolute path", name)
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("launch: %s must be valid UTF-8", name)
	}
	for _, runeValue := range value {
		if runeValue < 0x20 || runeValue == 0x7f || (runeValue >= 0x80 && runeValue < 0xa0) {
			return "", fmt.Errorf("launch: %s contains a terminal control", name)
		}
	}
	clean := filepath.Clean(value)
	if clean != value {
		return "", fmt.Errorf("launch: %s must be clean", name)
	}
	return clean, nil
}

func remoteWithin(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func normalizeRemoteSandboxType(raw string) (string, error) {
	switch raw {
	case "read-only", "readOnly":
		return "read-only", nil
	case "workspace-write", "workspaceWrite":
		return "workspace-write", nil
	default:
		return "", fmt.Errorf("launch: unsupported Codex sandbox %q", raw)
	}
}

func validateRemoteRequest(req CodexRemoteRequest) error {
	if _, err := cleanRemotePath("binary_path", req.BinaryPath); err != nil {
		return err
	}
	home, err := cleanRemotePath("home", req.Home)
	if err != nil {
		return err
	}
	codexHome, err := cleanRemotePath("codex_home", req.CodexHome)
	if err != nil {
		return err
	}
	if !remoteWithin(home, codexHome) || codexHome == home {
		return fmt.Errorf("launch: codex_home must be a private child of home")
	}
	if _, err := cleanRemotePath("socket", req.Socket); err != nil {
		return err
	}
	cwd, err := cleanRemotePath("cwd", req.CWD)
	if err != nil {
		return err
	}
	if !validSessionID(req.NativeSessionID) {
		return fmt.Errorf("launch: native session id must be a lowercase UUID")
	}
	if req.Provider != "openai" {
		return fmt.Errorf("launch: unsupported Codex provider %q", req.Provider)
	}
	if err := config.CheckModelName(req.Model); err != nil {
		return fmt.Errorf("launch: unsupported Codex model %q: %w", req.Model, err)
	}
	if req.ApprovalPolicy != "never" && req.ApprovalPolicy != "on-request" {
		return fmt.Errorf("launch: unsupported Codex approval policy %q", req.ApprovalPolicy)
	}
	if req.ApprovalsReviewer != "user" {
		return fmt.Errorf("launch: unsupported Codex approvals reviewer %q", req.ApprovalsReviewer)
	}
	sandboxType, err := normalizeRemoteSandboxType(req.Sandbox.Type)
	if err != nil {
		return err
	}
	if req.Sandbox.NetworkAccess {
		return fmt.Errorf("launch: Codex remote network access must be disabled")
	}
	if sandboxType == "read-only" && len(req.Sandbox.WritableRoots) != 0 {
		return fmt.Errorf("launch: read-only Codex sandbox cannot have writable roots")
	}
	if sandboxType == "read-only" && (req.Sandbox.ExcludeTmpdirEnvVar || req.Sandbox.ExcludeSlashTmp) {
		return fmt.Errorf("launch: read-only Codex sandbox cannot set workspace-write exclusions")
	}
	seen := make(map[string]struct{}, len(req.RuntimeWorkspaceRoots))
	if len(req.RuntimeWorkspaceRoots) > remoteRootsMax {
		return fmt.Errorf("launch: too many runtime workspace roots")
	}
	foundCWD := false
	for _, root := range req.RuntimeWorkspaceRoots {
		cleanRoot, rootErr := cleanRemotePath("runtime workspace root", root)
		if rootErr != nil {
			return rootErr
		}
		if _, exists := seen[cleanRoot]; exists {
			return fmt.Errorf("launch: duplicate runtime workspace root %q", root)
		}
		seen[cleanRoot] = struct{}{}
		foundCWD = foundCWD || cleanRoot == cwd
	}
	if !foundCWD {
		return fmt.Errorf("launch: runtime workspace roots must contain cwd")
	}
	seen = make(map[string]struct{}, len(req.Sandbox.WritableRoots))
	if len(req.Sandbox.WritableRoots) > remoteRootsMax {
		return fmt.Errorf("launch: too many sandbox writable roots")
	}
	for _, root := range req.Sandbox.WritableRoots {
		cleanRoot, rootErr := cleanRemotePath("sandbox writable root", root)
		if rootErr != nil {
			return rootErr
		}
		if _, exists := seen[cleanRoot]; exists {
			return fmt.Errorf("launch: duplicate sandbox writable root %q", root)
		}
		seen[cleanRoot] = struct{}{}
	}
	return nil
}

func codexWritableRootsConfig(roots []string) string {
	quoted := make([]string, len(roots))
	for index, root := range roots {
		quoted[index] = strconv.Quote(root)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func codexRemoteArgs(req CodexRemoteRequest) []string {
	args := []string{
		"resume", req.NativeSessionID,
		"--remote", "unix://" + req.Socket,
		"--cd", req.CWD,
		"--sandbox", req.Sandbox.Type,
		"--ask-for-approval", req.ApprovalPolicy,
		"--config", `approvals_reviewer="user"`,
		"--config", "sandbox_workspace_write.network_access=false",
		"--config", "sandbox_workspace_write.writable_roots=" + codexWritableRootsConfig(req.Sandbox.WritableRoots),
		"--config", fmt.Sprintf("sandbox_workspace_write.exclude_tmpdir_env_var=%t", req.Sandbox.ExcludeTmpdirEnvVar),
		"--config", fmt.Sprintf("sandbox_workspace_write.exclude_slash_tmp=%t", req.Sandbox.ExcludeSlashTmp),
	}
	for _, root := range req.RuntimeWorkspaceRoots {
		if root != req.CWD {
			args = append(args, "--add-dir", root)
		}
	}
	return args
}

// BuildCodexRemoteInvocation builds an isolated invocation that attaches to an
// existing native Codex session over the supplied Unix socket.
func BuildCodexRemoteInvocation(req CodexRemoteRequest) (BuiltInvocation, error) {
	canonicalSandboxType, err := normalizeRemoteSandboxType(req.Sandbox.Type)
	if err != nil {
		return BuiltInvocation{}, err
	}
	req.Sandbox.Type = canonicalSandboxType
	if err := validateRemoteRequest(req); err != nil {
		return BuiltInvocation{}, err
	}
	baseEnv := []string{
		"PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C.UTF-8",
		"TERM=xterm-256color",
		"HOME=" + req.Home,
		"CODEX_HOME=" + req.CodexHome,
	}
	configScratch := config.LaunchConfig{Defaults: config.LaunchDefaults{
		Harness:        "codex",
		Model:          req.Model,
		ApprovalPolicy: req.ApprovalPolicy,
		Sandbox:        req.Sandbox.Type,
	}}
	built, err := BuildInvocation(InvocationRequest{
		Config:  configScratch,
		CWD:     req.CWD,
		BaseEnv: baseEnv,
		InjectedEnv: map[string]string{
			"XDG_CACHE_HOME":  filepath.Join(req.Home, ".cache"),
			"XDG_CONFIG_HOME": filepath.Join(req.Home, ".config"),
			"XDG_DATA_HOME":   filepath.Join(req.Home, ".local", "share"),
		},
		Resolve: func(harness string, _ config.LaunchDefaults) (ResolvedBinary, error) {
			if harness != "codex" {
				return ResolvedBinary{}, fmt.Errorf("launch: remote Codex resolver received %q", harness)
			}
			return ResolvedBinary{Path: req.BinaryPath, Source: BinaryCodexConfig}, nil
		},
	})
	if err != nil {
		return BuiltInvocation{}, err
	}
	built.Invocation.Args = codexRemoteArgs(req)
	return built, nil
}
