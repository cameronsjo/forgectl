//go:build aix || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type codexObservationFake struct {
	path       string
	listener   net.Listener
	turn       map[string]any
	threadID   string
	threadCWD  string
	model      string
	provider   string
	jsonRPC    *string
	delay      time.Duration
	large      bool
	methodsMu  sync.Mutex
	methods    []string
	params     map[string][]json.RawMessage
	serverDone chan struct{}
}

func newCodexObservationFake(test *testing.T) *codexObservationFake {
	test.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "fctl-cdx-")
	if err != nil {
		test.Fatalf("temp dir: %v", err)
	}
	test.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "codex.sock")
	listener, err := (&net.ListenConfig{}).Listen(test.Context(), "unix", path)
	if err != nil {
		test.Fatalf("listen: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		test.Fatalf("chmod socket: %v", err)
	}
	fake := &codexObservationFake{
		path:       path,
		listener:   listener,
		threadID:   "thread-1",
		threadCWD:  dir,
		model:      "test-model",
		provider:   "openai-codex",
		params:     make(map[string][]json.RawMessage),
		serverDone: make(chan struct{}),
	}
	protocol := codexObservationProtocolVersion
	fake.jsonRPC = &protocol
	server := &http.Server{Handler: websocket.Server{Handler: websocket.Handler(fake.handle)}, ReadHeaderTimeout: time.Second}
	go func() {
		_ = server.Serve(listener)
		close(fake.serverDone)
	}()
	test.Cleanup(func() {
		_ = listener.Close()
		<-fake.serverDone
	})
	return fake
}

func (fake *codexObservationFake) handle(ws *websocket.Conn) {
	defer func() { _ = ws.Close() }()
	for {
		var fields map[string]json.RawMessage
		if err := websocket.JSON.Receive(ws, &fields); err != nil {
			return
		}
		var method string
		_ = json.Unmarshal(fields["method"], &method)
		fake.methodsMu.Lock()
		fake.methods = append(fake.methods, method)
		if rawParams, ok := fields["params"]; ok {
			fake.params[method] = append(fake.params[method], append(json.RawMessage(nil), rawParams...))
		}
		fake.methodsMu.Unlock()
		var requestID int
		_ = json.Unmarshal(fields["id"], &requestID)
		if method == "initialized" {
			continue
		}
		if fake.delay > 0 {
			time.Sleep(fake.delay)
		}
		if fake.large {
			_ = websocket.Message.Send(ws, strings.Repeat("x", codexObservationMaxPayloadBytes+1))
			return
		}
		var result any
		switch method {
		case "initialize":
			result = map[string]any{}
		case "thread/read":
			result = map[string]any{"thread": map[string]any{
				"id": fake.threadID, "cwd": fake.threadCWD, "model": fake.model, "modelProvider": fake.provider,
			}}
		case "thread/turns/list":
			if fake.turn == nil {
				result = map[string]any{"data": []any{}}
			} else {
				result = map[string]any{"data": []any{fake.turn}}
			}
		default:
			return
		}
		response := map[string]any{"id": requestID, "result": result}
		if fake.jsonRPC != nil {
			response["jsonrpc"] = *fake.jsonRPC
		}
		_ = websocket.JSON.Send(ws, response)
	}
}

func (fake *codexObservationFake) worker() Worker {
	return Worker{Harness: HarnessCodex, RunID: "run-1", ThreadID: fake.threadID, Worktree: fake.threadCWD, Socket: fake.path}
}

func (fake *codexObservationFake) seenMethods() []string {
	fake.methodsMu.Lock()
	defer fake.methodsMu.Unlock()
	return append([]string(nil), fake.methods...)
}

func (fake *codexObservationFake) seenParams(method string) []json.RawMessage {
	fake.methodsMu.Lock()
	defer fake.methodsMu.Unlock()
	return append([]json.RawMessage(nil), fake.params[method]...)
}

