package goalnative

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	claudeMCPName       = "goal"
	claudeMCPConfigName = "claude-mcp.json"
	claudeEnvBinary     = "/usr/bin/env"
	claudeProcessWait   = 20 * time.Second
)

type claudeSession struct {
	config   NativeConfig
	process  *JSONProcess
	identity NativeIdentity

	mu             sync.Mutex
	closed         bool
	active         *claudeTurn
	outcomes       map[string]claudeOutcome
	controls       map[string]chan claudeControlResult
	seenResults    map[string]struct{}
	lastResult     int
	resultObserved bool
	idle           chan struct{}
	closeOnce      sync.Once
	closeErr       error
	lastOutcomeErr error
	systemReady    chan struct{}
	systemOnce     sync.Once
	systemErr      error
}

type claudeTurn struct {
	prompt    NativePrompt
	nativeID  string
	done      chan struct{}
	accepted  chan error
	consumed  bool
	finishing bool
}

type claudeOutcome struct {
	result TurnResult
	err    error
}

type claudeControlResult struct {
	response json.RawMessage
	err      error
}

type claudeControlRequest struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

type claudeMCPConfig struct {
	MCPServers map[string]claudeMCPServer `json:"mcpServers"`
}

type claudeMCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

type claudeSystemInit struct {
	Type       string                 `json:"type"`
	Subtype    string                 `json:"subtype"`
	SessionID  string                 `json:"session_id"`
	CWD        string                 `json:"cwd"`
	Model      string                 `json:"model"`
	Permission string                 `json:"permissionMode"`
	Tools      []string               `json:"tools"`
	MCPServers []claudeMCPObservation `json:"mcp_servers"`
	Plugins    []struct {
		Path   string `json:"path"`
		Source string `json:"source"`
	} `json:"plugins"`
}

type claudeMCPObservation struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type claudeInitResponse struct {
	Models []struct {
		Value         string `json:"value"`
		ResolvedModel string `json:"resolvedModel"`
	} `json:"models"`
	CurrentPermissionMode string   `json:"current_permission_mode"`
	SessionState          string   `json:"session_state"`
	Capabilities          []string `json:"capabilities"`
}

type claudeResult struct {
	Type             string   `json:"type"`
	Subtype          string   `json:"subtype"`
	SessionID        string   `json:"session_id"`
	UUID             string   `json:"uuid"`
	UserMessageUUID  string   `json:"user_message_uuid"`
	UserMessageUUIDs []string `json:"user_message_uuids"`
	Result           string   `json:"result"`
	TerminalReason   string   `json:"terminal_reason"`
	IsError          *bool    `json:"is_error"`
	ResultIndex      *int     `json:"result_index"`
}

func StartClaude(ctx context.Context, config NativeConfig) (NativeSession, error) {
	if err := validateClaudeConfig(config); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.ControlDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Claude control directory: %w", err)
	}
	if err := ensurePrivateDir(config.ControlDir); err != nil {
		return nil, err
	}

	sessionID, err := newClaudeUUID()
	if err != nil {
		return nil, fmt.Errorf("create Claude session identity: %w", err)
	}
	mcpPath := filepath.Join(config.ControlDir, claudeMCPConfigName)
	if err := writeClaudeMCPConfig(mcpPath, config); err != nil {
		return nil, err
	}

	session := &claudeSession{
		config:      config,
		identity:    NativeIdentity{Harness: "claude", SessionID: sessionID, Provider: "anthropic", Model: config.Model},
		outcomes:    make(map[string]claudeOutcome),
		controls:    make(map[string]chan claudeControlResult),
		seenResults: make(map[string]struct{}),
		lastResult:  -1,
		idle:        make(chan struct{}),
		systemReady: make(chan struct{}),
	}
	close(session.idle)

	arguments := claudeArguments(config, sessionID, mcpPath)
	process, err := StartJSONProcess(ctx, ProcessConfig{
		Binary:           config.Binary,
		SupervisorBinary: config.SupervisorBinary,
		ControlDir:       config.ControlDir,
		Cwd:              config.Cwd,
		Socket:           config.Socket,
		Arguments:        arguments,
		Environment:      append([]string(nil), config.Environment...),
		OnMessage:        session.handleMessage,
	})
	if err != nil {
		return nil, err
	}
	session.process = process

	if err := session.initialize(ctx, sessionID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), claudeProcessWait)
		closeErr := process.Close(cleanupCtx)
		cancel()
		if closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("%w: %v", ErrCleanupUnconfirmed, closeErr))
		}
		return nil, err
	}
	return session, nil
}

