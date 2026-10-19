package goaltools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxFrame = 1 << 20

type Handler func(context.Context, string, json.RawMessage) (any, error)

type Request struct {
	Version    int             `json:"version"`
	Capability string          `json:"capability"`
	RequestID  string          `json:"request_id"`
	Operation  string          `json:"operation"`
	Arguments  json.RawMessage `json:"arguments"`
}

type Response struct {
	Version   int             `json:"version"`
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type Server struct {
	listener    net.Listener
	capability  string
	handler     Handler
	context     context.Context
	cancel      context.CancelFunc
	wait        sync.WaitGroup
	mu          sync.Mutex
	connections map[net.Conn]bool
	seen        map[string]bool
	closed      bool
}

func Start(ctx context.Context, socket, capabilityFile string, handler Handler) (*Server, error) {
	if handler == nil {
		return nil, errors.New("goal tool handler is required")
	}
	for _, path := range []string{socket, capabilityFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errors.New("goal tool control paths must be absolute and clean")
		}
		info, err := os.Lstat(filepath.Dir(path))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return nil, errors.New("goal tool control directory must be private")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("goal tool control path already exists or cannot be inspected")
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	capability := hex.EncodeToString(secret)
	directory, err := capabilityDirectory(capabilityFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	file, err := directory.OpenFile(filepath.Base(capabilityFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.WriteString(capability)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	server := &Server{listener: listener, capability: capability, handler: handler, context: serverCtx, cancel: cancel, connections: make(map[net.Conn]bool), seen: make(map[string]bool)}
	server.wait.Add(1)
	go server.serve()
	return server, nil
}

func (server *Server) serve() {
	defer server.wait.Done()
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}
		server.mu.Lock()
		if server.closed || len(server.connections) >= 8 {
			server.mu.Unlock()
			_ = connection.Close()
			continue
		}
		server.connections[connection] = true
		server.wait.Add(1)
		server.mu.Unlock()
		go server.handle(connection)
	}
}

func (server *Server) handle(connection net.Conn) {
	defer server.wait.Done()
	defer func() {
		_ = connection.Close()
		server.mu.Lock()
		delete(server.connections, connection)
		server.mu.Unlock()
	}()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Minute)); err != nil {
		return
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 8192), maxFrame+1)
	if !scanner.Scan() {
		return
	}
	var request Request
	response := Response{Version: 1}
	if err := Decode(scanner.Bytes(), &request); err != nil {
		response.Error = "invalid goal tool request"
	} else {
		response.RequestID = request.RequestID
		requestContext, cancel := context.WithTimeout(server.context, 3*time.Minute)
		readDone := make(chan struct{})
		go func() {
			_ = scanner.Scan()
			cancel()
			close(readDone)
		}()
		defer func() {
			cancel()
			_ = connection.Close()
			<-readDone
		}()
		result, err := server.dispatch(requestContext, request)
		if err != nil {
			response.Error = err.Error()
			if len(response.Error) > 2048 {
				response.Error = response.Error[:2048]
			}
		} else {
			// termsafe:allow-raw-json private role tool response, never terminal output
			response.Result, err = json.Marshal(result)
			if err != nil || len(response.Result) > maxFrame/2 {
				response.Result = nil
				response.Error = "goal tool result exceeded its bound"
			}
		}
	}
	// termsafe:allow-raw-json private role tool socket, never terminal output
	_ = json.NewEncoder(connection).Encode(response)
}

func (server *Server) dispatch(ctx context.Context, request Request) (any, error) {
	if request.Version != 1 || subtle.ConstantTimeCompare([]byte(request.Capability), []byte(server.capability)) != 1 {
		return nil, errors.New("goal tool capability or version mismatch")
	}
	identifier, err := hex.DecodeString(request.RequestID)
	if err != nil || len(identifier) != 16 || hex.EncodeToString(identifier) != request.RequestID || !KnownTool(request.Operation) {
		return nil, errors.New("goal tool request identity or operation is invalid")
	}
	server.mu.Lock()
	if server.closed || server.seen[request.RequestID] || len(server.seen) >= 4096 {
		server.mu.Unlock()
		return nil, errors.New("goal tool request is closed, repeated or exceeds the run bound")
	}
	server.seen[request.RequestID] = true
	server.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return server.handler(ctx, request.Operation, request.Arguments)
}

func (server *Server) Close(ctx context.Context) error {
	server.mu.Lock()
	server.closed = true
	server.cancel()
	_ = server.listener.Close()
	for connection := range server.connections {
		_ = connection.Close()
	}
	server.mu.Unlock()
	done := make(chan struct{})
	go func() { server.wait.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("goal tool requests did not drain before cleanup deadline")
	}
}

func ReadCapability(path string) (string, error) {
	directory, err := capabilityDirectory(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = directory.Close() }()
	name := filepath.Base(path)
	info, err := directory.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 64 {
		return "", errors.New("goal tool capability must be a private 64-byte regular file")
	}
	file, err := directory.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o600 || opened.Size() != 64 {
		return "", errors.New("goal tool capability changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil || len(data) != 64 {
		return "", errors.New("goal tool capability changed while reading")
	}
	decoded, err := hex.DecodeString(string(data))
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != string(data) {
		return "", errors.New("goal tool capability is malformed")
	}
	return string(data), nil
}

func capabilityDirectory(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("goal tool capability path must be absolute and clean")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("goal tool capability directory must be private")
	}
	directory, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o700 {
		_ = directory.Close()
		return nil, errors.New("goal tool capability directory changed while opening")
	}
	return directory, nil
}

func Call(ctx context.Context, socket, capability, operation string, arguments json.RawMessage) (json.RawMessage, error) {
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return nil, err
	}
	request := Request{Version: 1, Capability: capability, RequestID: hex.EncodeToString(identifier), Operation: operation, Arguments: arguments}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errors.New("goal tool channel is unavailable; request was not sent")
	}
	defer func() { _ = connection.Close() }()
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()
	deadline := time.Now().Add(3 * time.Minute)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// termsafe:allow-raw-json private role tool socket, never terminal output
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return nil, errors.New("goal tool delivery is unconfirmed; do not replay")
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 8192), maxFrame+1)
	if !scanner.Scan() {
		return nil, errors.New("goal tool result is unconfirmed; do not replay")
	}
	var response Response
	if err := Decode(scanner.Bytes(), &response); err != nil || response.Version != 1 || response.RequestID != request.RequestID {
		return nil, errors.New("goal tool response correlation is invalid")
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if len(response.Result) == 0 {
		return nil, errors.New("goal tool response omitted its result")
	}
	return response.Result, nil
}

func Decode(data []byte, destination any) error {
	if len(data) == 0 || len(data) > maxFrame {
		return errors.New("goal tool frame exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("goal tool frame contains trailing data")
	}
	return nil
}
