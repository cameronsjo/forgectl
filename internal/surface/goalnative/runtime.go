package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/net/websocket"
)

const (
	protocolVersion       = "2.0"
	maxPayloadBytes       = 1 << 20
	maxEvents             = 256
	startupTimeout        = 15 * time.Second
	rpcTimeout            = 3 * time.Minute
	startupCleanupTimeout = 20 * time.Second
)

var ErrCleanupUnconfirmed = errors.New("native cleanup unconfirmed")

type Config struct {
	Binary             string
	SupervisorBinary   string
	ControlDir         string
	Cwd                string
	Socket             string
	Model              string
	Environment        []string
	WritableRoots      []string
	OnEvent            func(json.RawMessage)
	ExecutionOnly      bool
	OnPromptAccepted   func(string, string)
	ToolSocket         string
	ToolCapabilityFile string
}

type TurnResult struct {
	ID     string
	Status string
	Text   string
}

type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type Session struct {
	ThreadID string
	Model    string
	Provider string

	config        Config
	lease         io.Closer
	child         *os.Process
	childDone     chan struct{}
	childMu       sync.Mutex
	childErr      error
	conn          *websocket.Conn
	done          chan struct{}
	doneOnce      sync.Once
	closeOnce     sync.Once
	closed        atomic.Bool
	mu            sync.Mutex
	writeMu       sync.Mutex
	nextID        int64
	pending       map[int64]chan rpcReply
	events        []json.RawMessage
	turnMu        sync.Mutex
	turnIdle      chan struct{}
	turnOpen      bool
	turnActive    bool
	nativeBusy    map[string]struct{}
	latestTurn    string
	latestStatus  string
	turnText      map[string]string
	turnFinalText map[string]string
	readErr       error
	receipt       string
	toolCalls     map[string]struct{}
	toolSlots     chan struct{}
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type hostSpec struct {
	Binary        string   `json:"binary"`
	ControlDir    string   `json:"control_dir"`
	Cwd           string   `json:"cwd"`
	Socket        string   `json:"socket"`
	Model         string   `json:"model"`
	WritableRoots []string `json:"writable_roots"`
	Receipt       string   `json:"receipt"`
	ExecutionOnly bool     `json:"execution_only,omitempty"`
	Protocol      string   `json:"protocol,omitempty"`
	Arguments     []string `json:"arguments,omitempty"`
	BrokerTools   bool     `json:"broker_tools,omitempty"`
}

func Start(ctx context.Context, config Config) (*Session, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if !config.ExecutionOnly {
		if err := validateOpenAIEnvironment(config.Environment); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(config.ControlDir, 0o700); err != nil {
		return nil, fmt.Errorf("create goal control directory: %w", err)
	}
	if err := ensurePrivateDir(config.ControlDir); err != nil {
		return nil, err
	}
	if config.ExecutionOnly {
		environment, err := prepareExecutionEnvironment(config)
		if err != nil {
			return nil, err
		}
		config.Environment = environment
	}
	if err := validateSocketDestination(config.Socket); err != nil {
		return nil, err
	}
	receipt := filepath.Join(config.ControlDir, "guardian-receipt.json")
	spec := hostSpec{Binary: config.Binary, ControlDir: config.ControlDir, Cwd: config.Cwd, Socket: config.Socket, Model: config.Model, WritableRoots: append([]string(nil), config.WritableRoots...), Receipt: receipt, ExecutionOnly: config.ExecutionOnly, BrokerTools: config.ToolSocket != ""}
	specPath := filepath.Join(config.ControlDir, fmt.Sprintf("host-spec-%d.json", time.Now().UnixNano()))
	if err := writePrivateJSON(specPath, spec); err != nil {
		return nil, err
	}
	leaseReader, leaseWriter, err := os.Pipe()
	if err != nil {
		_ = os.Remove(specPath)
		return nil, fmt.Errorf("create supervisor lease: %w", err)
	}
	cmd, err := startSupervisor(config.SupervisorBinary, specPath, leaseReader, config.Cwd, config.Environment)
	if err != nil {
		_ = leaseReader.Close()
		_ = leaseWriter.Close()
		_ = os.Remove(specPath)
		return nil, err
	}
	_ = leaseReader.Close()
	childDone := make(chan struct{})
	session := &Session{config: config, lease: leaseWriter, child: cmd, childDone: childDone, done: make(chan struct{}), pending: make(map[int64]chan rpcReply), turnIdle: make(chan struct{}), nativeBusy: make(map[string]struct{}), turnText: make(map[string]string), turnFinalText: make(map[string]string), receipt: receipt}
	session.toolCalls, session.toolSlots = make(map[string]struct{}), make(chan struct{}, 8)
	go func() {
		state, waitErr := cmd.Wait()
		if waitErr == nil && (state == nil || !state.Success()) {
			exitCode := -1
			if state != nil {
				exitCode = state.ExitCode()
			}
			waitErr = fmt.Errorf("goal supervisor exited unsuccessfully (exit code %d)", exitCode)
		}
		session.childMu.Lock()
		session.childErr = waitErr
		session.childMu.Unlock()
		close(childDone)
	}()
	close(session.turnIdle)
	if err := waitForUnixSocket(ctx, config.Socket, cmd, startupTimeout); err != nil {
		return nil, startupFailure(session, err)
	}
	conn, err := dialUnixWebSocket(ctx, config.Socket)
	if err != nil {
		return nil, startupFailure(session, err)
	}
	session.conn = conn
	go session.readLoop(context.WithoutCancel(ctx))
	if _, err := session.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "forgectl-goal", "version": "dev"}, "capabilities": map[string]any{"experimentalApi": config.ToolSocket != ""}}); err != nil {
		return nil, startupFailure(session, err)
	}
	if err := session.notify(ctx, "initialized", map[string]any{}); err != nil {
		return nil, startupFailure(session, err)
	}
	configResult, err := session.Call(ctx, "config/read", map[string]any{"cwd": config.Cwd, "includeLayers": true})
	if err != nil {
		return nil, startupFailure(session, err)
	}
	if err := assertUntrustedConfig(configResult, config.Cwd); err != nil {
		return nil, startupFailure(session, err)
	}
	if err := assertOpenAIRouting(configResult); err != nil {
		return nil, startupFailure(session, err)
	}
	if err := assertNativeIsolation(configResult); err != nil {
		return nil, startupFailure(session, err)
	}
	if config.ExecutionOnly {
		return session, nil
	}
	threadParams := map[string]any{"cwd": config.Cwd, "model": config.Model, "modelProvider": "openai", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": "read-only", "ephemeral": false, "threadSource": "startup"}
	if config.ToolSocket != "" {
		if err := assertBrokerTools(configResult); err != nil {
			return nil, startupFailure(session, err)
		}
		threadParams["dynamicTools"] = dynamicGoalTools()
		threadParams["allowProviderModelFallback"] = false
	}
	thread, err := session.Call(ctx, "thread/start", threadParams)
	if err != nil {
		return nil, startupFailure(session, err)
	}
	var selection struct {
		Thread struct {
			ID, Model, ModelProvider, CWD string
			Ephemeral                     *bool
		} `json:"thread"`
		ID, Model, ModelProvider, CWD     string
		ApprovalPolicy, ApprovalsReviewer string
		Sandbox                           struct{ Type string } `json:"sandbox"`
	}
	if err := json.Unmarshal(thread, &selection); err != nil {
		return nil, startupFailure(session, fmt.Errorf("decode native thread: %w", err))
	}
	if selection.Thread.ID != "" {
		session.ThreadID, session.Model, session.Provider = selection.Thread.ID, selection.Thread.Model, selection.Thread.ModelProvider
	} else {
		session.ThreadID, session.Model, session.Provider = selection.ID, selection.Model, selection.ModelProvider
	}
	if session.ThreadID == "" || session.Model != config.Model || session.Provider != "openai" || selection.Model != config.Model || selection.ModelProvider != "openai" || selection.CWD != config.Cwd || selection.Thread.CWD != config.Cwd || selection.ApprovalPolicy != "never" || selection.ApprovalsReviewer != "user" || selection.Sandbox.Type != "readOnly" || selection.Thread.Ephemeral == nil || *selection.Thread.Ephemeral {
		return nil, startupFailure(session, errors.New("native thread did not honor the requested OpenAI model"))
	}
	return session, nil
}

