//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package goalnative

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type ProcessConfig struct {
	Binary           string
	SupervisorBinary string
	ControlDir       string
	Cwd              string
	Socket           string
	Arguments        []string
	Environment      []string
	OnMessage        func(json.RawMessage)
}

type JSONProcess struct {
	owner     *Session
	conn      net.Conn
	done      chan struct{}
	writeMu   sync.Mutex
	mu        sync.Mutex
	readErr   error
	closeOnce sync.Once
}

func StartJSONProcess(ctx context.Context, config ProcessConfig) (*JSONProcess, error) {
	validation := Config{Binary: config.Binary, SupervisorBinary: config.SupervisorBinary, ControlDir: config.ControlDir, Cwd: config.Cwd, Socket: config.Socket, WritableRoots: []string{config.Cwd}, ExecutionOnly: true}
	if err := validateConfig(validation); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.ControlDir, 0o700); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(config.ControlDir); err != nil {
		return nil, err
	}
	if err := validateSocketDestination(config.Socket); err != nil {
		return nil, err
	}
	spec := hostSpec{Binary: config.Binary, ControlDir: config.ControlDir, Cwd: config.Cwd, Socket: config.Socket, Protocol: "json-lines", Arguments: append([]string(nil), config.Arguments...), WritableRoots: []string{config.Cwd}, Receipt: filepath.Join(config.ControlDir, "guardian-receipt.json")}
	if err := validateHostSpec(spec); err != nil {
		return nil, err
	}
	specPath := filepath.Join(config.ControlDir, "process-spec.json")
	if err := writePrivateJSON(specPath, spec); err != nil {
		return nil, err
	}
	leaseReader, leaseWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	child, err := startSupervisor(config.SupervisorBinary, specPath, leaseReader, config.Cwd, config.Environment)
	_ = leaseReader.Close()
	if err != nil {
		_ = leaseWriter.Close()
		return nil, err
	}
	owner := &Session{config: validation, lease: leaseWriter, child: child, childDone: make(chan struct{}), done: make(chan struct{})}
	go func() {
		state, waitErr := child.Wait()
		if waitErr == nil && (state == nil || !state.Success()) {
			waitErr = errors.New("native JSON guardian exited unsuccessfully")
		}
		owner.childMu.Lock()
		owner.childErr = waitErr
		owner.childMu.Unlock()
		close(owner.childDone)
	}()
	if err := waitForUnixSocket(ctx, config.Socket, child, startupTimeout); err != nil {
		return nil, startupFailure(owner, err)
	}
	dialer := net.Dialer{Timeout: startupTimeout}
	connection, err := dialer.DialContext(ctx, "unix", config.Socket)
	if err != nil {
		return nil, startupFailure(owner, err)
	}
	process := &JSONProcess{owner: owner, conn: connection, done: make(chan struct{})}
	go process.read(config.OnMessage)
	return process, nil
}

func (process *JSONProcess) Write(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxPayloadBytes {
		return errors.New("native JSON input is invalid or exceeds its bound")
	}
	process.writeMu.Lock()
	defer process.writeMu.Unlock()
	deadline := time.Now().Add(rpcTimeout)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	if err := process.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err = process.conn.Write(append(data, '\n'))
	return err
}

func (process *JSONProcess) read(observe func(json.RawMessage)) {
	defer close(process.done)
	scanner := bufio.NewScanner(process.conn)
	scanner.Buffer(make([]byte, 8192), maxPayloadBytes+1)
	for scanner.Scan() {
		data := append(json.RawMessage(nil), scanner.Bytes()...)
		if !json.Valid(data) || len(data) > maxPayloadBytes {
			process.mu.Lock()
			process.readErr = errors.New("native JSON output is malformed or exceeds its bound")
			process.mu.Unlock()
			_ = process.conn.Close()
			return
		}
		if observe != nil {
			observe(data)
		}
	}
	process.mu.Lock()
	process.readErr = scanner.Err()
	if process.readErr == nil {
		process.readErr = io.EOF
	}
	process.mu.Unlock()
}

func (process *JSONProcess) Done() <-chan struct{} { return process.done }

func (process *JSONProcess) Err() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.readErr
}

func (process *JSONProcess) Close(ctx context.Context) error {
	process.closeOnce.Do(func() { _ = process.conn.Close() })
	return process.owner.Close(ctx)
}

type processBridge struct {
	listener *net.UnixListener
	input    io.WriteCloser
	output   io.ReadCloser
	mu       sync.Mutex
	conn     net.Conn
	closed   bool
}

func bridgeProcess(socket string, command *exec.Cmd) (*processBridge, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen for owned native process: %w", err)
	}
	bridge := &processBridge{listener: listener}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = bridge.Close()
		return nil, err
	}
	command.Stdout = nil
	bridge.input, err = command.StdinPipe()
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	bridge.output, err = command.StdoutPipe()
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	go bridge.serve()
	return bridge, nil
}

func (bridge *processBridge) serve() {
	connection, err := bridge.listener.AcceptUnix()
	if err != nil {
		return
	}
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		_ = connection.Close()
		return
	}
	bridge.conn = connection
	bridge.mu.Unlock()
	_ = bridge.listener.Close()
	go func() {
		_, _ = io.Copy(bridge.input, connection)
		_ = bridge.input.Close()
	}()
	_, _ = io.Copy(connection, bridge.output)
	_ = connection.Close()
}

func (bridge *processBridge) Close() error {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	bridge.closed = true
	if bridge.listener != nil {
		_ = bridge.listener.Close()
	}
	if bridge.conn != nil {
		_ = bridge.conn.Close()
	}
	if bridge.input != nil {
		_ = bridge.input.Close()
	}
	if bridge.output != nil {
		_ = bridge.output.Close()
	}
	return nil
}
