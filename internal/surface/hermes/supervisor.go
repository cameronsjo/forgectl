//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	maxLine          = 1 << 20
	maxSocketPath    = 103
	maxHistoryBytes  = 8 << 20
	maxHistoryUpdate = 50000
)

type Config struct {
	Command       string
	Args          []string
	Model         string
	Ledger        string
	Member        string
	Worktree      string
	Socket        string
	ListenSocket  string
	RunID         string
	SessionID     string
	OnSession     func(string) error
	NativeReplies bool
	PrepareReply  func(context.Context, Request) (ReplyPreparation, error)
	PublishReply  func(context.Context, ReplyPublication) (ReplyPublicationResult, error)
	Stdout        io.Writer
	Stderr        io.Writer
}

type ReplyPreparation struct {
	Prompt         string
	SourceDigest   string
	Recipient      string
	RecipientRunID string
}

type ReplyPublication struct {
	Input    Request
	Prepared ReplyPreparation
	Body     string
}

type ReplyPublicationResult struct {
	MessageID string
	Status    string
}

type Ready struct {
	Type          string `json:"type"`
	Ready         bool   `json:"ready"`
	RunID         string `json:"run_id"`
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	NativeVersion string `json:"native_version"`
	Mode          string `json:"mode"`
	RouteSocket   string `json:"route_socket"`
	ListenSocket  string `json:"listen_socket"`
}

type Request struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

type Response struct {
	V           int         `json:"v"`
	OK          bool        `json:"ok"`
	ID          string      `json:"id,omitempty"`
	RunID       string      `json:"run_id,omitempty"`
	SessionID   string      `json:"session_id,omitempty"`
	Accepted    *bool       `json:"accepted,omitempty"`
	Evidence    string      `json:"evidence,omitempty"`
	Idle        *bool       `json:"idle,omitempty"`
	Known       bool        `json:"known,omitempty"`
	Unconfirmed bool        `json:"unconfirmed,omitempty"`
	Retry       bool        `json:"retry,omitempty"`
	Error       string      `json:"error,omitempty"`
	Inspection  *Inspection `json:"inspection,omitempty"`
}

type historyUpdate struct {
	SessionID      string
	Kind           string
	Text           string
	ContentType    string
	MetadataDigest string
	Size           int
}

type receipt struct {
	ID           string `json:"id"`
	SessionID    string `json:"session_id"`
	RunID        string `json:"run_id"`
	Digest       string `json:"digest"`
	Text         string `json:"text"`
	NativePrompt string `json:"native_prompt,omitempty"`
	State        string `json:"state"`
}

type replyRecord struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	RunID          string `json:"run_id"`
	Digest         string `json:"digest"`
	Text           string `json:"text"`
	SourceDigest   string `json:"source_digest,omitempty"`
	Recipient      string `json:"recipient,omitempty"`
	RecipientRunID string `json:"recipient_run_id,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	PromptDigest   string `json:"prompt_digest,omitempty"`
	Body           string `json:"body,omitempty"`
	Frame          string `json:"frame,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	Status         string `json:"status,omitempty"`
	State          string `json:"state"`
}

type nativeEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type nativeMessage struct {
	envelope nativeEnvelope
	line     []byte
}

type nativeClient struct {
	command      *exec.Cmd
	cancel       context.CancelFunc
	stdin        io.WriteCloser
	events       chan nativeMessage
	done         chan struct{}
	exit         chan error
	readErr      chan struct{}
	readOnce     sync.Once
	readMu       sync.Mutex
	readFailure  error
	errMu        sync.Mutex
	processErr   error
	doneOnce     sync.Once
	mu           sync.Mutex
	nextID       int
	onPermission func(json.RawMessage)
	permissionMu sync.Mutex
	permissions  uint64
	unsupported  uint64
}

func (client *nativeClient) processMessage(message nativeMessage, id int, method string, updates *[]historyUpdate) (json.RawMessage, bool, error) {
	if message.envelope.Method != "" {
		if message.envelope.Method == "session/update" {
			var params struct {
				SessionID string          `json:"sessionId"`
				Update    json.RawMessage `json:"update"`
			}
			if err := json.Unmarshal(message.envelope.Params, &params); err != nil || params.SessionID == "" || len(params.Update) == 0 {
				return nil, false, errors.New("native Hermes session/update notification is malformed")
			}
			update, err := parseHistoryUpdate(params.Update)
			if err != nil {
				return nil, false, errors.New("native Hermes session/update payload is malformed")
			}
			if updates != nil {
				size := len(params.Update)
				if len(*updates) >= maxHistoryUpdate || historyBytes(*updates)+size > maxHistoryBytes {
					return nil, false, errors.New("native Hermes history exceeded the bounded update limit")
				}
				*updates = append(*updates, historyUpdate{
					SessionID:      params.SessionID,
					Kind:           update.Kind,
					Text:           update.Text,
					ContentType:    update.ContentType,
					MetadataDigest: metadataDigest(params.Update),
					Size:           size,
				})
			}
		} else if len(message.envelope.ID) == 0 {
			return nil, false, fmt.Errorf("native Hermes sent unsupported notification %q", message.envelope.Method)
		}
		if len(message.envelope.ID) > 0 {
			if err := client.answer(message.envelope.ID, message.envelope.Method); err != nil {
				return nil, false, err
			}
		}
		return nil, false, nil
	}
	if len(message.envelope.ID) == 0 {
		return nil, false, errors.New("native Hermes response lacks an id")
	}
	var responseID int
	if err := json.Unmarshal(message.envelope.ID, &responseID); err != nil {
		return nil, false, fmt.Errorf("native Hermes response id is invalid: %w", err)
	}
	if responseID != id {
		return nil, false, fmt.Errorf("native Hermes response id %d does not match request %d", responseID, id)
	}
	if len(message.envelope.Error) > 0 && string(message.envelope.Error) != "null" {
		return nil, true, fmt.Errorf("hermes %s returned an error: %s", method, oneLine(message.envelope.Error, 300))
	}
	if len(message.envelope.Result) == 0 {
		return nil, true, fmt.Errorf("hermes %s response lacks a result", method)
	}
	return message.envelope.Result, true, nil
}