func (session *claudeSession) Identity() NativeIdentity {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.identity
}

func (session *claudeSession) Submit(ctx context.Context, prompt NativePrompt) error {
	if err := ValidatePrompt(prompt); err != nil {
		return err
	}
	nativeID, err := newClaudeUUID()
	if err != nil {
		return err
	}
	turn := &claudeTurn{prompt: prompt, nativeID: nativeID, done: make(chan struct{}), accepted: make(chan error, 1)}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return errors.New("claude session is closed")
	}
	if session.active != nil {
		session.mu.Unlock()
		return errors.New("claude turn already active")
	}
	if _, exists := session.outcomes[prompt.ID]; exists {
		session.mu.Unlock()
		return errors.New("claude prompt identity was already used")
	}
	session.active = turn
	session.idle = make(chan struct{})
	session.mu.Unlock()

	frame := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": prompt.Text,
		},
		"uuid":               nativeID,
		"parent_tool_use_id": nil,
	}
	if err := session.process.Write(ctx, frame); err != nil {
		session.finish(prompt.ID, TurnResult{ID: prompt.ID, Status: "failed"}, err, "")
		return err
	}
	session.mu.Lock()
	accepted := turn.accepted
	processDone := session.process.Done()
	session.mu.Unlock()
	select {
	case err := <-accepted:
		return err
	case <-processDone:
		return claudeProcessError(session.process)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (session *claudeSession) Wait(ctx context.Context, promptID string) (TurnResult, error) {
	if promptID == "" {
		return TurnResult{}, errors.New("claude request identity is required")
	}
	session.mu.Lock()
	if outcome, ok := session.outcomes[promptID]; ok {
		session.mu.Unlock()
		return outcome.result, outcome.err
	}
	active := session.active
	if active == nil || active.prompt.ID != promptID {
		session.mu.Unlock()
		return TurnResult{}, errors.New("claude request is not active")
	}
	done := active.done
	processDone := session.process.Done()
	session.mu.Unlock()

	select {
	case <-done:
		session.mu.Lock()
		outcome := session.outcomes[promptID]
		session.mu.Unlock()
		return outcome.result, outcome.err
	case <-processDone:
		session.mu.Lock()
		if outcome, ok := session.outcomes[promptID]; ok {
			session.mu.Unlock()
			return outcome.result, outcome.err
		}
		session.mu.Unlock()
		if err := session.process.Err(); err != nil {
			return TurnResult{ID: promptID, Status: "failed"}, err
		}
		return TurnResult{ID: promptID, Status: "failed"}, errors.New("claude process ended before its result")
	case <-ctx.Done():
		return TurnResult{}, ctx.Err()
	}
}

func (session *claudeSession) Turn(ctx context.Context, text string) (TurnResult, error) {
	prompt, err := NewPrompt(text)
	if err != nil {
		return TurnResult{}, err
	}
	if err := session.Submit(ctx, prompt); err != nil {
		return TurnResult{}, err
	}
	return session.Wait(ctx, prompt.ID)
}

func (session *claudeSession) WaitIdle(ctx context.Context) error {
	for {
		session.mu.Lock()
		idle := session.idle
		closed := session.closed
		lastOutcomeErr := session.lastOutcomeErr
		active := session.active != nil
		session.mu.Unlock()
		if closed {
			return errors.New("claude session is closed")
		}
		if !active {
			if lastOutcomeErr != nil {
				return lastOutcomeErr
			}
			return nil
		}
		select {
		case <-idle:
			continue
		case <-session.process.Done():
			if err := session.process.Err(); err != nil {
				return err
			}
			return errors.New("claude process ended before becoming idle")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (session *claudeSession) Close(ctx context.Context) error {
	var activeID string
	session.closeOnce.Do(func() {
		session.mu.Lock()
		session.closed = true
		if session.active != nil && !session.active.finishing {
			activeID = session.active.prompt.ID
		}
		session.mu.Unlock()
	})
	if activeID != "" {
		session.finish(activeID, TurnResult{ID: activeID, Status: "failed"}, errors.New("claude session closed"), "")
	}
	err := session.process.Close(ctx)
	session.mu.Lock()
	session.closeErr = err
	session.mu.Unlock()
	return err
}

func (session *claudeSession) initialize(ctx context.Context, requestedSessionID string) error {
	initRaw, err := session.control(ctx, map[string]any{"subtype": "initialize", "hooks": map[string]any{}})
	if err != nil {
		return fmt.Errorf("claude initialization: %w", err)
	}
	var initResponse claudeInitResponse
	if err := json.Unmarshal(initRaw, &initResponse); err != nil {
		return fmt.Errorf("decode Claude initialization: %w", err)
	}
	if initResponse.CurrentPermissionMode != "dontAsk" {
		return errors.New("claude did not retain dontAsk permission mode")
	}
	session.mu.Lock()
	observedSessionID := session.identity.SessionID
	session.mu.Unlock()
	if observedSessionID != requestedSessionID {
		return errors.New("claude reported a different native session identity")
	}

	if _, err := session.control(ctx, map[string]any{"subtype": "set_model", "model": session.config.Model}); err != nil {
		return fmt.Errorf("select Claude model: %w", err)
	}
	mcpRaw, err := session.control(ctx, map[string]any{"subtype": "mcp_status"})
	if err != nil {
		return fmt.Errorf("observe Claude MCP servers: %w", err)
	}
	if err := assertClaudeMCP(mcpRaw); err != nil {
		return err
	}
	contextRaw, err := session.control(ctx, map[string]any{"subtype": "get_context_usage"})
	if err != nil {
		return fmt.Errorf("observe Claude model context: %w", err)
	}
	var contextResponse struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(contextRaw, &contextResponse); err != nil {
		return fmt.Errorf("decode Claude model context: %w", err)
	}
	if contextResponse.Model == "" {
		return errors.New("claude context did not report its active model")
	}
	if contextResponse.Model != session.config.Model {
		return errors.New("claude context reported a different model")
	}
	session.emit(NativeEvent{Identity: session.Identity(), Kind: "startup", Status: "initialized", Text: fmt.Sprintf("model=%s tools=0 mcp=%s", session.config.Model, claudeMCPName), Evidence: "claude-stream-json", Settled: true})
	return nil
}

func (session *claudeSession) control(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	requestID, err := newClaudeRequestID()
	if err != nil {
		return nil, err
	}
	response := make(chan claudeControlResult, 1)
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil, errors.New("claude session is closed")
	}
	session.controls[requestID] = response
	session.mu.Unlock()
	frame := claudeControlRequest{Type: "control_request", RequestID: requestID, Request: request}
	if err := session.process.Write(ctx, frame); err != nil {
		session.mu.Lock()
		delete(session.controls, requestID)
		session.mu.Unlock()
		return nil, err
	}
	select {
	case result := <-response:
		return result.response, result.err
	case <-session.process.Done():
		return nil, claudeProcessError(session.process)
	case <-ctx.Done():
		session.mu.Lock()
		delete(session.controls, requestID)
		session.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (session *claudeSession) handleMessage(raw json.RawMessage) {
	safeRaw := sanitizeClaudeEvent(raw)
	if session.config.OnRawEvent != nil {
		session.config.OnRawEvent(safeRaw)
	}

	var envelope struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		UUID      string `json:"uuid"`
		IsReplay  bool   `json:"isReplay"`
		Message   struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	if envelope.SessionID != "" {
		session.mu.Lock()
		if session.identity.SessionID == "" {
			session.identity.SessionID = envelope.SessionID
		}
		session.mu.Unlock()
	}
	if envelope.Type == "control_response" {
		session.handleControlResponse(envelope.Response)
		return
	}
	if envelope.Type == "system" && envelope.Subtype == "init" {
		session.handleSystemInit(raw)
		return
	}
	if envelope.Type == "result" {
		session.handleResult(raw)
		return
	}
	if envelope.Type == "user" {
		session.handleUserReplay(envelope.SessionID, envelope.UUID, envelope.IsReplay, envelope.Message.Role, envelope.Message.Content)
	}
}

func (session *claudeSession) handleControlResponse(raw json.RawMessage) {
	var response struct {
		Subtype  string          `json:"subtype"`
		Request  string          `json:"request_id"`
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Request == "" {
		return
	}
	result := claudeControlResult{response: response.Response}
	if response.Subtype != "success" {
		result.err = errors.New("claude control request was refused")
		if len(response.Error) > 0 && string(response.Error) != "null" {
			result.err = fmt.Errorf("claude control request was refused: %s", compact(response.Error))
		}
	}
	session.mu.Lock()
	wait := session.controls[response.Request]
	delete(session.controls, response.Request)
	session.mu.Unlock()
	if wait != nil {
		wait <- result
	}
}

func (session *claudeSession) handleSystemInit(raw json.RawMessage) {
	var init claudeSystemInit
	var initErr error
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		initErr = fmt.Errorf("decode Claude system initialization: %w", err)
	} else if err := json.Unmarshal(raw, &init); err != nil {
		initErr = fmt.Errorf("decode Claude system initialization: %w", err)
	} else if init.SessionID == "" {
		initErr = errors.New("claude system initialization omitted session identity")
	} else if _, ok := fields["cwd"]; !ok || init.CWD != session.config.Cwd {
		initErr = errors.New("claude system initialization reported a different cwd")
	} else if _, ok := fields["model"]; !ok || init.Model != session.config.Model {
		initErr = errors.New("claude system initialization reported a different model")
	} else if _, ok := fields["permissionMode"]; !ok || init.Permission != "dontAsk" {
		initErr = errors.New("claude system initialization reported an unexpected permission mode")
	} else if _, ok := fields["tools"]; !ok || !claudeNativeToolsAreSafe(init.Tools) {
		initErr = errors.New("claude system initialization reported an unexpected tool")
	} else if _, ok := fields["mcp_servers"]; !ok || !claudeMCPListIsSafe(init.MCPServers) {
		initErr = errors.New("claude system initialization reported an unexpected MCP server")
	} else if _, ok := fields["plugins"]; !ok || !claudePluginsAreBuiltin(init.Plugins) {
		initErr = errors.New("claude system initialization reported a non-builtin plugin")
	}
	if initErr != nil {
		session.mu.Lock()
		session.systemErr = initErr
		activeID := ""
		if session.active != nil {
			activeID = session.active.prompt.ID
		}
		session.mu.Unlock()
		session.systemOnce.Do(func() { close(session.systemReady) })
		if activeID != "" {
			session.finish(activeID, TurnResult{ID: activeID, Status: "failed"}, initErr, "")
		}
		return
	}
	session.mu.Lock()
	if session.identity.SessionID == "" {
		session.identity.SessionID = init.SessionID
	} else if session.identity.SessionID != init.SessionID {
		session.systemErr = errors.New("claude system initialization reported a different native session identity")
	}
	systemErr := session.systemErr
	activeID := ""
	if systemErr != nil && session.active != nil {
		activeID = session.active.prompt.ID
	}
	session.mu.Unlock()
	session.systemOnce.Do(func() { close(session.systemReady) })
	if systemErr != nil {
		if activeID != "" {
			session.finish(activeID, TurnResult{ID: activeID, Status: "failed"}, systemErr, "")
		}
		return
	}
	session.emit(NativeEvent{Identity: session.Identity(), NativeID: init.SessionID, Kind: "startup", Status: "observed", Evidence: "claude-system-init", Settled: true})
}

func (session *claudeSession) handleUserReplay(sessionID, nativeID string, replay bool, role, content string) {
	session.mu.Lock()
	if session.active == nil || sessionID == "" || sessionID != session.identity.SessionID || !replay || role != "user" || content != session.active.prompt.Text || nativeID == "" || nativeID != session.active.nativeID || session.active.consumed {
		session.mu.Unlock()
		return
	}
	session.active.consumed = true
	requestID := session.active.prompt.ID
	accepted := session.active.accepted
	session.mu.Unlock()
	select {
	case accepted <- nil:
	default:
	}
	session.emit(NativeEvent{Identity: session.Identity(), RequestID: requestID, NativeID: nativeID, Kind: "prompt", Status: "accepted", InputSHA256: PromptDigest(content), Evidence: "claude-replayed-user"})
	session.emit(NativeEvent{Identity: session.Identity(), RequestID: requestID, NativeID: nativeID, Kind: "prompt", Status: "consumed", Evidence: "claude-replayed-user"})
}

func (session *claudeSession) handleResult(raw json.RawMessage) {
	var result claudeResult
	if json.Unmarshal(raw, &result) != nil || result.SessionID == "" {
		return
	}
	session.mu.Lock()
	if session.active == nil || result.SessionID != session.identity.SessionID {
		session.mu.Unlock()
		return
	}
	if !session.active.consumed || session.active.nativeID == "" || result.UUID == "" || result.ResultIndex == nil {
		promptID := session.active.prompt.ID
		session.mu.Unlock()
		session.finish(promptID, TurnResult{ID: promptID, Status: "failed"}, errors.New("claude result was not correlated to the replayed prompt"), result.UUID)
		return
	}
	if claudeResultHasPromptIdentity(result) && !claudeResultBelongsToPrompt(result, session.active.nativeID) {
		session.mu.Unlock()
		return
	}
	if !claudeResultHasPromptIdentity(result) {
		promptID := session.active.prompt.ID
		session.mu.Unlock()
		session.finish(promptID, TurnResult{ID: promptID, Status: "failed"}, errors.New("claude result omitted prompt identity"), result.UUID)
		return
	}
	if result.UUID != "" {
		if _, seen := session.seenResults[result.UUID]; seen {
			session.mu.Unlock()
			return
		}
		session.seenResults[result.UUID] = struct{}{}
	}
	if result.ResultIndex != nil {
		if session.resultObserved && *result.ResultIndex <= session.lastResult {
			session.mu.Unlock()
			return
		}
		session.resultObserved = true
		session.lastResult = *result.ResultIndex
	}
	promptID := session.active.prompt.ID
	if !result.IsErrorValueValid() || result.Subtype != "success" || !validClaudeTerminalReason(result.TerminalReason) || strings.TrimSpace(result.Result) == "" {
		err := claudeResultError(result)
		session.mu.Unlock()
		turnID := result.UUID
		if turnID == "" {
			turnID = promptID
		}
		session.finish(promptID, TurnResult{ID: turnID, Status: "failed"}, err, result.UUID)
		return
	}
	session.mu.Unlock()
	if result.UUID == "" {
		session.finish(promptID, TurnResult{ID: promptID, Status: "failed"}, errors.New("claude result omitted native identity"), "")
		return
	}
	session.finish(promptID, TurnResult{ID: result.UUID, Status: "completed", Text: result.Result}, nil, result.UUID)
}

func (session *claudeSession) finish(promptID string, result TurnResult, err error, nativeID string) {
	session.mu.Lock()
	if session.active == nil || session.active.prompt.ID != promptID || session.active.finishing {
		session.mu.Unlock()
		return
	}
	session.active.finishing = true
	identity := session.identity
	session.mu.Unlock()
	status := result.Status
	if err != nil {
		status = "failed"
	}
	session.emit(NativeEvent{Identity: identity, RequestID: promptID, NativeID: nativeID, Kind: "prompt", Status: status, Text: result.Text, Evidence: "claude-result", Settled: true})
	session.mu.Lock()
	session.finishLocked(promptID, result, err)
	session.mu.Unlock()
}

func (session *claudeSession) finishLocked(promptID string, result TurnResult, err error) {
	if session.active == nil || session.active.prompt.ID != promptID {
		return
	}
	if !session.active.consumed {
		acceptErr := err
		if acceptErr == nil {
			acceptErr = errors.New("claude turn settled without consuming the replayed prompt")
		}
		select {
		case session.active.accepted <- acceptErr:
		default:
		}
	}
	session.outcomes[promptID] = claudeOutcome{result: result, err: err}
	session.lastOutcomeErr = err
	close(session.active.done)
	session.active = nil
	close(session.idle)
}

func (session *claudeSession) emit(event NativeEvent) {
	if session.config.OnEvent != nil {
		session.config.OnEvent(event)
	}
}

func validateClaudeConfig(config NativeConfig) error {
	for name, value := range map[string]string{
		"Claude binary":        config.Binary,
		"supervisor binary":    config.SupervisorBinary,
		"control directory":    config.ControlDir,
		"cwd":                  config.Cwd,
		"socket":               config.Socket,
		"tool socket":          config.ToolSocket,
		"tool capability file": config.ToolCapabilityFile,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be absolute and clean", name)
		}
	}
	if config.Provider != "anthropic" {
		return fmt.Errorf("claude provider must be anthropic, got %q", config.Provider)
	}
	if err := validateClaudeEnvironment(config.Environment); err != nil {
		return err
	}
	if !safeToken(config.Model) {
		return errors.New("claude model is required and must be a safe token")
	}
	if info, err := os.Stat(config.Cwd); err != nil || !info.IsDir() {
		return errors.New("claude cwd must be an existing directory")
	}
	if _, err := os.Stat(claudeEnvBinary); err != nil {
		return fmt.Errorf("claude tool environment helper is unavailable: %w", err)
	}
	return nil
}

func validateClaudeEnvironment(environment []string) error {
	if environment == nil {
		environment = os.Environ()
	}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY":
			if strings.TrimSpace(value) != "" {
				return fmt.Errorf("claude provider route flag %s is not allowed for native anthropic", key)
			}
		case "ANTHROPIC_BASE_URL":
			if value == "" {
				continue
			}
			parsed, err := url.Parse(value)
			if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "api.anthropic.com" || parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
				return errors.New("anthropic base URL must be the direct Anthropic HTTPS endpoint")
			}
		}
	}
	return nil
}

func claudeArguments(config NativeConfig, sessionID, mcpPath string) []string {
	allowedTools := make([]string, 0, len(GoalToolNames))
	for _, tool := range GoalToolNames {
		allowedTools = append(allowedTools, "mcp__goal__"+tool)
	}
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--await-initialize",
		"--replay-user-messages",
		"--no-session-persistence",
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--mcp-config", mcpPath,
		"--tools", "",
		"--allowedTools", strings.Join(allowedTools, ","),
		"--disable-slash-commands",
		"--session-id", sessionID,
		"--model", config.Model,
	}
}