func startupFailure(session *Session, cause error) error {
	cleanupContext, cancel := context.WithTimeout(context.Background(), startupCleanupTimeout)
	defer cancel()
	if cleanupErr := session.Close(cleanupContext); cleanupErr != nil {
		return errors.Join(cause, fmt.Errorf("%w: %v", ErrCleanupUnconfirmed, cleanupErr))
	}
	return cause
}

func (session *Session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "" || !safeToken(method) {
		return nil, errors.New("invalid native method")
	}
	if session.config.ExecutionOnly && !executionMethod(method) {
		return nil, errors.New("execution-only host rejects model, authentication and arbitrary RPC")
	}
	id := atomic.AddInt64(&session.nextID, 1)
	wait := make(chan rpcReply, 1)
	session.mu.Lock()
	if session.closed.Load() || session.conn == nil {
		session.mu.Unlock()
		return nil, errors.New("native session is closed")
	}
	session.pending[id] = wait
	session.mu.Unlock()
	if err := session.send(ctx, rpcEnvelope{JSONRPC: protocolVersion, ID: json.RawMessage(fmt.Sprintf("%d", id)), Method: method, Params: mustJSON(params)}); err != nil {
		session.removePending(id)
		return nil, err
	}
	select {
	case reply := <-wait:
		return reply.result, reply.err
	case <-ctx.Done():
		session.removePending(id)
		return nil, ctx.Err()
	case <-session.done:
		session.removePending(id)
		return nil, errors.New("native session closed")
	}
}

