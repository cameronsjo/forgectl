package goalnative

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
)

func TestClaudeArgumentsDisableAmbientCapabilities(t *testing.T) {
	args := claudeArguments(NativeConfig{Model: "claude-sonnet-5-5"}, "session-id", "/private/control/mcp.json")
	joined := strings.Join(args, "\x00")
	for _, expected := range []string{
		"--input-format\x00stream-json",
		"--output-format\x00stream-json",
		"--permission-mode\x00dontAsk",
		"--permission-prompts\x00none",
		"--replay-user-messages",
		"--setting-sources\x00",
		"--strict-mcp-config",
		"--mcp-config\x00/private/control/mcp.json",
		"--tools\x00",
		"--allowedTools\x00mcp__goal__goal_exec,mcp__goal__goal_read,mcp__goal__goal_write,mcp__goal__goal_mail,mcp__goal__goal_inbox",
		"--disable-slash-commands",
		"--session-id\x00session-id",
		"--model\x00claude-sonnet-5-5",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("Claude arguments missing %q: %q", expected, args)
		}
	}
	if strings.Contains(joined, "--bare") || strings.Contains(joined, "--plugin-dir") {
		t.Fatalf("Claude arguments enable ambient auth/config capability: %q", args)
	}
}

func TestClaudeFinishPublishesCompletionBeforeWaitReturns(t *testing.T) {
	eventStarted := make(chan struct{})
	releaseEvent := make(chan struct{})
	eventDone := make(chan struct{})
	session := newClaudeTestSession()
	session.process = &JSONProcess{done: make(chan struct{})}
	session.config.OnEvent = func(event NativeEvent) {
		if event.Status != "completed" {
			return
		}
		close(eventStarted)
		<-releaseEvent
		close(eventDone)
	}
	prompt := NativePrompt{ID: "00112233445566778899aabbccddeeff", Text: "complete this"}
	session.active = &claudeTurn{prompt: prompt, nativeID: "native-prompt", consumed: true, done: make(chan struct{}), accepted: make(chan error, 1)}

	finishDone := make(chan struct{})
	go func() {
		session.finish(prompt.ID, TurnResult{ID: "native-result", Status: "completed", Text: "done"}, nil, "native-result")
		close(finishDone)
	}()
	<-eventStarted
	waitDone := make(chan struct{})
	var result TurnResult
	var waitErr error
	go func() {
		result, waitErr = session.Wait(context.Background(), prompt.ID)
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before the completion observer finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseEvent)
	<-eventDone
	<-finishDone
	<-waitDone
	if waitErr != nil || result.Status != "completed" || result.Text != "done" {
		t.Fatalf("Wait result = %#v, %v", result, waitErr)
	}
	if session.active != nil {
		t.Fatal("completed turn remained active")
	}
}