func writeClaudeMCPConfig(path string, config NativeConfig) error {
	value := claudeMCPConfig{MCPServers: map[string]claudeMCPServer{
		claudeMCPName: {
			Command: claudeEnvBinary,
			Args: []string{
				"-i", "PATH=/usr/bin:/bin",
				"HOME=" + config.ControlDir,
				config.SupervisorBinary,
				"surface", "project", "goal", "_tools",
				"--socket", config.ToolSocket,
				"--capability-file", config.ToolCapabilityFile,
			},
			Env: map[string]string{},
		},
	}}
	if err := writePrivateJSON(path, value); err != nil {
		return fmt.Errorf("write Claude MCP configuration: %w", err)
	}
	return nil
}

func assertClaudeMCP(raw json.RawMessage) error {
	var response struct {
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode Claude MCP status: %w", err)
	}
	if len(response.MCPServers) == 0 || string(response.MCPServers) == "null" {
		return errors.New("claude did not report its goal MCP server")
	}
	var list []struct {
		Name   string          `json:"name"`
		Status string          `json:"status"`
		Tools  json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(response.MCPServers, &list) == nil {
		if len(list) != 1 || list[0].Name != claudeMCPName || list[0].Status != "connected" {
			return errors.New("claude reported an unexpected MCP server")
		}
		if err := assertClaudeMCPTools(list[0].Tools); err != nil {
			return err
		}
		return nil
	}
	var table map[string]json.RawMessage
	if json.Unmarshal(response.MCPServers, &table) != nil || len(table) != 1 {
		return errors.New("claude reported an unexpected MCP server")
	}
	if _, ok := table[claudeMCPName]; !ok {
		return errors.New("claude did not report its goal MCP server")
	}
	if err := assertClaudeMCPToolsFromServer(table[claudeMCPName]); err != nil {
		return err
	}
	return nil
}

func assertClaudeMCPTools(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("claude MCP status omitted its tool set")
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return errors.New("claude MCP status has malformed tool metadata")
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !containsGoalTool(tool.Name) {
			return fmt.Errorf("claude MCP exposed an unexpected tool %q", boundedClaudeToken(tool.Name))
		}
		seen[tool.Name] = struct{}{}
	}
	if len(tools) != len(GoalToolNames) || len(seen) != len(GoalToolNames) {
		return errors.New("claude MCP did not expose the complete goal tool set")
	}
	return nil
}