func (session *Session) notify(ctx context.Context, method string, params any) error {
	return session.send(ctx, rpcEnvelope{JSONRPC: protocolVersion, Method: method, Params: mustJSON(params)})
}

func (session *Session) send(ctx context.Context, envelope rpcEnvelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// termsafe:allow-raw-json native JSON-RPC protocol frame, never terminal output
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	session.mu.Lock()
	conn := session.conn
	session.mu.Unlock()
	if conn == nil || session.closed.Load() {
		return errors.New("native session is closed")
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	if session.closed.Load() {
		return errors.New("native session is closed")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(rpcTimeout)); err != nil {
		return err
	}
	if err := websocket.Message.Send(conn, string(data)); err != nil {
		return err
	}
	return nil
}

func validateSocketDestination(path string) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("socket directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("socket destination must not be a symlink")
		}
		return errors.New("socket destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect socket destination: %w", err)
	}
	return nil
}

func (session *Session) Turn(ctx context.Context, prompt string) (TurnResult, error) {
	if prompt == "" {
		return TurnResult{}, errors.New("native prompt is empty")
	}
	session.turnMu.Lock()
	if session.turnActive || len(session.nativeBusy) > 0 {
		session.turnMu.Unlock()
		return TurnResult{}, errors.New("native turn already active")
	}
	session.turnActive = true
	session.updateTurnIdleLocked()
	session.turnMu.Unlock()
	defer func() {
		session.turnMu.Lock()
		session.turnActive = false
		session.updateTurnIdleLocked()
		session.turnMu.Unlock()
	}()
	result, err := session.Call(ctx, "turn/start", map[string]any{"threadId": session.ThreadID, "cwd": session.config.Cwd, "approvalPolicy": "never", "sandboxPolicy": workspacePolicy(session.config.WritableRoots), "input": []map[string]string{{"type": "text", "text": prompt}}})
	if err != nil {
		return TurnResult{}, err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &started); err != nil {
		return TurnResult{}, err
	}
	turnID := started.Turn.ID
	if turnID == "" {
		turnID = started.ID
	}
	if turnID == "" {
		return TurnResult{}, errors.New("native turn has no identity")
	}
	if session.config.OnPromptAccepted != nil {
		session.config.OnPromptAccepted(turnID, prompt)
	}
	event, err := session.waitEvent(ctx, func(event rpcEnvelope) bool {
		return event.Method == "turn/completed" && eventHasTurn(event.Params, session.ThreadID, turnID)
	})
	if err != nil {
		return TurnResult{}, err
	}
	var params struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID, Status string
			Error      json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(event.Params, &params); err != nil {
		return TurnResult{}, err
	}
	if params.Turn.Status != "completed" {
		return TurnResult{ID: turnID, Status: params.Turn.Status}, nativeTurnFailure(params.Turn.Status, params.Turn.Error)
	}
	text := session.finalText(turnID)
	if strings.TrimSpace(text) == "" {
		return TurnResult{ID: turnID, Status: params.Turn.Status}, errors.New("native turn completed without final agent text")
	}
	return TurnResult{ID: turnID, Status: params.Turn.Status, Text: text}, nil
}

