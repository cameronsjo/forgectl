package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// onlyUnder makes the MCP lookups read files under root alone, so a
// .mcp.json above the test's temp dir on some host cannot change a result.
func onlyUnder(t *testing.T, root string) {
	t.Helper()
	prev := mcpFiles
	t.Cleanup(func() { mcpFiles = prev })
	mcpFiles = func(path string) []byte {
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return nil
		}
		return readMCPFile(path)
	}
}

func writeJSON(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPendingProjectMCPServers(t *testing.T) {
	found := []string{"obsidian", "homelab", "obsidian", "", "approved"}
	if got, left := pendingProjectMCPServers(found, mcpApproval{names: []string{"approved"}}); !slices.Equal(got, []string{"homelab", "obsidian"}) || left != 0 {
		t.Errorf("pending = %q (%d left), want homelab and obsidian, sorted, once each", got, left)
	}
	if got, left := pendingProjectMCPServers(found, mcpApproval{all: true}); got != nil || left != 0 {
		t.Errorf("enableAllProjectMcpServers still turned off %q (%d left)", got, left)
	}
	if got, left := pendingProjectMCPServers(nil, mcpApproval{}); got != nil || left != 0 {
		t.Errorf("no servers turned off %q (%d left)", got, left)
	}
}

// TestPendingProjectMCPServersIsBounded: the names are repository content
// riding one argv entry, so a long name and the names past the cap are left
// out and counted, and the worst-case document stays under the trampoline's
// per-entry cap.
func TestPendingProjectMCPServersIsBounded(t *testing.T) {
	var found []string
	for i := range maxMCPOffNames + 5 {
		found = append(found, fmt.Sprintf("s%03d", i))
	}
	found = append(found, strings.Repeat("x", maxMCPServerNameBytes+1))
	off, left := pendingProjectMCPServers(found, mcpApproval{})
	if len(off) != maxMCPOffNames || left != 6 {
		t.Fatalf("off %d names, left %d; want %d and 6", len(off), left, maxMCPOffNames)
	}
	if note := workerMCPOffNote(off, left); !strings.Contains(note, "6 more were not turned off") {
		t.Errorf("the note does not count what was left out: %q", note)
	}

	var worst []string
	for i := range maxMCPOffNames {
		worst = append(worst, fmt.Sprintf("%03d", i)+strings.Repeat("\x01", maxMCPServerNameBytes-3))
	}
	doc, err := withMCPOff(workerClaudeEditSettings, worst)
	if err != nil {
		t.Fatal(err)
	}
	if const128K := 128 << 10; len(doc) >= const128K {
		t.Errorf("worst-case --settings is %d bytes, over the trampoline's %d", len(doc), const128K)
	}
}

// mcpLayout is forgectl#1456's repo: a .mcp.json at the top and one in
// .claude/, and a worker worktree below both with its own .mcp.json.
func mcpLayout(t *testing.T) (root, worktree, home string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	onlyUnder(t, root)
	repo := filepath.Join(root, "The Compendium")
	worktree = filepath.Join(repo, ".claude", "worktrees", "w")
	home = filepath.Join(root, "home")
	writeJSON(t, filepath.Join(repo, ".mcp.json"), `{"mcpServers":{"obsidian-mcp-server":{"command":"x"}}}`)
	writeJSON(t, filepath.Join(repo, ".claude", ".mcp.json"), `{"mcpServers":{"homelab-mcp":{"command":"x"},"kept":{"command":"x"}}}`)
	writeJSON(t, filepath.Join(worktree, ".mcp.json"), `{"mcpServers":{"obsidian-mcp-server":{"command":"x"}}}`)
	writeJSON(t, filepath.Join(root, "broken", ".mcp.json"), `{not json`)
	if err := os.MkdirAll(home, 0o750); err != nil {
		t.Fatal(err)
	}
	return root, worktree, home
}

func TestProjectMCPServerNamesWalksToTheRoot(t *testing.T) {
	root, worktree, _ := mcpLayout(t)
	got := projectMCPServerNames(worktree)
	want := []string{"obsidian-mcp-server", "homelab-mcp", "kept", "obsidian-mcp-server"}
	if !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q (the worktree's, then each parent's)", got, want)
	}
	if got := projectMCPServerNames(filepath.Join(root, "broken")); got != nil {
		t.Errorf("an unparsable .mcp.json named %q", got)
	}
}

func TestProjectMCPApprovalReadsUserProjectAndLocal(t *testing.T) {
	_, worktree, home := mcpLayout(t)
	cfg := filepath.Join(home, ".claude")
	writeJSON(t, filepath.Join(cfg, "settings.json"), `{"enabledMcpjsonServers":["kept"]}`)
	writeJSON(t, filepath.Join(worktree, ".claude", "settings.json"), `{"enabledMcpjsonServers":["p"]}`)
	writeJSON(t, filepath.Join(worktree, ".claude", "settings.local.json"), `{"enabledMcpjsonServers":["l"]}`)
	a := projectMCPApproval(worktree, cfg)
	if a.all || !slices.Equal(a.names, []string{"kept", "p", "l"}) {
		t.Errorf("approval = %+v, want kept, p, l from user, project and local", a)
	}
	if a := projectMCPApproval(worktree, ""); slices.Contains(a.names, "kept") {
		t.Errorf("an unknown config dir still read user settings: %+v", a)
	}
	writeJSON(t, filepath.Join(cfg, "settings.json"), `{"enableAllProjectMcpServers":true}`)
	if a := projectMCPApproval(worktree, cfg); !a.all {
		t.Errorf("enableAllProjectMcpServers in user settings was not read: %+v", a)
	}
}