func assertClaudeMCPToolsFromServer(raw json.RawMessage) error {
	var server struct {
		Status string          `json:"status"`
		Tools  json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(raw, &server) != nil {
		return errors.New("claude MCP status has malformed server metadata")
	}
	if server.Status != "connected" {
		return errors.New("claude MCP server is not connected")
	}
	return assertClaudeMCPTools(server.Tools)
}

func containsGoalTool(name string) bool {
	for _, expected := range GoalToolNames {
		if name == expected || name == "mcp__"+claudeMCPName+"__"+expected {
			return true
		}
	}
	return false
}

func claudeMCPListIsSafe(servers []claudeMCPObservation) bool {
	return len(servers) == 0 || (len(servers) == 1 && servers[0].Name == claudeMCPName && servers[0].Status == "connected")
}

func claudeNativeToolsAreSafe(tools []string) bool {
	if len(tools) != len(GoalToolNames) {
		return false
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !containsGoalTool(tool) || !strings.HasPrefix(tool, "mcp__"+claudeMCPName+"__") {
			return false
		}
		seen[tool] = struct{}{}
	}
	return len(seen) == len(GoalToolNames)
}

func claudePluginsAreBuiltin(plugins []struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}) bool {
	for _, plugin := range plugins {
		if plugin.Path != "builtin" && !strings.HasSuffix(plugin.Source, "@builtin") {
			return false
		}
	}
	return true
}

