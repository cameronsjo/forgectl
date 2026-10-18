package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/websocket"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
)

const (
	codexObservationProtocolVersion = "2.0"
	codexObservationMaxPayloadBytes = 1 << 20
	codexObservationMaxMessages     = 64
	codexObservationMaxTokenBytes   = 4096
)

// CodexTurnObservation is the last settled turn visible in native history.
// An in-progress native turn is deliberately not represented as a turn: it is
// not completion evidence.
type CodexTurnObservation struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// CodexObservation is a bounded, read-only snapshot of an enrolled Codex
// thread. It contains no transcript, prompt, or native error body.
type CodexObservation struct {
	NativeSessionID string                `json:"native_session_id"`
	Provider        string                `json:"provider"`
	Model           string                `json:"model"`
	CWD             string                `json:"cwd"`
	Turn            *CodexTurnObservation `json:"turn,omitempty"`
	Activity        *activity.Snapshot    `json:"activity,omitempty"`
}

type codexRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type codexRPCResponse struct {
	JSONRPC json.RawMessage `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type codexThreadRead struct {
	Thread codexThread `json:"thread"`
}

type codexThread struct {
	ID            string `json:"id"`
	CWD           string `json:"cwd"`
	Model         string `json:"model"`
	ModelProvider string `json:"modelProvider"`
}

type codexTurnsList struct {
	Data       json.RawMessage `json:"data"`
	NextCursor string          `json:"nextCursor"`
}

type codexTurn struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	StartedAt   json.RawMessage `json:"startedAt"`
	CompletedAt json.RawMessage `json:"completedAt"`
	Error       json.RawMessage `json:"error"`
}

// Inspect reads native Codex metadata without resuming, starting, steering,
// or otherwise mutating the thread. RunID is required as the enrollment gate;
// Codex's app-server protocol has no run-id field to echo back.
func (adapter CodexAdapter) Inspect(ctx context.Context, worker Worker) (CodexObservation, error) {
	var observation CodexObservation
	err := adapter.withCodexRPC(ctx, worker, func(ctx context.Context, client *codexRPCClient) error {
		if err := codexInitialize(ctx, client); err != nil {
			return err
		}
		thread, err := codexReadThread(ctx, client, worker)
		if err != nil {
			return err
		}
		turnsResult, err := client.call(ctx, "thread/turns/list", map[string]any{
			"threadId":      worker.ThreadID,
			"limit":         activity.MaxSpans,
			"sortDirection": "desc",
			"itemsView":     "notLoaded",
		})
		if err != nil {
			return err
		}
		var turns codexTurnsList
		if err := json.Unmarshal(turnsResult, &turns); err != nil || turns.Data == nil {
			return errors.New("codex thread/turns/list returned malformed metadata")
		}
		var nativeTurns []codexTurn
		if string(turns.Data) == "null" {
			return errors.New("codex thread/turns/list returned malformed metadata")
		}
		if err := json.Unmarshal(turns.Data, &nativeTurns); err != nil || len(nativeTurns) > activity.MaxSpans {
			return errors.New("codex thread/turns/list returned malformed metadata")
		}
		observedAt := time.Now().UTC()
		activitySnapshot, err := codexActivitySnapshot(nativeTurns, observedAt)
		if err != nil {
			return err
		}
		activitySnapshot.Partial = activitySnapshot.Partial || turns.NextCursor != ""
		observation = CodexObservation{NativeSessionID: thread.ID, Provider: thread.ModelProvider, Model: thread.Model, CWD: thread.CWD, Activity: &activitySnapshot}
		var settled *CodexTurnObservation
		for _, nativeTurn := range nativeTurns {
			turn, turnErr := codexSettledTurn(nativeTurn)
			if turnErr != nil {
				return turnErr
			}
			if settled == nil && turn != nil {
				settled = turn
			}
		}
		observation.Turn = settled
		return nil
	})
	return observation, err
}

func (adapter CodexAdapter) withCodexRPC(ctx context.Context, worker Worker, fn func(context.Context, *codexRPCClient) error) error {
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	if err := validateCodexObservationWorker(worker); err != nil {
		return err
	}
	snapshot, err := inspectCodexSocket(worker.Socket)
	if err != nil {
		return err
	}
	timeout := adapter.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", worker.Socket)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		return NotReady("dial codex app-server: endpoint unavailable")
	}
	ws, err := codexWebSocket(ctx, conn, deadline)
	if err != nil {
		_ = conn.Close()
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		return Unconfirmed("codex app-server handshake failed")
	}
	var closeSocket sync.Once
	closeWebSocket := func() { closeSocket.Do(func() { _ = ws.Close() }) }
	defer closeWebSocket()
	if !sameCodexSocket(snapshot, worker.Socket) {
		return Unconfirmed("codex app-server socket changed during observation")
	}
	stopCancel := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeWebSocket()
		case <-stopCancel:
		}
	}()
	defer close(stopCancel)
	client := codexRPCClient{ws: ws, nextID: 1, deadline: deadline}
	return fn(ctx, &client)
}

func validateCodexObservationWorker(worker Worker) error {
	if worker.RunID == "" || worker.ThreadID == "" {
		return NotReady("codex observation requires a run and native thread identity")
	}
	if worker.Harness != "" && worker.Harness != HarnessCodex {
		return fmt.Errorf("codex observation requires a codex worker")
	}
	if !threadPattern.MatchString(worker.ThreadID) {
		return fmt.Errorf("codex thread id %s has unexpected characters", quoteTrunc(worker.ThreadID))
	}
	if worker.Worktree == "" || !filepath.IsAbs(worker.Worktree) || filepath.Clean(worker.Worktree) != worker.Worktree {
		return fmt.Errorf("codex observation requires an absolute clean worktree")
	}
	if worker.Socket == "" {
		return NotReady("no codex app-server socket recorded for this worker")
	}
	return nil
}

func codexInitialize(ctx context.Context, client *codexRPCClient) error {
	if _, err := client.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "forgectl", "version": "dev"},
	}); err != nil {
		return err
	}
	return client.notify(ctx, "initialized", map[string]any{})
}

func codexReadThread(ctx context.Context, client *codexRPCClient, worker Worker) (codexThread, error) {
	readResult, err := client.call(ctx, "thread/read", map[string]any{
		"threadId":     worker.ThreadID,
		"includeTurns": false,
	})
	if err != nil {
		return codexThread{}, err
	}
	var read codexThreadRead
	if err := json.Unmarshal(readResult, &read); err != nil {
		return codexThread{}, errors.New("codex thread/read returned malformed metadata")
	}
	thread := read.Thread
	if thread.ID != worker.ThreadID || thread.CWD != worker.Worktree || !safeCodexToken(thread.ModelProvider) || !safeCodexToken(thread.Model) {
		return codexThread{}, errors.New("codex thread metadata does not match the enrolled worker")
	}
	return thread, nil
}

type codexRPCClient struct {
	ws       *websocket.Conn
	nextID   int
	deadline time.Time
}

func (client *codexRPCClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	requestID := client.nextID
	client.nextID++
	request := codexRPCRequest{JSONRPC: codexObservationProtocolVersion, ID: &requestID, Method: method, Params: params}
	if err := client.send(ctx, request); err != nil {
		return nil, err
	}
	for range codexObservationMaxMessages {
		response, err := client.receive(ctx)
		if err != nil {
			return nil, err
		}
		if len(response.ID) == 0 || string(response.ID) == "null" {
			continue
		}
		var responseID int
		if err := json.Unmarshal(response.ID, &responseID); err != nil || responseID != requestID {
			return nil, errors.New("codex app-server returned an unexpected response")
		}
		if len(response.Error) != 0 && string(response.Error) != "null" {
			return nil, fmt.Errorf("codex app-server rejected %s", method)
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return nil, fmt.Errorf("codex app-server returned no result for %s", method)
		}
		return response.Result, nil
	}
	return nil, errors.New("codex app-server sent too many notifications")
}

func (client *codexRPCClient) notify(ctx context.Context, method string, params any) error {
	return client.send(ctx, codexRPCRequest{JSONRPC: codexObservationProtocolVersion, Method: method, Params: params})
}

func (client *codexRPCClient) send(ctx context.Context, request codexRPCRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.ws.SetWriteDeadline(client.deadline); err != nil {
		return errors.New("codex app-server write deadline unavailable")
	}
	if err := websocket.JSON.Send(client.ws, request); err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		var timeoutError net.Error
		if errors.As(err, &timeoutError) && timeoutError.Timeout() {
			return context.DeadlineExceeded
		}
		return errors.New("codex app-server request failed")
	}
	return nil
}

func (client *codexRPCClient) receive(ctx context.Context) (codexRPCResponse, error) {
	if err := ctx.Err(); err != nil {
		return codexRPCResponse{}, err
	}
	if err := client.ws.SetReadDeadline(client.deadline); err != nil {
		return codexRPCResponse{}, errors.New("codex app-server read deadline unavailable")
	}
	var payload []byte
	if err := websocket.Message.Receive(client.ws, &payload); err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return codexRPCResponse{}, contextError
		}
		var timeoutError net.Error
		if errors.As(err, &timeoutError) && timeoutError.Timeout() {
			return codexRPCResponse{}, context.DeadlineExceeded
		}
		if errors.Is(err, websocket.ErrFrameTooLarge) {
			return codexRPCResponse{}, errors.New("codex app-server response exceeds payload limit")
		}
		return codexRPCResponse{}, errors.New("codex app-server response failed")
	}
	if len(payload) == 0 || len(payload) > codexObservationMaxPayloadBytes {
		return codexRPCResponse{}, errors.New("codex app-server response exceeds payload limit")
	}
	var response codexRPCResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return codexRPCResponse{}, errors.New("codex app-server response is malformed")
	}
	if len(response.JSONRPC) != 0 {
		var protocol string
		if err := json.Unmarshal(response.JSONRPC, &protocol); err != nil || protocol != codexObservationProtocolVersion {
			return codexRPCResponse{}, errors.New("codex app-server response is malformed")
		}
	}
	return response, nil
}

type codexSocketSnapshot struct{ info os.FileInfo }

func inspectCodexSocket(path string) (codexSocketSnapshot, error) {
	if strings.Contains(path, "://") {
		return codexSocketSnapshot{}, errors.New("codex observation supports Unix sockets only")
	}
	if err := validateCodexSocketPath(path); err != nil {
		return codexSocketSnapshot{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return codexSocketSnapshot{}, NotReady("codex app-server socket is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || !codexOwnedByCurrentUser(info) {
		return codexSocketSnapshot{}, errors.New("codex app-server socket is not a private owned Unix socket")
	}
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm()&0o077 != 0 || !codexOwnedByCurrentUser(dirInfo) {
		return codexSocketSnapshot{}, errors.New("codex app-server socket directory is not private and owned")
	}
	return codexSocketSnapshot{info: info}, nil
}

func sameCodexSocket(snapshot codexSocketSnapshot, path string) bool {
	info, err := os.Lstat(path)
	return err == nil && os.SameFile(snapshot.info, info)
}

func codexWebSocket(ctx context.Context, conn net.Conn, deadline time.Time) (*websocket.Conn, error) {
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	config := &websocket.Config{
		Location: &url.URL{Scheme: "ws", Host: "localhost", Path: "/"},
		Origin:   &url.URL{Scheme: "http", Host: "localhost"},
		Version:  websocket.ProtocolVersionHybi13,
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopped:
		}
	}()
	ws, err := websocket.NewClient(config, conn)
	close(stopped)
	if err != nil {
		return nil, err
	}
	ws.MaxPayloadBytes = codexObservationMaxPayloadBytes
	return ws, nil
}

func codexSettledTurn(native codexTurn) (*CodexTurnObservation, error) {
	if !threadPattern.MatchString(native.ID) {
		return nil, errors.New("codex turn metadata has an invalid identity")
	}
	var status string
	switch native.Status {
	case "completed", "failed":
		status = native.Status
	case "interrupted":
		status = "incomplete"
	case "inProgress":
		return nil, nil
	default:
		return nil, errors.New("codex turn metadata has an invalid status")
	}
	if native.Status == "completed" && len(native.Error) != 0 && string(native.Error) != "null" {
		return nil, errors.New("codex completed turn contains an error")
	}
	turn := &CodexTurnObservation{ID: native.ID, Status: status}
	if len(native.CompletedAt) != 0 && string(native.CompletedAt) != "null" {
		var seconds int64
		if err := json.Unmarshal(native.CompletedAt, &seconds); err != nil || seconds < 0 {
			return nil, errors.New("codex turn metadata has an invalid completion time")
		}
		turn.ObservedAt = time.Unix(seconds, 0).UTC()
	}
	return turn, nil
}

func codexActivitySnapshot(turns []codexTurn, observedAt time.Time) (activity.Snapshot, error) {
	snapshot := activity.Snapshot{
		Version:    activity.Version,
		Source:     activity.SourceCodex,
		Coverage:   activity.CoverageNativeHistory,
		ObservedAt: observedAt,
		Spans:      make([]activity.Span, 0, len(turns)),
	}
	seen := make(map[string]struct{}, len(turns))
	for _, native := range turns {
		if !threadPattern.MatchString(native.ID) {
			return activity.Snapshot{}, errors.New("codex activity has an invalid turn identity")
		}
		if _, duplicate := seen[native.ID]; duplicate {
			return activity.Snapshot{}, errors.New("codex history repeats a turn identity")
		}
		seen[native.ID] = struct{}{}
		startedAt, hasStartedAt, err := codexUnixTime(native.StartedAt)
		if err != nil {
			return activity.Snapshot{}, err
		}
		endedAt, hasEndedAt, err := codexUnixTime(native.CompletedAt)
		if err != nil {
			return activity.Snapshot{}, err
		}
		state, err := codexActivityState(native.Status)
		if err != nil {
			return activity.Snapshot{}, err
		}
		if state == activity.Finished && len(native.Error) != 0 && string(native.Error) != "null" {
			return activity.Snapshot{}, errors.New("codex completed turn contains an error")
		}
		if hasStartedAt && startedAt.After(observedAt.Add(time.Second)) || hasEndedAt && endedAt.After(observedAt.Add(time.Second)) {
			return activity.Snapshot{}, errors.New("codex activity has a future timestamp")
		}
		if !hasStartedAt || (state != activity.Running && !hasEndedAt) {
			snapshot.Partial = true
			continue
		}
		span := activity.Span{ID: native.ID, TurnID: native.ID, StartedAt: startedAt, State: state}
		if hasEndedAt {
			span.EndedAt = &endedAt
		}
		if err := activity.ValidateSpan(span); err != nil {
			return activity.Snapshot{}, err
		}
		snapshot.Spans = append(snapshot.Spans, span)
	}
	if err := activity.Validate(snapshot); err != nil {
		return activity.Snapshot{}, err
	}
	return snapshot, nil
}

func codexActivityState(status string) (activity.State, error) {
	switch status {
	case "inProgress":
		return activity.Running, nil
	case "completed":
		return activity.Finished, nil
	case "failed":
		return activity.Failed, nil
	case "interrupted":
		return activity.Incomplete, nil
	default:
		return "", errors.New("codex activity has an invalid turn status")
	}
}

func codexUnixTime(raw json.RawMessage) (time.Time, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false, nil
	}
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err != nil || seconds < 0 {
		return time.Time{}, false, errors.New("codex activity has an invalid Unix timestamp")
	}
	value := time.Unix(seconds, 0).UTC()
	if value.Year() < 1970 || value.Year() > 9999 {
		return time.Time{}, false, errors.New("codex activity has an out-of-range Unix timestamp")
	}
	return value, true, nil
}

func safeCodexToken(value string) bool {
	if value == "" || len(value) > codexObservationMaxTokenBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || unicode.In(character, unicode.Cf) {
			return false
		}
	}
	return true
}