type parsedHistoryUpdate struct {
	Kind        string
	Text        string
	ContentType string
}

func parseHistoryUpdate(raw []byte) (parsedHistoryUpdate, error) {
	var update struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &update); err != nil || update.SessionUpdate == "" {
		return parsedHistoryUpdate{}, errors.New("history update is malformed")
	}
	parsed := parsedHistoryUpdate{Kind: update.SessionUpdate}
	switch update.SessionUpdate {
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
		contentRaw := bytes.TrimSpace(update.Content)
		if len(contentRaw) == 0 || bytes.Equal(contentRaw, []byte("null")) {
			return parsedHistoryUpdate{}, errors.New("text history update lacks content")
		}
		var content struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(contentRaw, &content); err != nil || content.Type != "text" || content.Text == nil {
			return parsedHistoryUpdate{}, errors.New("text history update content is malformed")
		}
		parsed.Text, parsed.ContentType = *content.Text, content.Type
	case "tool_call", "tool_call_update":
		contentRaw := bytes.TrimSpace(update.Content)
		if len(contentRaw) > 0 && !bytes.Equal(contentRaw, []byte("null")) {
			var content []json.RawMessage
			if err := json.Unmarshal(contentRaw, &content); err != nil {
				return parsedHistoryUpdate{}, errors.New("structured history update content is malformed")
			}
			for _, item := range content {
				var entry map[string]json.RawMessage
				if err := json.Unmarshal(item, &entry); err != nil || entry == nil {
					return parsedHistoryUpdate{}, errors.New("structured history update entry is malformed")
				}
			}
			parsed.ContentType = "structured"
		}
	}
	return parsed, nil
}

func historyBytes(updates []historyUpdate) int {
	total := 0
	for _, update := range updates {
		total += update.Size
	}
	return total
}

type sessionResult struct {
	SessionID string `json:"sessionId"`
	AgentInfo struct {
		Version string `json:"version"`
	} `json:"agentInfo"`
	Models struct {
		CurrentModelID string `json:"currentModelId"`
	} `json:"models"`
	Modes struct {
		CurrentModeID string `json:"currentModeId"`
	} `json:"modes"`
}

type supervisor struct {
	config               Config
	native               *nativeClient
	listener             net.Listener
	listenerInfo         os.FileInfo
	sessionID            string
	model                string
	mode                 string
	nativeVer            string
	receiptPath          string
	receipts             map[string]receipt
	replyPath            string
	replies              map[string]replyRecord
	mu                   sync.Mutex
	nativeMu             sync.Mutex
	busy                 bool
	eventMu              sync.Mutex
	events               eventWriter
	receiptLock          *os.File
	handlers             sync.WaitGroup
	forwards             sync.WaitGroup
	connections          map[net.Conn]struct{}
	closing              bool
	lastTurn             *TurnObservation
	history              []historyUpdate
	activitySince        time.Time
	activitySessionStart time.Time
	activitySpans        []activity.Span
	activityPartial      bool
}

func (receiver *supervisor) currentSession() string {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.sessionID
}

func sessionKey(member, session string) string {
	sum := sha256.Sum256([]byte(member + "\x00" + session))
	return hex.EncodeToString(sum[:12])
}

func nativeSessionKey(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:12])
}

func (receiver *supervisor) acquireReceiptLock() error {
	path := filepath.Join(receiver.config.Ledger, "hermes-session-"+nativeSessionKey(receiver.sessionID)+".lock")
	file, err := openLedgerFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return errors.New("another Hermes supervisor owns this native session")
	}
	receiver.receiptLock = file
	return nil
}

func (receiver *supervisor) releaseReceiptLock() {
	if receiver.receiptLock == nil {
		return
	}
	_ = syscall.Flock(int(receiver.receiptLock.Fd()), syscall.LOCK_UN)
	_ = receiver.receiptLock.Close()
}

type eventWriter struct {
	writer io.Writer
	mu     *sync.Mutex
}