func nativeTurnFailure(status string, raw json.RawMessage) error {
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &detail) != nil || strings.TrimSpace(detail.Message) == "" {
		return fmt.Errorf("native turn ended with status %s", status)
	}
	message := strings.TrimSpace(detail.Message)
	if len(message) > 2048 {
		message = message[:2048] + " (truncated)"
	}
	return fmt.Errorf("native turn ended with status %s: %s", status, message)
}

func (session *Session) Exec(ctx context.Context, argv []string, cwd string, roots []string, timeout time.Duration) (CommandResult, error) {
	return session.exec(ctx, argv, cwd, roots, timeout, nil)
}

func (session *Session) exec(ctx context.Context, argv []string, cwd string, roots []string, timeout time.Duration, environment map[string]string) (CommandResult, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return CommandResult{}, errors.New("native command is empty")
	}
	for _, argument := range argv {
		if strings.ContainsRune(argument, 0) {
			return CommandResult{}, errors.New("native command contains a NUL byte")
		}
	}
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return CommandResult{}, errors.New("native command cwd must be absolute and clean")
	}
	if err := validateSandbox(cwd, roots); err != nil {
		return CommandResult{}, err
	}
	if timeout <= 0 {
		timeout = rpcTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timeoutMS := timeout.Milliseconds()
	if timeoutMS < 1 {
		timeoutMS = 1
	}
	request, err := NewPrompt("command execution")
	if err != nil {
		return CommandResult{}, err
	}
	params := map[string]any{"command": argv, "cwd": cwd, "sandboxPolicy": workspacePolicy(roots), "timeoutMs": timeoutMS, "processId": request.ID, "outputBytesCap": 128 * 1024}
	if len(environment) > 0 {
		params["env"] = environment
	}
	raw, err := session.Call(callCtx, "command/exec", params)
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		_, terminateErr := session.Call(cleanup, "command/exec/terminate", map[string]string{"processId": request.ID})
		stop()
		if terminateErr != nil {
			return CommandResult{}, errors.Join(err, fmt.Errorf("command termination unconfirmed: %w", terminateErr))
		}
		return CommandResult{}, err
	}
	var response struct {
		ExitCode *int    `json:"exitCode"`
		Stdout   *string `json:"stdout"`
		Stderr   *string `json:"stderr"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return CommandResult{}, fmt.Errorf("decode native command result: %w", err)
	}
	if response.ExitCode == nil || response.Stdout == nil || response.Stderr == nil {
		return CommandResult{}, errors.New("native command result is incomplete")
	}
	return CommandResult{ExitCode: *response.ExitCode, Stdout: *response.Stdout, Stderr: *response.Stderr}, nil
}

func (session *Session) WaitIdle(ctx context.Context) error {
	for {
		session.turnMu.Lock()
		idle := session.turnIdle
		busy := session.turnActive || len(session.nativeBusy) > 0
		latestTurn, latestStatus := session.latestTurn, session.latestStatus
		session.turnMu.Unlock()
		if !busy {
			if latestTurn != "" && latestStatus != "completed" {
				return fmt.Errorf("native turn %s ended with status %s", latestTurn, latestStatus)
			}
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		case <-session.done:
			return errors.New("native session closed")
		}
	}
}

func (session *Session) Events() []json.RawMessage {
	session.mu.Lock()
	defer session.mu.Unlock()
	result := make([]json.RawMessage, len(session.events))
	for index := range session.events {
		result[index] = append(json.RawMessage(nil), session.events[index]...)
	}
	return result
}

func (session *Session) Close(ctx context.Context) error {
	var result error
	childFinished := session.childDone == nil
	session.closeOnce.Do(func() {
		session.closed.Store(true)
		if session.conn != nil {
			_ = session.conn.Close()
		}
		if session.lease != nil {
			result = session.lease.Close()
		}
	})
	if session.childDone != nil {
		select {
		case <-session.childDone:
			childFinished = true
			session.childMu.Lock()
			childErr := session.childErr
			session.childMu.Unlock()
			if childErr != nil {
				result = errors.Join(result, childErr)
			}
			if receiptErr := validateGuardianReceipts(session.config.ControlDir, session.config.Socket); receiptErr != nil {
				result = errors.Join(result, receiptErr)
			}
		case <-ctx.Done():
			result = errors.Join(result, ctx.Err())
		}
	}
	if childFinished {
		session.signalDone()
	}
	return result
}

func (session *Session) signalDone() { session.doneOnce.Do(func() { close(session.done) }) }

func (session *Session) readLoop(ctx context.Context) {
	for {
		var data []byte
		if err := websocket.Message.Receive(session.conn, &data); err != nil {
			session.mu.Lock()
			session.readErr = err
			session.mu.Unlock()
			session.failPending(err)
			session.signalDone()
			return
		}
		if len(data) == 0 || len(data) > maxPayloadBytes {
			session.failPending(errors.New("native event exceeds payload limit"))
			session.signalDone()
			return
		}
		var event rpcEnvelope
		if err := json.Unmarshal(data, &event); err != nil {
			session.failPending(errors.New("native event is malformed"))
			session.signalDone()
			return
		}
		response := len(event.ID) > 0 && event.Method == ""
		if !response {
			session.recordEvent(data)
			session.observeTurn(event)
		}
		if event.Method != "" && len(event.ID) > 0 && len(event.Result) == 0 && len(event.Error) == 0 {
			if event.Method == "item/tool/call" && session.config.ToolSocket != "" {
				session.handleGoalTool(ctx, event)
				continue
			}
			_ = session.send(ctx, rpcEnvelope{JSONRPC: protocolVersion, ID: event.ID, Result: mustJSON(map[string]string{"decision": "decline"})})
			continue
		}
		if len(event.ID) == 0 || event.Method != "" {
			continue
		}
		var id int64
		if err := json.Unmarshal(event.ID, &id); err != nil {
			continue
		}
		session.mu.Lock()
		wait := session.pending[id]
		delete(session.pending, id)
		session.mu.Unlock()
		if wait == nil {
			continue
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			wait <- rpcReply{err: fmt.Errorf("native %s failed: %s", event.Method, compact(event.Error))}
		} else {
			wait <- rpcReply{result: event.Result}
		}
	}
}

func (session *Session) observeTurn(event rpcEnvelope) {
	if event.Method != "turn/started" && event.Method != "turn/completed" {
		return
	}
	var params struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if json.Unmarshal(event.Params, &params) != nil || session.ThreadID == "" || params.ThreadID != session.ThreadID || params.Turn.ID == "" {
		return
	}
	session.turnMu.Lock()
	defer session.turnMu.Unlock()
	session.latestTurn = params.Turn.ID
	if event.Method == "turn/started" {
		session.nativeBusy[params.Turn.ID] = struct{}{}
		if params.Turn.Status == "" {
			params.Turn.Status = "inProgress"
		}
		session.latestStatus = params.Turn.Status
	} else {
		delete(session.nativeBusy, params.Turn.ID)
		session.latestStatus = params.Turn.Status
	}
	session.updateTurnIdleLocked()
}

func (session *Session) updateTurnIdleLocked() {
	busy := session.turnActive || len(session.nativeBusy) > 0
	if busy && !session.turnOpen {
		session.turnIdle = make(chan struct{})
		session.turnOpen = true
		return
	}
	if !busy && session.turnOpen {
		close(session.turnIdle)
		session.turnOpen = false
	}
}

func (session *Session) waitEvent(ctx context.Context, predicate func(rpcEnvelope) bool) (rpcEnvelope, error) {
	deadline := time.Now().Add(rpcTimeout)
	for {
		select {
		case <-ctx.Done():
			return rpcEnvelope{}, ctx.Err()
		case <-session.done:
			return rpcEnvelope{}, errors.New("native session closed")
		case <-time.After(25 * time.Millisecond):
		}
		session.mu.Lock()
		events := append([]json.RawMessage(nil), session.events...)
		session.mu.Unlock()
		for _, raw := range events {
			var event rpcEnvelope
			if json.Unmarshal(raw, &event) == nil && predicate(event) {
				return event, nil
			}
		}
		if time.Now().After(deadline) {
			return rpcEnvelope{}, context.DeadlineExceeded
		}
	}
}

func (session *Session) finalText(turnID string) string {
	session.mu.Lock()
	defer session.mu.Unlock()
	if text := session.turnFinalText[turnID]; text != "" {
		return text
	}
	return session.turnText[turnID]
}

func (session *Session) recordEvent(data []byte) {
	session.mu.Lock()
	session.captureTurnTextLocked(data)
	if len(session.events) == maxEvents {
		copy(session.events, session.events[1:])
		session.events = session.events[:maxEvents-1]
	}
	session.events = append(session.events, append(json.RawMessage(nil), data...))
	callback := session.config.OnEvent
	copyData := append(json.RawMessage(nil), data...)
	session.mu.Unlock()
	if callback != nil {
		callback(copyData)
	}
}

func (session *Session) captureTurnTextLocked(data []byte) {
	var event struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Phase   string `json:"phase"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal(data, &event) != nil || event.Method != "item/completed" || event.Params.ThreadID != session.ThreadID || event.Params.TurnID == "" || event.Params.Item.Type != "agentMessage" {
		return
	}
	text := event.Params.Item.Text
	if text == "" {
		var parts []string
		for _, content := range event.Params.Item.Content {
			if content.Type == "text" {
				parts = append(parts, content.Text)
			}
		}
		text = strings.Join(parts, "")
	}
	if text == "" {
		return
	}
	if len(text) > 128*1024 {
		text = text[:128*1024]
	}
	if len(session.turnText) >= maxEvents {
		for turnID := range session.turnText {
			if turnID != event.Params.TurnID {
				delete(session.turnText, turnID)
				break
			}
		}
	}
	session.turnText[event.Params.TurnID] = text
	if event.Params.Item.Phase == "final_answer" {
		if len(session.turnFinalText) >= maxEvents {
			for turnID := range session.turnFinalText {
				if turnID != event.Params.TurnID {
					delete(session.turnFinalText, turnID)
					break
				}
			}
		}
		session.turnFinalText[event.Params.TurnID] = text
	}
}