func TestWorkerClaudeConfigDir(t *testing.T) {
	for want, env := range map[string][]string{
		"/c":         {"HOME=/h", "CLAUDE_CONFIG_DIR=/c"},
		"/h/.claude": {"HOME=/h"},
		"":           {"HOME=relative", "CLAUDE_CONFIG_DIR=also-relative"},
	} {
		if got := workerClaudeConfigDir(env); got != want {
			t.Errorf("workerClaudeConfigDir(%q) = %q, want %q", env, got, want)
		}
	}
	// A relative CLAUDE_CONFIG_DIR is not resolved against HOME's default.
	if got := workerClaudeConfigDir([]string{"HOME=/h", "CLAUDE_CONFIG_DIR=rel"}); got != "" {
		t.Errorf("a relative CLAUDE_CONFIG_DIR read %q, want unknown", got)
	}
}

// buildMCPWorker builds a worker at worktree with home as its HOME.
func buildMCPWorker(t *testing.T, harness, worktree, home string, worker bool) BuiltInvocation {
	t.Helper()
	built, err := BuildInvocation(InvocationRequest{
		StdoutTerminal: true,
		Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: harness, PermissionMode: "plan"}},
		CWD:            worktree, Worker: worker,
		BaseEnv: []string{"HOME=" + home, "PATH=/usr/bin"},
		Resolve: fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH}),
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	return built
}

// TestClaudeWorkerTurnsOffUnapprovedProjectMCPServers is forgectl#1456: a
// claude worker's --settings names every project MCP server above its
// worktree that the operator has not approved, so Claude Code never opens
// the approval dialog, and a note names them. An approved server stays on.
func TestClaudeWorkerTurnsOffUnapprovedProjectMCPServers(t *testing.T) {
	_, worktree, home := mcpLayout(t)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), `{"enabledMcpjsonServers":["kept"]}`)

	built := buildMCPWorker(t, "claude", worktree, home, true)
	var doc struct {
		Off  []string `json:"disabledMcpjsonServers"`
		Mode bool     `json:"useAutoModeDuringPlan"`
	}
	if err := json.Unmarshal([]byte(settingsArg(built.Invocation.Args)), &doc); err != nil {
		t.Fatalf("--settings does not parse: %v", err)
	}
	if want := []string{"homelab-mcp", "obsidian-mcp-server"}; !slices.Equal(doc.Off, want) {
		t.Errorf("disabledMcpjsonServers = %q, want %q", doc.Off, want)
	}
	if !slices.ContainsFunc(built.Notes, func(n string) bool {
		return strings.Contains(n, "homelab-mcp, obsidian-mcp-server") && !strings.Contains(n, "kept")
	}) {
		t.Errorf("no note naming the servers turned off: %q", built.Notes)
	}

	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), `{"enableAllProjectMcpServers":true}`)
	if got := settingsArg(buildMCPWorker(t, "claude", worktree, home, true).Invocation.Args); strings.Contains(got, "disabledMcpjsonServers") {
		t.Errorf("with every project server approved, the worker still turned some off: %s", got)
	}
}

// TestOnlyAClaudeWorkerTurnsOffProjectMCPServers: an operator's own launch
// and a codex worker get no such setting; the operator is there to answer.
func TestOnlyAClaudeWorkerTurnsOffProjectMCPServers(t *testing.T) {
	_, worktree, home := mcpLayout(t)
	for name, built := range map[string]BuiltInvocation{
		"operator launch": buildMCPWorker(t, "claude", worktree, home, false),
		"codex worker":    buildMCPWorker(t, "codex", worktree, home, true),
	} {
		if strings.Contains(strings.Join(built.Invocation.Args, " "), "disabledMcpjsonServers") || len(built.Notes) > 0 && strings.Contains(strings.Join(built.Notes, " "), "MCP") {
			t.Errorf("%s: argv %q notes %q name project MCP servers", name, built.Invocation.Args, built.Notes)
		}
	}
}

// TestWorkerSettingsWithMCPOffValidateAgainstTheSettingsSchema: the key
// rides the same --settings document, which one rejected key voids whole.
func TestWorkerSettingsWithMCPOffValidateAgainstTheSettingsSchema(t *testing.T) {
	schema := workerSettingsSchema(t)
	for _, base := range []string{workerClaudeSettings, workerClaudeEditSettings} {
		doc, err := withMCPOff(base, []string{"homelab-mcp", "a b", "x y"})
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal([]byte(doc), &instance); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Errorf("worker settings with disabledMcpjsonServers fail the settings schema: %v\n%s", err, doc)
		}
	}
	if doc, err := withMCPOff(workerClaudeSettings, nil); err != nil || doc != workerClaudeSettings {
		t.Errorf("no servers changed the settings: %s, %v", doc, err)
	}
}