func (outputWriter eventWriter) write(value any) {
	outputWriter.mu.Lock()
	defer outputWriter.mu.Unlock()
	_ = termsafe.JSONEncoder(outputWriter.writer).Encode(value)
}

func Run(ctx context.Context, config Config) error {
	var err error
	config, err = normalizeConfig(config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	native, err := startNative(config)
	if err != nil {
		return err
	}
	receiver := &supervisor{config: config, native: native, receipts: map[string]receipt{}, replies: map[string]replyRecord{}, connections: map[net.Conn]struct{}{}}
	output := eventWriter{writer: writerOrDiscard(config.Stdout), mu: &receiver.eventMu}
	receiver.events = output
	exitEventDone := make(chan struct{})
	native.onPermission = func(id json.RawMessage) {
		output.write(map[string]any{"type": "hermes-permission-denied", "native_request_id": oneLine(id, 200), "session_id": receiver.currentSession(), "run_id": config.RunID})
	}
	go func() {
		defer close(exitEventDone)
		err, ok := <-native.exit
		if !ok {
			return
		}
		event := map[string]any{"type": "hermes-native-exit", "run_id": config.RunID, "session_id": receiver.currentSession()}
		if err == nil {
			event["exitcode"] = 0
		} else if processErr, ok := err.(*exec.ExitError); ok {
			if status, ok := processErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				event["exitcode"] = -1
				event["signal"] = status.Signal().String()
			} else {
				event["exitcode"] = processErr.ExitCode()
			}
		} else {
			event["exitcode"] = -1
			event["error"] = oneLine([]byte(err.Error()), 300)
		}
		output.write(event)
	}()
	defer func() {
		cancel()
		receiver.closeListener()
		receiver.mu.Lock()
		receiver.closing = true
		for connection := range receiver.connections {
			_ = connection.Close()
		}
		receiver.mu.Unlock()
		native.stop()
		receiver.handlers.Wait()
		receiver.forwards.Wait()
		receiver.releaseReceiptLock()
		<-exitEventDone
	}()
	if config.SessionID != "" {
		receiver.mu.Lock()
		receiver.sessionID = config.SessionID
		receiver.mu.Unlock()
		if err := receiver.acquireReceiptLock(); err != nil {
			return err
		}
	}
	session, model, mode, version, updates, err := receiver.prepare(ctx)
	if err != nil {
		return err
	}
	receiver.setNativeSession(session, model, mode, version)
	if receiver.receiptLock == nil {
		if err := receiver.acquireReceiptLock(); err != nil {
			return err
		}
	}
	receiver.receiptPath = filepath.Join(config.Ledger, "hermes-receipts-"+sessionKey(config.Member, session)+".jsonl")
	receiver.receipts, err = loadReceipts(receiver.receiptPath)
	if err != nil {
		return err
	}
	if config.NativeReplies {
		receiver.replyPath = filepath.Join(config.Ledger, "hermes-replies-"+sessionKey(config.Member, session)+".jsonl")
		receiver.replies, err = loadReplies(receiver.replyPath)
		if err != nil {
			return err
		}
	}
	if err := receiver.reconcile(updates); err != nil {
		return err
	}
	listener, info, err := listen(ctx, config.ListenSocket)
	if err != nil {
		return err
	}
	receiver.listener, receiver.listenerInfo = listener, info
	if config.OnSession != nil {
		if err := config.OnSession(session); err != nil {
			return err
		}
	}
	if err := receiver.recoverReplies(ctx); err != nil {
		return err
	}
	provider, _, _ := strings.Cut(model, ":")
	output.write(Ready{Type: "hermes-ready", Ready: true, RunID: config.RunID, SessionID: session, NativeVersion: version, Model: model, Provider: provider, Mode: mode, RouteSocket: config.Socket, ListenSocket: config.ListenSocket})
	return receiver.serve(ctx)
}

func normalizeConfig(config Config) (Config, error) {
	if !filepath.IsAbs(config.Command) || filepath.Clean(config.Command) != config.Command {
		return Config{}, errors.New("hermes command must be an absolute clean path")
	}
	if !filepath.IsAbs(config.Ledger) || !filepath.IsAbs(config.Worktree) || !filepath.IsAbs(config.Socket) {
		return Config{}, errors.New("hermes ledger, worktree and socket must be absolute paths")
	}
	if config.ListenSocket == "" {
		config.ListenSocket = config.Socket
	}
	if config.NativeReplies && (config.PrepareReply == nil || config.PublishReply == nil) {
		return Config{}, errors.New("hermes native replies require prepare and publish callbacks")
	}
	if !filepath.IsAbs(config.ListenSocket) || filepath.Clean(config.ListenSocket) != config.ListenSocket {
		return Config{}, errors.New("hermes listen socket must be an absolute clean path")
	}
	for _, socket := range []string{config.Socket, config.ListenSocket} {
		if len(socket) > maxSocketPath || strings.ContainsAny(socket, "\x00\n\r\t%?#") {
			return Config{}, errors.New("hermes socket path is invalid")
		}
	}
	if !validToken(config.Member) || !validToken(config.RunID) {
		return Config{}, errors.New("hermes member and run id are required")
	}
	provider, model, ok := strings.Cut(config.Model, ":")
	if !ok || provider == "" || model == "" || strings.ContainsAny(config.Model, "\x00\n\r\t") {
		return Config{}, errors.New("hermes model must be PROVIDER:MODEL")
	}
	if config.SessionID != "" && !validToken(config.SessionID) {
		return Config{}, errors.New("hermes session id is invalid")
	}
	if info, err := os.Stat(config.Worktree); err != nil {
		return Config{}, fmt.Errorf("hermes worktree: %w", err)
	} else if !info.IsDir() {
		return Config{}, errors.New("hermes worktree must be a directory")
	}
	return config, nil
}