func TestWriteClaudeMCPConfigUsesCredentialFreeBrokerEnvironment(t *testing.T) {
	root := t.TempDir()
	path := root + "/mcp.json"
	config := NativeConfig{ControlDir: root, SupervisorBinary: "/private/bin/forgectl", ToolSocket: "/private/run/tool.sock", ToolCapabilityFile: "/private/run/capability.json"}
	if err := writeClaudeMCPConfig(path, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is a test-generated file below the owner-private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	var document claudeMCPConfig
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	server, ok := document.MCPServers[claudeMCPName]
	if !ok || server.Command != claudeEnvBinary || len(server.Env) != 0 {
		t.Fatalf("MCP server = %#v", server)
	}
	if len(server.Args) < 9 || server.Args[0] != "-i" || server.Args[1] != "PATH=/usr/bin:/bin" || server.Args[2] != "HOME="+config.ControlDir || server.Args[3] != config.SupervisorBinary {
		t.Fatalf("MCP command does not use the exact broker and clean environment: %#v", server.Args)
	}
	if !strings.Contains(strings.Join(server.Args, "\x00"), "goal\x00_tools") {
		t.Fatalf("MCP command is not the goal tool surface: %#v", server.Args)
	}
}

func TestClaudeResultRejectsErrorAndAcceptsCorrelatedCompletion(t *testing.T) {
	session := newClaudeTestSession()
	session.active = &claudeTurn{prompt: NativePrompt{ID: "prompt-one", Text: "one"}, nativeID: "native-prompt-one", consumed: true, done: make(chan struct{}), accepted: make(chan error, 1)}
	session.handleResult(json.RawMessage(`{"type":"result","subtype":"success","session_id":"foreign","uuid":"foreign-result","terminal_reason":"end_turn","is_error":false,"result":"wrong"}`))
	if session.active == nil || len(session.outcomes) != 0 {
		t.Fatal("foreign result settled the active request")
	}
	session.handleResult(json.RawMessage(`{"type":"result","subtype":"success","session_id":"native","uuid":"result-one","user_message_uuid":"native-prompt-one","result_index":0,"terminal_reason":"end_turn","is_error":false,"result":"final"}`))
	outcome, ok := session.outcomes["prompt-one"]
	if !ok || outcome.err != nil || outcome.result.ID != "result-one" || outcome.result.Text != "final" {
		t.Fatalf("completion outcome = %#v, present=%t", outcome, ok)
	}

	session.idle = make(chan struct{})
	session.active = &claudeTurn{prompt: NativePrompt{ID: "prompt-two", Text: "two"}, nativeID: "native-prompt-two", consumed: true, done: make(chan struct{}), accepted: make(chan error, 1)}
	session.handleResult(json.RawMessage(`{"type":"result","subtype":"success","session_id":"native","uuid":"stale-result","user_message_uuid":"native-prompt-two","result_index":0,"terminal_reason":"end_turn","is_error":false,"result":"stale"}`))
	if session.active == nil || len(session.outcomes) != 1 {
		t.Fatal("stale result settled a later request")
	}
	session.handleResult(json.RawMessage(`{"type":"result","subtype":"success","session_id":"native","uuid":"result-two","user_message_uuid":"native-prompt-two","result_index":1,"terminal_reason":"end_turn","is_error":true,"result":"failed"}`))
	outcome, ok = session.outcomes["prompt-two"]
	if !ok || outcome.err == nil || outcome.result.Status != "failed" {
		t.Fatalf("negative completion outcome = %#v, present=%t", outcome, ok)
	}
}

func TestClaudeReplayAcceptanceRequiresExactPromptIdentity(t *testing.T) {
	session := newClaudeTestSession()
	turn := &claudeTurn{prompt: NativePrompt{ID: "prompt", Text: "exact text"}, nativeID: "native-prompt", done: make(chan struct{}), accepted: make(chan error, 1)}
	session.active = turn
	session.handleUserReplay("foreign", "native-prompt", true, "user", "exact text")
	session.handleUserReplay("native", "wrong", true, "user", "exact text")
	session.handleUserReplay("native", "native-prompt", true, "user", "different text")
	if turn.consumed {
		t.Fatal("invalid replay was accepted")
	}
	session.handleUserReplay("native", "native-prompt", true, "user", "exact text")
	if !turn.consumed {
		t.Fatal("exact replay was not accepted")
	}
	select {
	case err := <-turn.accepted:
		if err != nil {
			t.Fatalf("accepted replay returned error: %v", err)
		}
	default:
		t.Fatal("accepted replay did not produce acceptance receipt")
	}
}

func TestSanitizeClaudeEventRemovesCredentialFields(t *testing.T) {
	raw := json.RawMessage(`{"type":"result","session_id":"native","apiKey":"secret","nested":{"authorization":"bearer secret","value":"keep"}}`)
	safe := string(sanitizeClaudeEvent(raw))
	if strings.Contains(safe, "secret") || strings.Contains(safe, "authorization") || strings.Contains(safe, "apiKey") {
		t.Fatalf("credential field survived sanitization: %s", safe)
	}
	if !strings.Contains(safe, "keep") {
		t.Fatalf("ordinary event field was removed: %s", safe)
	}
}

func TestAssertClaudeMCPRefusesUnexpectedServers(t *testing.T) {
	if err := assertClaudeMCP(json.RawMessage(`{"mcpServers":[{"name":"goal","status":"connected","tools":[{"name":"goal_exec"},{"name":"goal_read"},{"name":"goal_write"},{"name":"goal_mail"},{"name":"goal_inbox"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"mcpServers":[]}`,
		`{"mcpServers":[{"name":"goal","status":"failed","tools":[]}]}`,
		`{"mcpServers":[{"name":"goal","status":"connected"}]}`,
		`{"mcpServers":[{"name":"other"}]}`,
		`{"mcpServers":{"other":{}}}`,
	} {
		if err := assertClaudeMCP(json.RawMessage(raw)); err == nil {
			t.Fatalf("unexpected MCP status accepted: %s", raw)
		}
	}
}

func TestClaudeSystemInitAttestationUsesMCPObjectsAndPrefixedTools(t *testing.T) {
	session := newClaudeTestSession()
	session.config.Cwd = "/worktree"
	session.handleSystemInit(json.RawMessage(`{"type":"system","subtype":"init","session_id":"native","cwd":"/worktree","model":"claude-sonnet-5-5","permissionMode":"dontAsk","tools":["mcp__goal__goal_exec","mcp__goal__goal_read","mcp__goal__goal_write","mcp__goal__goal_mail","mcp__goal__goal_inbox"],"mcp_servers":[{"name":"goal","status":"connected","source":"dynamic"}],"plugins":[{"path":"builtin","source":"cc-plugin-telemetry@builtin"}]}`))
	if session.systemErr != nil || session.Identity().SessionID != "native" {
		t.Fatalf("system init attestation = identity=%#v error=%v", session.Identity(), session.systemErr)
	}
}

func TestValidateClaudeEnvironmentRejectsProviderOverrides(t *testing.T) {
	for _, environment := range [][]string{
		{"CLAUDE_CODE_USE_BEDROCK=1"},
		{"CLAUDE_CODE_USE_VERTEX=true"},
		{"ANTHROPIC_BASE_URL=https://gateway.example.test"},
	} {
		if err := validateClaudeEnvironment(environment); err == nil {
			t.Fatalf("provider override accepted: %v", environment)
		}
	}
	if err := validateClaudeEnvironment([]string{"ANTHROPIC_BASE_URL=https://api.anthropic.com"}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeRealStartupNoPrompt(t *testing.T) {
	if os.Getenv("FORGECTL_REAL_CLAUDE_TEST") != "1" {
		t.Skip("set FORGECTL_REAL_CLAUDE_TEST=1 to run the credentialed no-prompt Claude startup probe")
	}
	claudeBinary := envOr("FORGECTL_CLAUDE_BINARY", "/Users/cameron/.local/bin/claude")
	supervisorBinary := envOr("FORGECTL_CLAUDE_SUPERVISOR", "/private/tmp/forgectl-native-teams-dev")
	for name, path := range map[string]string{"Claude binary": claudeBinary, "supervisor binary": supervisorBinary} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s is unavailable at %s: %v", name, path, err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := os.Getenv("FORGECTL_CLAUDE_ARTIFACT_DIR")
	if artifactDir != "" && (!filepath.IsAbs(artifactDir) || filepath.Clean(artifactDir) != artifactDir) {
		t.Fatalf("artifact directory must be absolute and clean: %q", artifactDir)
	}
	if artifactDir == "" {
		artifactDir, err = os.MkdirTemp("/private/tmp", "forgectl-claude-startup-probe.")
		if err != nil {
			t.Fatal(err)
		}
	} else if err := os.MkdirAll(artifactDir, 0o700); err != nil { //nolint:gosec // artifactDir is validated absolute and clean
		t.Fatal(err)
	}
	if !filepath.IsAbs(artifactDir) || filepath.Clean(artifactDir) != artifactDir {
		t.Fatalf("artifact directory must be absolute and clean: %q", artifactDir)
	}
	if err := ensurePrivateDir(artifactDir); err != nil {
		t.Fatal(err)
	}
	t.Logf("Claude startup probe artifacts: %s", artifactDir)
	eventFile, err := os.OpenFile(filepath.Join(artifactDir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // artifactDir is validated owner-private test output
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eventFile.Close() }()
	var eventMu sync.Mutex
	onRawEvent := func(raw json.RawMessage) {
		eventMu.Lock()
		defer eventMu.Unlock()
		_, _ = eventFile.Write(append(append([]byte(nil), raw...), '\n'))
	}
	controlDir := filepath.Join(artifactDir, "control")
	toolDir := filepath.Join(artifactDir, "tools")
	for _, directory := range []string{controlDir, toolDir} {
		if err := os.Mkdir(directory, 0o700); err != nil { //nolint:gosec // directory is below validated owner-private artifactDir
			t.Fatal(err)
		}
	}
	toolSocket := filepath.Join(toolDir, "tools.sock")
	capabilityFile := filepath.Join(toolDir, "capability")
	toolServer, err := goaltools.Start(t.Context(), toolSocket, capabilityFile, func(_ context.Context, operation string, _ json.RawMessage) (any, error) {
		return map[string]string{"operation": operation, "status": "harmless probe"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := toolServer.Close(closeCtx); err != nil {
			t.Errorf("close probe MCP broker: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var events []NativeEvent
	var eventsMu sync.Mutex
	session, err := StartClaude(ctx, NativeConfig{
		Binary:             claudeBinary,
		SupervisorBinary:   supervisorBinary,
		ControlDir:         controlDir,
		Cwd:                cwd,
		Socket:             filepath.Join(controlDir, "claude.sock"),
		Provider:           "anthropic",
		Model:              "claude-opus-5-5",
		ToolSocket:         toolSocket,
		ToolCapabilityFile: capabilityFile,
		OnEvent: func(event NativeEvent) {
			eventsMu.Lock()
			events = append(events, event)
			eventsMu.Unlock()
		},
		OnRawEvent: onRawEvent,
	})
	if err != nil {
		t.Fatalf("real Claude startup failed; artifacts=%s: %v", artifactDir, err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	closeErr := session.Close(closeCtx)
	closeCancel()
	if closeErr != nil {
		t.Fatalf("real Claude no-prompt cleanup failed; artifacts=%s: %v", artifactDir, closeErr)
	}

	eventsMu.Lock()
	startupFound := false
	for _, event := range events {
		if event.Kind == "startup" && event.Status == "initialized" && event.Identity.Model == "claude-opus-5-5" {
			startupFound = true
		}
		if event.Kind == "prompt" || event.Kind == "result" {
			eventsMu.Unlock()
			t.Fatalf("no-prompt probe emitted turn event %#v; artifacts=%s", event, artifactDir)
		}
	}
	eventsMu.Unlock()
	if !startupFound {
		t.Fatalf("real Claude startup did not emit the initialized event; artifacts=%s", artifactDir)
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func newClaudeTestSession() *claudeSession {
	return &claudeSession{
		identity:    NativeIdentity{Harness: "claude", SessionID: "native", Provider: "anthropic", Model: "claude-sonnet-5-5"},
		config:      NativeConfig{Cwd: "/worktree", Model: "claude-sonnet-5-5"},
		outcomes:    make(map[string]claudeOutcome),
		controls:    make(map[string]chan claudeControlResult),
		seenResults: make(map[string]struct{}),
		lastResult:  -1,
		idle:        make(chan struct{}),
		systemReady: make(chan struct{}),
	}
}
