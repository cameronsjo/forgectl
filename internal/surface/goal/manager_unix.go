//go:build unix

package goal

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"golang.org/x/sys/unix"
)

type Manager struct {
	Store       Store
	SelfBinary  string
	CodexBinary string
}

type binaryReceipt struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type runtimeReceipt struct {
	Version  int           `json:"version"`
	GoalID   string        `json:"goal_id"`
	Forgectl binaryReceipt `json:"forgectl"`
	Codex    binaryReceipt `json:"codex"`
}

type readyReceipt struct {
	Version int    `json:"version"`
	GoalID  string `json:"goal_id"`
	Ready   bool   `json:"ready"`
}

func inspectBinary(path string) (binaryReceipt, error) {
	if !filepath.IsAbs(path) {
		return binaryReceipt{}, errors.New("native executables must resolve to absolute paths")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return binaryReceipt{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return binaryReceipt{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Size() > 1024*1024*1024 {
		return binaryReceipt{}, errors.New("native executable is not a bounded executable regular file")
	}
	file, err := os.Open(canonical)
	if err != nil {
		return binaryReceipt{}, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return binaryReceipt{}, errors.New("native executable changed while being opened")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, 1024*1024*1024+1))
	if err != nil || size != info.Size() {
		return binaryReceipt{}, errors.New("native executable changed or exceeded its size bound")
	}
	return binaryReceipt{Path: canonical, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (manager Manager) Start(ctx context.Context, approved Prepared) (State, error) {
	if approved.Spec.BaseRevision == "" {
		return State{}, errors.New("explicit approval must cover an exact source HEAD")
	}
	current, err := Prepare(ctx, approved.Spec)
	if err != nil {
		return State{}, err
	}
	self, err := inspectBinary(manager.SelfBinary)
	if err != nil {
		return State{}, fmt.Errorf("forgectl executable: %w", err)
	}
	codex, err := inspectBinary(manager.CodexBinary)
	if err != nil {
		return State{}, fmt.Errorf("codex executable: %w", err)
	}
	state, err := manager.Store.Create(ctx, current)
	if err != nil {
		return State{}, err
	}
	runtime := runtimeReceipt{Version: Version, GoalID: state.ID, Forgectl: self, Codex: codex}
	// termsafe:allow-raw-json private runtime receipt, never terminal output
	contents, err := json.Marshal(runtime)
	if err != nil {
		return state, err
	}
	if err := manager.Store.WriteArtifact(state.ID, "runtime.json", contents); err != nil {
		return state, err
	}
	directory, err := manager.Store.directory(state.ID, false)
	if err != nil {
		return state, err
	}
	defer func() { _ = unix.Close(directory) }()
	log, err := privdir.OpenFileChecked(directory, "supervisor.log", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return state, err
	}
	defer func() { _ = log.Close() }()
	command := exec.CommandContext(context.Background(), self.Path, "surface", "project", "goal", "_supervise", state.ID, "--root", manager.Store.Root, "--codex-binary", codex.Path) //nolint:gosec // G204: this verified executable starts the explicitly approved detached goal.
	command.Env = cleanEnvironment(os.Environ())
	command.Stdin = nil
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	readiness, err := command.StdoutPipe()
	if err != nil {
		return state, err
	}
	if err := command.Start(); err != nil {
		return state, manager.startFailed(state, err)
	}
	ready := make(chan error, 1)
	go func() {
		defer func() { _ = readiness.Close() }()
		line, err := bufio.NewReader(io.LimitReader(readiness, 4096)).ReadBytes('\n')
		if err == nil {
			var receipt readyReceipt
			err = decodeJSON(line, &receipt)
			if err == nil && (receipt.Version != Version || receipt.GoalID != state.ID || !receipt.Ready) {
				err = errors.New("supervisor readiness identity mismatch")
			}
		}
		ready <- err
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err == nil {
			return manager.Store.Load(ctx, state.ID)
		}
		_ = command.Process.Kill()
		return state, manager.startFailed(state, fmt.Errorf("supervisor did not acknowledge its lease; inspect %s: %w", filepath.Join(state.RunDir, "supervisor.log"), err))
	case err := <-wait:
		return state, manager.startFailed(state, fmt.Errorf("supervisor exited before readiness: %v", err))
	case <-ctx.Done():
		_ = command.Process.Kill()
		return state, manager.startFailed(state, ctx.Err())
	case <-timer.C:
		_ = command.Process.Kill()
		return state, manager.startFailed(state, errors.New("supervisor readiness timed out; retained state needs inspection"))
	}
}

func (manager Manager) startFailed(state State, cause error) error {
	state.Phase, state.Summary, state.Error = PhaseFailed, "Supervisor did not start", cause.Error()
	state.UpdatedAt = time.Now().UTC()
	return errors.Join(cause, manager.Store.Save(context.Background(), state))
}

func readRuntime(store Store, id, codex, self string) (runtimeReceipt, error) {
	directory, err := store.directory(id, false)
	if err != nil {
		return runtimeReceipt{}, err
	}
	defer func() { _ = unix.Close(directory) }()
	contents, err := readPrivate(directory, "runtime.json", 16*1024)
	if err != nil {
		return runtimeReceipt{}, err
	}
	var receipt runtimeReceipt
	if err := decodeJSON(contents, &receipt); err != nil {
		return runtimeReceipt{}, err
	}
	if receipt.Version != Version || receipt.GoalID != id {
		return runtimeReceipt{}, errors.New("runtime receipt identity changed")
	}
	actualCodex, err := inspectBinary(codex)
	if err != nil {
		return runtimeReceipt{}, err
	}
	actualSelf, err := inspectBinary(self)
	if err != nil {
		return runtimeReceipt{}, err
	}
	if actualCodex != receipt.Codex || actualSelf != receipt.Forgectl {
		return runtimeReceipt{}, errors.New("approved executable path or bytes changed")
	}
	return receipt, nil
}