func validToken(value string) bool {
	if value == "" || len(value) > 256 || strings.HasPrefix(value, "-") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || character == '/' || character == '\\' {
			return false
		}
	}
	return true
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

func startNative(config Config) (*nativeClient, error) {
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, config.Command, config.Args...) //nolint:gosec // G204: the operator selects the validated absolute native executable, not model output.
	command.Dir = config.Worktree
	command.Stderr = writerOrDiscard(config.Stderr)
	command.WaitDelay = time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, err
	}
	if err := command.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		return nil, err
	}
	client := &nativeClient{command: command, cancel: cancel, stdin: stdin, events: make(chan nativeMessage, 64), done: make(chan struct{}), exit: make(chan error, 1), readErr: make(chan struct{})}
	go client.read(stdout)
	go func() {
		err := command.Wait()
		client.finish(err)
	}()
	return client, nil
}

func (client *nativeClient) finish(err error) {
	client.doneOnce.Do(func() {
		client.errMu.Lock()
		client.processErr = err
		client.errMu.Unlock()
		close(client.done)
		client.exit <- err
		close(client.exit)
	})
}

func (client *nativeClient) waitError() error {
	client.errMu.Lock()
	defer client.errMu.Unlock()
	return client.processErr
}

func (client *nativeClient) failRead(err error) {
	client.readOnce.Do(func() {
		client.readMu.Lock()
		client.readFailure = err
		client.readMu.Unlock()
		close(client.readErr)
	})
}

func (client *nativeClient) readError() error {
	client.readMu.Lock()
	defer client.readMu.Unlock()
	return client.readFailure
}

func (client *nativeClient) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if err := validateReplyJSONLexemes(line); err != nil {
			client.failRead(fmt.Errorf("hermes stdout contained invalid JSON text: %w", err))
			return
		}
		var envelope nativeEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.JSONRPC != "2.0" {
			if err == nil {
				err = errors.New("missing JSON-RPC 2.0 version")
			}
			client.failRead(fmt.Errorf("hermes stdout was not JSON-RPC: %w", err))
			return
		}
		select {
		case client.events <- nativeMessage{envelope: envelope, line: line}:
		case <-client.done:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		client.failRead(err)
	} else {
		client.failRead(io.EOF)
	}
}

func (client *nativeClient) stop() {
	defer client.cancel()
	_ = client.stdin.Close()
	select {
	case <-client.done:
		return
	case <-time.After(5 * time.Second):
		if client.command.Process != nil {
			_ = client.command.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-client.done:
		case <-time.After(5 * time.Second):
			client.cancel()
			<-client.done
		}
	}
}

func (client *nativeClient) call(ctx context.Context, method string, params any, updates *[]historyUpdate) (json.RawMessage, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client.nextID++
	id := client.nextID
	// termsafe:allow-raw-json native ACP request sent only to the child stdin
	request, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return nil, err
	}
	if _, err := client.stdin.Write(append(request, '\n')); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-client.done:
			if err := client.waitError(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		case message := <-client.events:
			result, done, err := client.processMessage(message, id, method, updates)
			if err != nil || done {
				return result, err
			}
		case <-client.readErr:
			for {
				select {
				case message := <-client.events:
					result, done, err := client.processMessage(message, id, method, updates)
					if err != nil || done {
						return result, err
					}
				default:
					return nil, client.readError()
				}
			}
		}
	}
}