func TestCodexObservationReadsNativeMetadataWithoutMutation(test *testing.T) {
	fake := newCodexObservationFake(test)
	fake.turn = map[string]any{"id": "turn-1", "status": "completed", "completedAt": int64(1700000000), "error": nil}
	observation, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), fake.worker())
	if err != nil {
		test.Fatalf("Inspect: %v", err)
	}
	if observation.NativeSessionID != fake.threadID || observation.Provider != fake.provider || observation.Model != fake.model || observation.CWD != fake.threadCWD {
		test.Fatalf("observation = %#v", observation)
	}
	if observation.Turn == nil || observation.Turn.Status != "completed" || observation.Turn.ObservedAt.IsZero() {
		test.Fatalf("turn = %#v", observation.Turn)
	}
	want := []string{"initialize", "initialized", "thread/read", "thread/turns/list"}
	if got := fake.seenMethods(); fmt.Sprint(got) != fmt.Sprint(want) {
		test.Fatalf("methods = %v, want %v", got, want)
	}
	readParams := fake.seenParams("thread/read")
	if len(readParams) != 1 {
		test.Fatalf("thread/read params = %v", readParams)
	}
	var readRequest struct {
		ThreadID     string `json:"threadId"`
		IncludeTurns bool   `json:"includeTurns"`
	}
	if err := json.Unmarshal(readParams[0], &readRequest); err != nil || readRequest.ThreadID != fake.threadID || readRequest.IncludeTurns {
		test.Fatalf("thread/read params = %s", readParams[0])
	}
	turnsParams := fake.seenParams("thread/turns/list")
	if len(turnsParams) != 1 {
		test.Fatalf("thread/turns/list params = %v", turnsParams)
	}
	var turnsRequest struct {
		ThreadID      string `json:"threadId"`
		Limit         int    `json:"limit"`
		SortDirection string `json:"sortDirection"`
		ItemsView     string `json:"itemsView"`
	}
	if err := json.Unmarshal(turnsParams[0], &turnsRequest); err != nil || turnsRequest.ThreadID != fake.threadID || turnsRequest.Limit != 1 || turnsRequest.SortDirection != "desc" || turnsRequest.ItemsView != "notLoaded" {
		test.Fatalf("thread/turns/list params = %s", turnsParams[0])
	}
}

func TestCodexObservationAcceptsNativeOmittedJSONRPC(test *testing.T) {
	fake := newCodexObservationFake(test)
	fake.jsonRPC = nil
	if _, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), fake.worker()); err != nil {
		test.Fatalf("Inspect: %v", err)
	}
}

func TestCodexObservationRejectsWrongJSONRPC(test *testing.T) {
	fake := newCodexObservationFake(test)
	protocol := "1.0"
	fake.jsonRPC = &protocol
	if _, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), fake.worker()); err == nil {
		test.Fatal("Inspect unexpectedly accepted an invalid JSON-RPC version")
	}
}

func TestCodexObservationRejectsIdentityAndTurnEvidence(test *testing.T) {
	tests := []struct {
		name string
		edit func(*codexObservationFake)
	}{
		{name: "wrong thread", edit: func(fake *codexObservationFake) { fake.threadID = "other-thread" }},
		{name: "wrong cwd", edit: func(fake *codexObservationFake) { fake.threadCWD = filepath.Join(fake.threadCWD, "other") }},
		{name: "unknown status", edit: func(fake *codexObservationFake) { fake.turn = map[string]any{"id": "turn-1", "status": "mystery"} }},
		{name: "completed error", edit: func(fake *codexObservationFake) {
			fake.turn = map[string]any{"id": "turn-1", "status": "completed", "error": map[string]any{"message": "secret"}}
		}},
	}
	for _, caseTest := range tests {
		test.Run(caseTest.name, func(subtest *testing.T) {
			fake := newCodexObservationFake(subtest)
			worker := fake.worker()
			caseTest.edit(fake)
			if _, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), worker); err == nil {
				subtest.Fatal("Inspect unexpectedly succeeded")
			}
		})
	}
}