func (session *Session) failPending(err error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	for id, wait := range session.pending {
		wait <- rpcReply{err: err}
		delete(session.pending, id)
	}
}
func (session *Session) removePending(id int64) {
	session.mu.Lock()
	delete(session.pending, id)
	session.mu.Unlock()
}

func validateConfig(config Config) error {
	if (!config.ExecutionOnly && config.Model == "") || (config.ExecutionOnly && config.Model != "") {
		return errors.New("model is required only for a model session")
	}
	for name, value := range map[string]string{"binary": config.Binary, "supervisor binary": config.SupervisorBinary, "control directory": config.ControlDir, "cwd": config.Cwd, "socket": config.Socket} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	for name, value := range map[string]string{"binary": config.Binary, "supervisor binary": config.SupervisorBinary, "control directory": config.ControlDir, "cwd": config.Cwd, "socket": config.Socket} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be absolute and clean", name)
		}
	}
	if !filepath.IsAbs(config.Cwd) {
		return errors.New("cwd must be absolute")
	}
	if len(config.WritableRoots) == 0 {
		return errors.New("at least one writable root is required")
	}
	if info, err := os.Stat(config.Cwd); err != nil || !info.IsDir() {
		return errors.New("cwd must be an existing directory")
	}
	for _, root := range config.WritableRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return errors.New("writable roots must be absolute and clean")
		}
		if pathWithin(root, config.ControlDir) || pathWithin(config.ControlDir, root) || pathWithin(config.Socket, root) {
			return errors.New("control paths must be outside writable roots")
		}
	}
	if pathWithin(config.Socket, config.Cwd) || pathWithin(config.Cwd, config.Socket) {
		return errors.New("socket must be outside cwd")
	}
	return nil
}