func (client *nativeClient) answer(id json.RawMessage, method string) error {
	if method == "session/request_permission" {
		client.permissionMu.Lock()
		client.permissions++
		client.permissionMu.Unlock()
		if client.onPermission != nil {
			client.onPermission(id)
		}
	} else {
		client.permissionMu.Lock()
		client.unsupported++
		client.permissionMu.Unlock()
	}
	response := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result,omitempty"`
		Error   any             `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id}
	if method == "session/request_permission" {
		response.Result = map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
	} else {
		response.Error = map[string]any{"code": -32601, "message": "unsupported client operation"}
	}
	// termsafe:allow-raw-json native ACP response sent only to the child stdin
	line, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = client.stdin.Write(append(line, '\n'))
	return err
}

func (client *nativeClient) permissionCount() uint64 {
	client.permissionMu.Lock()
	defer client.permissionMu.Unlock()
	return client.permissions
}

func (client *nativeClient) unsupportedCount() uint64 {
	client.permissionMu.Lock()
	defer client.permissionMu.Unlock()
	return client.unsupported
}

func (receiver *supervisor) prepare(ctx context.Context) (string, string, string, string, []historyUpdate, error) {
	var initialized struct {
		AgentInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	result, err := receiver.native.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "forgectl", "version": "1"}, "clientCapabilities": map[string]any{}}, nil)
	if err != nil {
		return "", "", "", "", nil, err
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		return "", "", "", "", nil, fmt.Errorf("hermes initialize response: %w", err)
	}
	provider, _, _ := strings.Cut(receiver.config.Model, ":")
	if initialized.AgentInfo.Name != "hermes-agent" || initialized.AgentInfo.Version == "" || !initialized.AgentCapabilities.LoadSession {
		return "", "", "", "", nil, errors.New("native Hermes did not advertise the required agent identity and session loading capability")
	}
	authenticated := false
	for _, method := range initialized.AuthMethods {
		if method.ID == provider {
			authenticated = true
			break
		}
	}
	if !authenticated {
		return "", "", "", "", nil, fmt.Errorf("native Hermes does not advertise provider %q", provider)
	}
	if _, err := receiver.native.call(ctx, "authenticate", map[string]string{"methodId": provider}, nil); err != nil {
		return "", "", "", "", nil, err
	}
	var session sessionResult
	updates := make([]historyUpdate, 0)
	receiver.mu.Lock()
	receiver.activitySessionStart = time.Now().UTC()
	receiver.mu.Unlock()
	if receiver.config.SessionID == "" {
		result, err = receiver.native.call(ctx, "session/new", map[string]any{"cwd": receiver.config.Worktree, "mcpServers": []any{}}, &updates)
	} else {
		result, err = receiver.native.call(ctx, "session/load", map[string]any{"cwd": receiver.config.Worktree, "sessionId": receiver.config.SessionID, "mcpServers": []any{}}, &updates)
	}
	if err != nil {
		return "", "", "", "", nil, err
	}
	if err := json.Unmarshal(result, &session); err != nil {
		return "", "", "", "", nil, fmt.Errorf("hermes session response: %w", err)
	}
	if session.SessionID == "" {
		session.SessionID = receiver.config.SessionID
	}
	if session.SessionID == "" || (receiver.config.SessionID != "" && session.SessionID != receiver.config.SessionID) {
		return "", "", "", "", nil, errors.New("hermes returned an unexpected session id")
	}
	if session.Models.CurrentModelID != receiver.config.Model {
		return "", "", "", "", nil, fmt.Errorf("hermes selected model %q, want %q", session.Models.CurrentModelID, receiver.config.Model)
	}
	if session.Modes.CurrentModeID != "default" {
		return "", "", "", "", nil, fmt.Errorf("hermes selected mode %q, want default", session.Modes.CurrentModeID)
	}
	return session.SessionID, session.Models.CurrentModelID, session.Modes.CurrentModeID, initialized.AgentInfo.Version, updates, nil
}

func (receiver *supervisor) serve(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
		case <-receiver.native.done:
		case <-receiver.native.readErr:
		}
		_ = receiver.listener.Close()
	}()
	for {
		connection, err := receiver.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if nativeErr := receiver.native.waitError(); nativeErr != nil {
				return fmt.Errorf("native Hermes exited: %w", nativeErr)
			}
			if readErr := receiver.native.readError(); readErr != nil {
				return fmt.Errorf("native Hermes transport stopped: %w", readErr)
			}
			return errors.New("native Hermes exited")
		}
		receiver.mu.Lock()
		receiver.connections[connection] = struct{}{}
		receiver.handlers.Add(1)
		receiver.mu.Unlock()
		go receiver.handle(ctx, connection)
	}
}

func (receiver *supervisor) handle(ctx context.Context, connection net.Conn) {
	defer func() {
		_ = connection.Close()
		receiver.mu.Lock()
		delete(receiver.connections, connection)
		receiver.mu.Unlock()
		receiver.handlers.Done()
	}()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), maxLine)
	if !scanner.Scan() {
		return
	}
	var request Request
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		writeResponse(connection, Response{V: 1, Error: "request is not valid JSON"})
		return
	}
	if request.V != 1 {
		writeResponse(connection, Response{V: 1, ID: request.ID, Error: "unsupported protocol version"})
		return
	}
	response := receiver.handleRequest(ctx, request)
	writeResponse(connection, response)
}

func (receiver *supervisor) handleRequest(ctx context.Context, request Request) Response {
	base := Response{V: 1, ID: request.ID, RunID: receiver.config.RunID, SessionID: receiver.sessionID}
	if request.RunID != receiver.config.RunID {
		base.Error = "run id does not match the registered process"
		return base
	}
	if request.SessionID != receiver.sessionID {
		base.Error = "session id does not match the native session"
		return base
	}
	switch request.Type {
	case "inspect":
		receiver.mu.Lock()
		provider, model, found := strings.Cut(receiver.model, ":")
		if found && provider != "" && model != "" && receiver.mode != "" {
			base.OK = true
			base.Inspection = &Inspection{Provider: provider, Model: model, Mode: receiver.mode, ModelEvidence: "native-startup-handshake", Idle: !receiver.busy}
			if receiver.lastTurn != nil {
				turn := *receiver.lastTurn
				base.Inspection.Turn = &turn
			}
			base.Inspection.Activity = receiver.activitySnapshotLocked(time.Now().UTC())
		}
		receiver.mu.Unlock()
		return base
	case "state":
		receiver.mu.Lock()
		idle := !receiver.busy
		receiver.mu.Unlock()
		base.OK = true
		base.Idle = &idle
		return base
	case "receipt":
		return receiver.receiptResponse(base, request.ID)
	case "deliver":
		return receiver.deliverResponse(ctx, request)
	default:
		base.Error = "unknown request type"
		return base
	}
}

func (receiver *supervisor) receiptResponse(base Response, id string) Response {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	record, ok := receiver.receipts[id]
	if !ok || record.SessionID != receiver.sessionID {
		base.Unconfirmed = true
		base.Error = "receipt is unknown or belongs to another native session"
		return base
	}
	if record.State != "accepted" {
		base.Unconfirmed = true
		base.Error = "receipt is unconfirmed"
		return base
	}
	accepted := true
	base.OK, base.Known, base.Accepted, base.Evidence = true, true, &accepted, "history"
	return base
}

func (receiver *supervisor) deliverResponse(ctx context.Context, request Request) Response {
	base := Response{V: 1, ID: request.ID, RunID: receiver.config.RunID, SessionID: receiver.sessionID}
	if request.ID == "" || request.Text == "" {
		base.Error = "message id and text are required"
		return base
	}
	digest := digest(request.Text)
	receiver.mu.Lock()
	if receiver.closing || ctx.Err() != nil {
		receiver.mu.Unlock()
		base.Retry, base.Error = true, "native Hermes session is stopping"
		return base
	}
	if previous, ok := receiver.receipts[request.ID]; ok {
		if previous.Digest != digest {
			receiver.mu.Unlock()
			base.Error = "message id was reused with different text"
			return base
		}
		if previous.SessionID == receiver.sessionID && previous.State == "accepted" {
			accepted := true
			receiver.mu.Unlock()
			base.OK, base.Known, base.Accepted, base.Evidence = true, true, &accepted, "history"
			return base
		}
		base.Unconfirmed = true
		base.Error = "delivery is unconfirmed"
		receiver.mu.Unlock()
		return base
	}
	if receiver.busy {
		receiver.mu.Unlock()
		base.Retry, base.Error = true, "native Hermes session is busy"
		return base
	}
	record := receipt{ID: request.ID, SessionID: receiver.sessionID, RunID: receiver.config.RunID, Digest: digest, Text: request.Text, State: "pending"}
	if receiver.config.NativeReplies {
		reply := replyRecord{ID: record.ID, SessionID: record.SessionID, RunID: record.RunID, Digest: record.Digest, Text: record.Text, State: replyIntent}
		if previous, exists := receiver.replies[reply.ID]; exists {
			if previous.State != replyIntent || !sameReplyIdentity(previous, reply) {
				receiver.mu.Unlock()
				base.Error = "native reply intent conflicts with the message"
				return base
			}
		} else if err := receiver.appendReplyLocked(reply); err != nil {
			receiver.mu.Unlock()
			base.Retry, base.Error = true, "could not persist the native reply intent"
			return base
		}
	}
	if err := receiver.appendReceiptLocked(record); err != nil {
		receiver.mu.Unlock()
		base.Retry, base.Error = true, "could not persist the Hermes receipt"
		return base
	}
	receiver.busy = true
	receiver.forwards.Add(1)
	receiver.mu.Unlock()
	go receiver.forward(ctx, record)
	base.Unconfirmed = true
	base.Error = "native history acceptance is pending"
	return base
}

func (receiver *supervisor) forward(ctx context.Context, record receipt) {
	defer receiver.forwards.Done()
	receiver.events.write(map[string]any{"type": "hermes-turn-start", "message_id": record.ID, "session_id": receiver.sessionID, "run_id": receiver.config.RunID})
	var preparation ReplyPreparation
	var err error
	if receiver.config.NativeReplies {
		preparation, err = receiver.prepareNativeReply(ctx, record)
		if err == nil {
			record.NativePrompt = preparation.Prompt
			receiver.mu.Lock()
			current, ok := receiver.receipts[record.ID]
			if !ok || current.State != "pending" || current.Digest != record.Digest {
				err = errors.New("native reply receipt changed before preparation")
			} else {
				if err = receiver.appendReceiptLocked(record); err == nil {
					reply := receiver.replies[record.ID]
					if reply.State != replyIntent || reply.Digest != record.Digest {
						err = errors.New("native reply intent changed before preparation")
					} else {
						reply.SourceDigest, reply.Recipient, reply.RecipientRunID = preparation.SourceDigest, preparation.Recipient, preparation.RecipientRunID
						reply.Prompt, reply.PromptDigest, reply.State = preparation.Prompt, digest(preparation.Prompt), replyPrepared
						err = receiver.appendReplyLocked(reply)
					}
				}
			}
			receiver.mu.Unlock()
		}
	}
	if err != nil {
		receiver.mu.Lock()
		_ = receiver.markReplyUnknownLocked(record.ID, "preparation-error")
		receiver.busy = false
		receiver.mu.Unlock()
		receiver.events.write(map[string]any{"type": "hermes-turn-end", "message_id": record.ID, "session_id": receiver.sessionID, "run_id": receiver.config.RunID, "error": true})
		return
	}
	nativePrompt := record.Text
	if receiver.config.NativeReplies {
		nativePrompt = preparation.Prompt
	}
	receiver.nativeMu.Lock()
	var pre []historyUpdate
	if receiver.config.NativeReplies {
		var currentHistory []historyUpdate
		receiver.mu.Lock()
		currentHistory = append(currentHistory, receiver.history...)
		receiver.mu.Unlock()
		oldRelevant, oldErr := replyHistoryUpdates(currentHistory, receiver.sessionID)
		if oldErr != nil {
			err = oldErr
		} else {
			for _, update := range oldRelevant {
				if update.Kind == "user_message_chunk" && update.Text == preparation.Prompt {
					err = errors.New("native reply input already exists in durable history")
					break
				}
			}
		}
		if err == nil {
			_, err = receiver.native.call(ctx, "session/load", map[string]any{"cwd": receiver.config.Worktree, "sessionId": receiver.sessionID, "mcpServers": []any{}}, &pre)
		}
		if err == nil {
			freshRelevant, freshErr := replyHistoryUpdates(pre, receiver.sessionID)
			if freshErr != nil {
				err = freshErr
			} else if !historyPrefix(oldRelevant, freshRelevant) || len(oldRelevant) != len(freshRelevant) {
				err = errors.New("native reply pre-turn history changed outside the supervisor")
			}
		}
	}
	permissionStart := receiver.native.permissionCount()
	unsupportedStart := receiver.native.unsupportedCount()
	var liveUpdates []historyUpdate
	var result json.RawMessage
	var promptEndedAt time.Time
	if err == nil {
		receiver.beginActivity(record.ID, time.Now().UTC())
		result, err = receiver.native.call(ctx, "session/prompt", map[string]any{"sessionId": receiver.sessionID, "prompt": []map[string]string{{"type": "text", "text": nativePrompt}}}, &liveUpdates)
		promptEndedAt = time.Now().UTC()
		receiver.finishActivity(record.ID, activity.Unknown, promptEndedAt)
	}
	var prompt struct {
		StopReason string `json:"stopReason"`
	}
	if err == nil {
		err = json.Unmarshal(result, &prompt)
		if err == nil && prompt.StopReason == "" {
			err = errors.New("native Hermes prompt response lacks a stop reason")
		}
	}
	promptActivityState := hermesActivityState(err, prompt.StopReason, receiver.native.permissionCount()-permissionStart, receiver.native.unsupportedCount()-unsupportedStart)
	var postUpdates []historyUpdate
	if err == nil {
		_, err = receiver.native.call(ctx, "session/load", map[string]any{"cwd": receiver.config.Worktree, "sessionId": receiver.sessionID, "mcpServers": []any{}}, &postUpdates)
	}
	if err == nil {
		err = receiver.reconcile(postUpdates)
	}
	receiver.nativeMu.Unlock()
	nativeErr := err
	if nativeErr == nil && ctx.Err() != nil {
		nativeErr = ctx.Err()
	}
	var replyErr error
	proofFailed := false
	unknownReason := ""
	if nativeErr == nil && receiver.config.NativeReplies {
		proof, proofErr := proveNativeReplyEvidence(record, pre, liveUpdates, postUpdates, prompt.StopReason, receiver.native.permissionCount()-permissionStart, receiver.native.unsupportedCount()-unsupportedStart)
		if proofErr != nil {
			replyErr = proofErr
			proofFailed = true
			unknownReason = "proof-error"
			var diagnosticError *nativeReplyProofError
			if errors.As(proofErr, &diagnosticError) {
				receiver.events.write(map[string]any{
					"type":                 "hermes-native-reply-proof",
					"message_id":           record.ID,
					"session_id":           record.SessionID,
					"run_id":               record.RunID,
					"proof_stage":          diagnosticError.Diagnostics.Stage,
					"pre_update_count":     diagnosticError.Diagnostics.PreUpdateCount,
					"live_update_count":    diagnosticError.Diagnostics.LiveUpdateCount,
					"post_update_count":    diagnosticError.Diagnostics.PostUpdateCount,
					"durable_frame_bytes":  diagnosticError.Diagnostics.DurableFrameBytes,
					"durable_frame_sha256": diagnosticError.Diagnostics.DurableFrameSHA256,
					"live_frame_bytes":     diagnosticError.Diagnostics.LiveFrameBytes,
					"live_frame_sha256":    diagnosticError.Diagnostics.LiveFrameSHA256,
					"fixed_prefix_count":   diagnosticError.Diagnostics.FixedPrefixCount,
					"error":                true,
				})
			}
		} else {
			var readyRecord replyRecord
			receiver.mu.Lock()
			if ctx.Err() != nil {
				replyErr = ctx.Err()
				proofFailed = true
				unknownReason = "turn-cancelled"
			}
			reply := receiver.replies[record.ID]
			if replyErr == nil && reply.State != replyPrepared {
				replyErr = errors.New("native reply was not prepared")
				proofFailed = true
				unknownReason = "proof-error"
			} else if replyErr == nil {
				reply.Body = proof.Body
				reply.Frame = proof.CanonicalFrame
				reply.State = replyReady
				replyErr = receiver.appendReplyLocked(reply)
				readyRecord = reply
			}
			receiver.mu.Unlock()
			if replyErr == nil {
				replyErr = receiver.publishReady(ctx, readyRecord)
			}
		}
	}
	if nativeErr == nil && !proofFailed && promptActivityState != activity.Unknown {
		receiver.resolveActivity(record.ID, promptActivityState)
	}
	if nativeErr != nil && unknownReason == "" {
		unknownReason = "turn-error"
	}
	receiver.events.write(map[string]any{"type": "hermes-turn-end", "message_id": record.ID, "session_id": receiver.sessionID, "run_id": receiver.config.RunID, "stop_reason": prompt.StopReason, "error": nativeErr != nil})
	receiver.mu.Lock()
	status := "unknown"
	if nativeErr == nil {
		if prompt.StopReason == "end_turn" {
			status = "completed"
		} else {
			status = "incomplete"
		}
	}
	receiver.lastTurn = &TurnObservation{MessageID: record.ID, Status: status, ObservedAt: time.Now().UTC()}
	receiver.busy = false
	if receiver.config.NativeReplies && (nativeErr != nil || proofFailed || replyErr != nil && receiver.replies[record.ID].State == replyPrepared) {
		if unknownReason == "" {
			unknownReason = "turn-error"
		}
		_ = receiver.markReplyUnknownLocked(record.ID, unknownReason)
	}
	receiver.mu.Unlock()
}

func (receiver *supervisor) reconcile(updates []historyUpdate) error {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	receiver.history = append(receiver.history[:0], updates...)
	textOwners := map[string]int{}
	for _, record := range receiver.receipts {
		if record.SessionID == receiver.sessionID {
			textOwners[receiptNativeText(record)]++
		}
	}
	for id, record := range receiver.receipts {
		if record.SessionID != receiver.sessionID || record.State != "pending" || textOwners[receiptNativeText(record)] != 1 {
			continue
		}
		matches := 0
		for _, update := range updates {
			if update.SessionID == receiver.sessionID && update.Kind == "user_message_chunk" && update.Text == receiptNativeText(record) {
				matches++
			}
		}
		if matches == 1 {
			record.State = "accepted"
			record.RunID = receiver.config.RunID
			if err := receiver.appendReceiptLocked(record); err != nil {
				return err
			}
			receiver.receipts[id] = record
		}
	}
	return nil
}

func receiptNativeText(record receipt) string {
	if record.NativePrompt != "" {
		return record.NativePrompt
	}
	return record.Text
}

func (receiver *supervisor) appendReceiptLocked(record receipt) error {
	file, err := openLedgerFile(receiver.receiptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	// termsafe:allow-raw-json durable receipt written only to the private ledger
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	receiver.receipts[record.ID] = record
	return nil
}

func (receiver *supervisor) closeListener() {
	if receiver.listener == nil {
		return
	}
	_ = receiver.listener.Close()
	info, err := os.Lstat(receiver.config.ListenSocket)
	if err != nil {
		return
	}
	if receiver.listenerInfo == nil || !os.SameFile(info, receiver.listenerInfo) {
		return
	}
	_ = os.Remove(receiver.config.ListenSocket)
}

func listen(ctx context.Context, path string) (net.Listener, os.FileInfo, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, nil, errors.New("hermes listen socket already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return nil, nil, err
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	return listener, info, nil
}

func openLedgerFile(path string, flags int) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.OpenFile(filepath.Base(path), flags, 0o600)
}

func loadReceipts(path string) (map[string]receipt, error) {
	file, err := openLedgerFile(path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]receipt{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	result := map[string]receipt{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxLine)
	for scanner.Scan() {
		var record receipt
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || record.ID == "" || record.Digest != digest(record.Text) || (record.State != "pending" && record.State != "accepted") {
			return nil, errors.New("hermes receipt store is invalid")
		}
		if old, ok := result[record.ID]; ok && (old.Digest != record.Digest || old.SessionID != record.SessionID || old.State == "accepted" && record.State != "accepted") {
			return nil, errors.New("hermes receipt store contains conflicting records")
		}
		result[record.ID] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func writeResponse(connection net.Conn, response Response) {
	// termsafe:allow-raw-json supervisor protocol response sent only over its Unix socket
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	_, _ = connection.Write(append(data, '\n'))
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func oneLine(data []byte, limit int) string {
	value := strings.Join(strings.Fields(string(data)), " ")
	if len(value) > limit {
		return value[:limit] + "..."
	}
	return value
}
