package goaltools

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrokerValidPrivateChannelAndCapability(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	called := make(chan string, 1)
	server, err := Start(context.Background(), socket, capabilityFile, func(_ context.Context, operation string, arguments json.RawMessage) (any, error) {
		called <- operation
		if string(arguments) != `{"path":"README.md"}` {
			t.Errorf("arguments = %s", arguments)
		}
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)

	capability, err := ReadCapability(capabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(capability) != 64 {
		t.Fatalf("capability length = %d", len(capability))
	}
	assertPrivateRegularFile(t, capabilityFile, 0o600)
	assertPrivateSocket(t, socket)

	result, err := Call(context.Background(), socket, capability, "goal_read", json.RawMessage(`{"path":"README.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("result = %s", result)
	}
	select {
	case operation := <-called:
		if operation != "goal_read" {
			t.Fatalf("operation = %q", operation)
		}
	case <-time.After(time.Second):
		t.Fatal("handler was not called")
	}
}

func TestBrokerRejectsWrongCrossRoleAndStaleCapabilities(t *testing.T) {
	firstRoot := privateTestDir(t)
	secondRoot := privateTestDir(t)
	firstSocket := filepath.Join(firstRoot, "tools.sock")
	firstCapabilityFile := filepath.Join(firstRoot, "capability")
	secondSocket := filepath.Join(secondRoot, "tools.sock")
	secondCapabilityFile := filepath.Join(secondRoot, "capability")
	var secondCalls atomic.Int32
	first, err := Start(context.Background(), firstSocket, firstCapabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		return "first", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, first)
	second, err := Start(context.Background(), secondSocket, secondCapabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		secondCalls.Add(1)
		return "second", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, second)
	firstCapability, err := ReadCapability(firstCapabilityFile)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Call(context.Background(), secondSocket, firstCapability, "goal_read", json.RawMessage(`{"path":"x"}`)); err == nil || !strings.Contains(err.Error(), "capability or version mismatch") {
		t.Fatalf("cross-role capability error = %v", err)
	}
	if got := secondCalls.Load(); got != 0 {
		t.Fatalf("cross-role handler calls = %d", got)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(context.Background(), firstSocket, firstCapability, "goal_read", json.RawMessage(`{"path":"x"}`)); err == nil || !strings.Contains(err.Error(), "channel is unavailable") {
		t.Fatalf("stale capability error = %v", err)
	}
}

func TestReadCapabilityRejectsSymlinkAndPublicDirectory(t *testing.T) {
	root := privateTestDir(t)
	file := filepath.Join(root, "capability")
	if err := os.WriteFile(file, bytes.Repeat([]byte("a"), 64), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapability(file); err != nil {
		t.Fatalf("private regular capability was refused: %v", err)
	}
	link := filepath.Join(root, "linked-capability")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapability(link); err == nil {
		t.Fatal("symlink capability was accepted")
	}
	parent := privateTestDir(t)
	linkedRoot := filepath.Join(parent, "linked-root")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapability(filepath.Join(linkedRoot, "capability")); err == nil {
		t.Fatal("symlink capability directory was accepted")
	}
	if err := os.Chmod(root, 0o755); err != nil { //nolint:gosec // Deliberately public directory is a negative control.
		t.Fatal(err)
	}
	if _, err := ReadCapability(file); err == nil {
		t.Fatal("public capability directory was accepted")
	}
}

func TestBrokerDuplicateRequestIDIsExecutedExactlyOnce(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	var calls atomic.Int32
	server, err := Start(context.Background(), socket, capabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		calls.Add(1)
		return "accepted", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)
	capability, err := ReadCapability(capabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Version: 1, Capability: capability, RequestID: strings.Repeat("a", 32), Operation: "goal_exec", Arguments: json.RawMessage(`{"argv":["true"]}`)}
	first := rawBrokerRequest(t, socket, request)
	if first.Error != "" || string(first.Result) != `"accepted"` {
		t.Fatalf("first response = %+v", first)
	}
	second := rawBrokerRequest(t, socket, request)
	if !strings.Contains(second.Error, "repeated") {
		t.Fatalf("duplicate response = %+v", second)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d", got)
	}
}

func TestBrokerRejectsMalformedOversizeUnknownPropertiesAndOperations(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	var calls atomic.Int32
	server, err := Start(context.Background(), socket, capabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		calls.Add(1)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)
	capability, err := ReadCapability(capabilityFile)
	if err != nil {
		t.Fatal(err)
	}

	var request Request
	for _, input := range [][]byte{[]byte("{"), bytes.Repeat([]byte("x"), maxFrame+1), []byte(`{"version":1,"capability":"x","unexpected":true}`), []byte(`{"version":1} {}`)} {
		if err := Decode(input, &request); err == nil {
			t.Fatalf("Decode accepted invalid input of length %d", len(input))
		}
	}
	unknown := rawBrokerRequest(t, socket, Request{Version: 1, Capability: capability, RequestID: strings.Repeat("b", 32), Operation: "goal_unknown", Arguments: json.RawMessage(`{}`)})
	if !strings.Contains(unknown.Error, "identity or operation is invalid") {
		t.Fatalf("unknown operation response = %+v", unknown)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid request handler calls = %d", got)
	}
}

func TestBrokerCloseRemovesOwnedSocket(t *testing.T) {
	socketRoot, controlRoot := privateTestDir(t), privateTestDir(t)
	socket, capabilityFile := filepath.Join(socketRoot, "tools.sock"), filepath.Join(controlRoot, "capability")
	server, err := Start(t.Context(), socket, capabilityFile, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("owned socket was not created: %v", err)
	}
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("owned socket survived close: %v", err)
	}
	if err := os.Remove(socketRoot); err != nil {
		t.Fatalf("closed broker left its socket directory nonempty: %v", err)
	}
	assertPrivateRegularFile(t, capabilityFile, 0o600)
}

func TestBrokerCloseCancelsAndDrainsHandler(t *testing.T) {
	root := privateTestDir(t)
	socket := filepath.Join(root, "tools.sock")
	capabilityFile := filepath.Join(root, "capability")
	started := make(chan struct{})
	server, err := Start(context.Background(), socket, capabilityFile, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ReadCapability(capabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	callDone := make(chan error, 1)
	go func() {
		_, callErr := Call(context.Background(), socket, capability, "goal_inbox", json.RawMessage(`{}`))
		callDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("call did not drain")
	}
}

func TestBrokerClientCancellationReachesInFlightHandler(t *testing.T) {
	root := privateTestDir(t)
	socket, capabilityFile := filepath.Join(root, "tools.sock"), filepath.Join(root, "capability")
	started, stopped := make(chan struct{}), make(chan struct{})
	server, err := Start(t.Context(), socket, capabilityFile, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, server)
	capability, err := ReadCapability(capabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, err := Call(ctx, socket, capability, "goal_exec", json.RawMessage(`{"argv":["probe"]}`))
		callDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request never reached the handler")
	}
	cancel()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("canceled request reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not close its channel")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("handler survived client cancellation while server remained open")
	}
	server.mu.Lock()
	closed := server.closed
	server.mu.Unlock()
	if closed {
		t.Fatal("test canceled the server instead of the individual request")
	}
}

func rawBrokerRequest(t *testing.T, socket string, request Request) Response {
	t.Helper()
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func closeServer(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertPrivateRegularFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		t.Fatalf("%s mode/type = %s/%s", path, info.Mode().Perm(), info.Mode().Type())
	}
}

func assertPrivateSocket(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode/type = %s/%s", info.Mode().Perm(), info.Mode().Type())
	}
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "gt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}