func validClaudeTerminalReason(reason string) bool {
	if reason == "" || reason == "api_error" || reason == "interrupted" || reason == "aborted" || reason == "cancelled" {
		return false
	}
	return !strings.HasPrefix(reason, "error_")
}

func (result claudeResult) IsErrorValueValid() bool {
	return result.IsError != nil && !*result.IsError
}

func claudeResultBelongsToPrompt(result claudeResult, nativeID string) bool {
	if result.UserMessageUUID == nativeID {
		return true
	}
	for _, candidate := range result.UserMessageUUIDs {
		if candidate == nativeID {
			return true
		}
	}
	return false
}

func claudeResultHasPromptIdentity(result claudeResult) bool {
	return result.UserMessageUUID != "" || len(result.UserMessageUUIDs) > 0
}

func claudeResultError(result claudeResult) error {
	if result.IsError == nil {
		return errors.New("claude result omitted is_error")
	}
	if *result.IsError {
		return fmt.Errorf("claude turn failed with terminal reason %s", boundedClaudeToken(result.TerminalReason))
	}
	if result.Subtype != "success" {
		return fmt.Errorf("claude turn ended with subtype %s", boundedClaudeToken(result.Subtype))
	}
	if !validClaudeTerminalReason(result.TerminalReason) {
		return fmt.Errorf("claude turn ended with terminal reason %s", boundedClaudeToken(result.TerminalReason))
	}
	return errors.New("claude turn completed without final text")
}

func boundedClaudeToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	if len(value) > 128 {
		return value[:128] + "..."
	}
	return value
}

func claudeProcessError(process *JSONProcess) error {
	if err := process.Err(); err != nil {
		return err
	}
	return errors.New("claude process ended before control response")
}

func newClaudeRequestID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "forgectl-" + hex.EncodeToString(data), nil
}

func newClaudeUUID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(data[0:4]), hex.EncodeToString(data[4:6]), hex.EncodeToString(data[6:8]), hex.EncodeToString(data[8:10]), hex.EncodeToString(data[10:])), nil
}

func sanitizeClaudeEvent(raw json.RawMessage) json.RawMessage {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	sanitizeClaudeValue(value)
	// termsafe:allow-raw-json redacted private native evidence, never terminal output
	data, err := json.Marshal(value)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return data
}

func sanitizeClaudeValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			lower := strings.ToLower(key)
			if lower == "account" || lower == "auth" || lower == "authentication" || lower == "token" || lower == "tokens" || lower == "apikey" || lower == "api_key" || lower == "apikeysource" || lower == "access_token" || lower == "credential" || lower == "credentials" || lower == "secret" || lower == "secrets" || lower == "authorization" {
				delete(current, key)
				continue
			}
			sanitizeClaudeValue(child)
		}
	case []any:
		for _, child := range current {
			sanitizeClaudeValue(child)
		}
	}
}