func TestCodexObservationRunningTurnHasNoSettledEvidence(test *testing.T) {
	fake := newCodexObservationFake(test)
	fake.turn = map[string]any{"id": "turn-1", "status": "inProgress"}
	observation, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), fake.worker())
	if err != nil {
		test.Fatalf("Inspect: %v", err)
	}
	if observation.Turn != nil {
		test.Fatalf("running turn = %#v, want no settled evidence", observation.Turn)
	}
}

func TestCodexObservationBoundsPayloadAndCancellation(test *testing.T) {
	test.Run("oversize", func(test *testing.T) {
		fake := newCodexObservationFake(test)
		fake.large = true
		if _, err := (CodexAdapter{Timeout: time.Second}).Inspect(context.Background(), fake.worker()); err == nil {
			test.Fatal("Inspect unexpectedly accepted oversized response")
		}
	})
	test.Run("cancel", func(test *testing.T) {
		fake := newCodexObservationFake(test)
		fake.delay = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := (CodexAdapter{Timeout: time.Second}).Inspect(ctx, fake.worker()); !errors.Is(err, context.Canceled) {
			test.Fatalf("Inspect error = %v, want cancellation", err)
		}
	})
	test.Run("timeout", func(test *testing.T) {
		fake := newCodexObservationFake(test)
		fake.delay = 100 * time.Millisecond
		if _, err := (CodexAdapter{Timeout: 10 * time.Millisecond}).Inspect(context.Background(), fake.worker()); err == nil {
			test.Fatal("Inspect unexpectedly succeeded after timeout")
		}
	})
}

func TestCodexObservationRejectsNonUnixAndUnsafeSocket(test *testing.T) {
	adapter := CodexAdapter{Timeout: time.Second}
	test.Run("non-unix", func(test *testing.T) {
		worker := Worker{Harness: HarnessCodex, RunID: "run-1", ThreadID: "thread-1", Worktree: "/tmp/work", Socket: "ws://host/socket"}
		if _, err := adapter.Inspect(context.Background(), worker); err == nil {
			test.Fatal("Inspect accepted a non-Unix endpoint")
		}
	})
	test.Run("regular file", func(test *testing.T) {
		dir := test.TempDir()
		path := filepath.Join(dir, "codex.sock")
		if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
			test.Fatal(err)
		}
		worker := Worker{Harness: HarnessCodex, RunID: "run-1", ThreadID: "thread-1", Worktree: "/tmp/work", Socket: path}
		if _, err := adapter.Inspect(context.Background(), worker); err == nil {
			test.Fatal("Inspect accepted a regular file")
		}
	})
	test.Run("symlink", func(test *testing.T) {
		dir := test.TempDir()
		target := filepath.Join(dir, "target")
		path := filepath.Join(dir, "codex.sock")
		if err := os.WriteFile(target, []byte("not a socket"), 0o600); err != nil {
			test.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			test.Fatal(err)
		}
		worker := Worker{Harness: HarnessCodex, RunID: "run-1", ThreadID: "thread-1", Worktree: "/tmp/work", Socket: path}
		if _, err := adapter.Inspect(context.Background(), worker); err == nil {
			test.Fatal("Inspect accepted a symlink")
		}
	})
	test.Run("public parent", func(test *testing.T) {
		dir, err := os.MkdirTemp("/tmp", "fctl-cdx-")
		if err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() { _ = os.RemoveAll(dir) })
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec
			test.Fatal(err)
		}
		path := filepath.Join(dir, "codex.sock")
		listener, err := (&net.ListenConfig{}).Listen(test.Context(), "unix", path)
		if err != nil {
			test.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		if err := os.Chmod(path, 0o600); err != nil {
			test.Fatal(err)
		}
		worker := Worker{Harness: HarnessCodex, RunID: "run-1", ThreadID: "thread-1", Worktree: "/tmp/work", Socket: path}
		if _, err := adapter.Inspect(context.Background(), worker); err == nil {
			test.Fatal("Inspect accepted a socket in a public directory")
		}
	})
}