func assertUntrustedConfig(raw json.RawMessage, cwd string) error {
	var response struct {
		Config struct {
			Projects map[string]struct {
				TrustLevel string `json:"trust_level"`
			} `json:"projects"`
		} `json:"config"`
		Layers []struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
			Config struct {
				Projects map[string]struct {
					TrustLevel string `json:"trust_level"`
				} `json:"projects"`
			} `json:"config"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode native config: %w", err)
	}
	if response.Config.Projects[cwd].TrustLevel != "untrusted" {
		return errors.New("native config did not retain the exact untrusted workspace")
	}
	for _, layer := range response.Layers {
		if layer.Name.Type == "sessionFlags" && layer.Config.Projects[cwd].TrustLevel == "untrusted" {
			return nil
		}
	}
	return errors.New("native untrusted workspace did not come from session flags")
}

func assertOpenAIRouting(raw json.RawMessage) error {
	var response struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode native provider configuration: %w", err)
	}
	providersRaw := response.Config["model_providers"]
	if len(providersRaw) == 0 {
		return nil
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(providersRaw, &providers); err != nil {
		return errors.New("native model provider configuration is malformed")
	}
	openAIRaw := providers["openai"]
	if len(openAIRaw) == 0 {
		return nil
	}
	var provider struct {
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(openAIRaw, &provider); err != nil {
		return errors.New("native OpenAI provider configuration is malformed")
	}
	if provider.BaseURL != "" && !directOpenAIEndpoint(provider.BaseURL) {
		return errors.New("native OpenAI provider routes to a non-OpenAI endpoint")
	}
	return nil
}

func assertNativeIsolation(raw json.RawMessage) error {
	var response struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return errors.New("native capability configuration is malformed")
	}
	if len(response.Config) == 0 {
		return errors.New("native capability configuration is missing")
	}
	features, ok := response.Config["features"]
	if !ok {
		return errors.New("native capability features are missing")
	}
	var featureValues map[string]json.RawMessage
	if err := json.Unmarshal(features, &featureValues); err != nil {
		return errors.New("native capability features are malformed")
	}
	for _, name := range []string{"apps", "hooks", "memories", "multi_agent", "plugins"} {
		if !nativeConfigFalse(featureValues[name]) {
			return errors.New("native capability feature is enabled")
		}
	}
	agents, ok := response.Config["agents"]
	if !ok || !nativeConfigEntryFalse(agents) {
		return errors.New("native agents are enabled")
	}
	var webSearch string
	if value, ok := response.Config["web_search"]; !ok || json.Unmarshal(value, &webSearch) != nil || webSearch != "disabled" {
		return errors.New("native web search is enabled")
	}
	notify, ok := response.Config["notify"]
	if !ok {
		return errors.New("native notifications are missing")
	}
	var notifyValues []json.RawMessage
	if trimmed := strings.TrimSpace(string(notify)); trimmed == "" || !strings.HasPrefix(trimmed, "[") || json.Unmarshal(notify, &notifyValues) != nil || len(notifyValues) != 0 {
		return errors.New("native notifications are enabled")
	}
	if err := assertNativeDisabledEntries(response.Config["apps"], true); err != nil {
		return err
	}
	for _, section := range []string{"mcp_servers", "plugins"} {
		if value, present := response.Config[section]; present {
			if err := assertNativeDisabledEntries(value, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func assertNativeDisabledEntries(raw json.RawMessage, requireDefault bool) error {
	if len(raw) == 0 {
		return errors.New("native capability entries are missing")
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return errors.New("native capability entries are malformed")
	}
	if requireDefault {
		if _, ok := entries["_default"]; !ok {
			return errors.New("native app defaults are missing")
		}
	}
	for _, entry := range entries {
		if !nativeConfigEntryFalse(entry) {
			return errors.New("native capability entry is enabled")
		}
	}
	return nil
}

func nativeConfigEntryFalse(raw json.RawMessage) bool {
	var entry map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &entry) != nil {
		return false
	}
	return nativeConfigFalse(entry["enabled"])
}

func nativeConfigFalse(raw json.RawMessage) bool {
	var value any
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value == false
}

func validateOpenAIEnvironment(environment []string) error {
	if environment == nil {
		environment = os.Environ()
	}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "OPENAI_BASE_URL" && value != "" && !directOpenAIEndpoint(value) {
			return errors.New("OPENAI_BASE_URL must use the direct OpenAI HTTPS endpoint")
		}
	}
	return nil
}

func directOpenAIEndpoint(value string) bool {
	endpoint, err := url.Parse(value)
	return err == nil && endpoint.Scheme == "https" && endpoint.Hostname() == "api.openai.com" && endpoint.Port() == "" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == ""
}

func validateSandbox(cwd string, roots []string) error {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return errors.New("sandbox cwd must be absolute and clean")
	}
	if len(roots) == 0 {
		return errors.New("sandbox requires at least one writable root")
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return errors.New("sandbox writable roots must be absolute and clean")
		}
	}
	return nil
}

func workspacePolicy(roots []string) map[string]any {
	return map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": append([]string(nil), roots...), "excludeTmpdirEnvVar": true, "excludeSlashTmp": true}
}
func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func safeToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
func mustJSON(value any) json.RawMessage {
	// termsafe:allow-raw-json native JSON-RPC protocol payload, never terminal output
	data, _ := json.Marshal(value)
	return data
}
func compact(data []byte) string {
	var value any
	if json.Unmarshal(data, &value) == nil {
		// termsafe:allow-raw-json compacted native protocol diagnostic, never terminal output
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	return string(data)
}
func eventHasTurn(raw json.RawMessage, threadID, turnID string) bool {
	var value struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	return json.Unmarshal(raw, &value) == nil && value.ThreadID == threadID && value.Turn.ID == turnID
}

func dialUnixWebSocket(ctx context.Context, socket string) (*websocket.Conn, error) {
	dialer := net.Dialer{Timeout: startupTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	config := &websocket.Config{Location: &url.URL{Scheme: "ws", Host: "localhost", Path: "/"}, Origin: &url.URL{Scheme: "http", Host: "localhost"}, Version: websocket.ProtocolVersionHybi13}
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ws.MaxPayloadBytes = maxPayloadBytes
	return ws, nil
}

func mustPrivatePath(path string) error {
	info, err := os.Lstat(path) //nolint:gosec // callers pass owner-controlled paths after validating their destination
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path must not be a symlink")
	}
	return nil
}
