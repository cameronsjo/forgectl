//go:build unix

package goalcontainer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
)

type Host struct {
	config       Config
	api          *client
	control      int
	gate         chan struct{}
	mutex        sync.Mutex
	closed       bool
	activeCancel context.CancelFunc
	unavailable  error
	cleanupError error
}

type operation struct {
	host     *Host
	request  createRequest
	receipt  Receipt
	sequence int
}

func New(ctx context.Context, config Config) (*Host, error) {
	if os.Geteuid() == 0 || os.Getegid() == 0 {
		return nil, errors.New("container execution requires non-root caller UID/GID")
	}
	if _, err := Inspect(ctx, config.Isolation); err != nil {
		return nil, err
	}
	control, err := privdir.Pin(privdir.Spec{Base: filepath.Dir(config.ControlDir), Leaf: filepath.Base(config.ControlDir), Mode: 0o700, AncestorMode: 0o700, Create: true})
	if err != nil {
		return nil, err
	}
	if err := config.validate(); err != nil {
		_ = unix.Close(control)
		return nil, err
	}
	host := &Host{config: config, api: newClient(config.Isolation.Socket), control: control, gate: make(chan struct{}, 1)}
	host.gate <- struct{}{}
	result, err := host.Exec(ctx, []string{HelperPath, "surface", "project", "goal", "_container-init", "--protocol"}, config.Directory, []string{config.Directory}, 15*time.Second)
	if err == nil && result.ExitCode == 0 {
		err = verifyProbe(result.Stdout)
	} else if err == nil {
		err = errors.New("container image did not execute its required Linux goal helper")
	}
	if err != nil {
		closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return nil, errors.Join(fmt.Errorf("container helper prerequisite: %w", err), host.Close(closeContext))
	}
	return host, nil
}

func (host *Host) File(ctx context.Context, root, path string, write bool, content, cwd string, roots []string) (goalnative.CommandResult, error) {
	argv := []string{HelperPath, "surface", "project", "goal", "_file", "--root", root, "--path", path}
	if write {
		argv = append(argv, "--write", "--content", content)
	}
	return host.Exec(ctx, argv, cwd, roots, 15*time.Second)
}

func (host *Host) Exec(ctx context.Context, argv []string, cwd string, roots []string, timeout time.Duration) (result goalnative.CommandResult, outcome error) {
	if timeout <= 0 || timeout > 120*time.Second {
		return result, errors.New("container operation timeout must be between zero and 120 seconds")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-host.gate:
	}
	defer func() { host.gate <- struct{}{} }()
	host.mutex.Lock()
	if host.closed || host.unavailable != nil {
		err := errors.Join(errors.New("container execution host is closed or unavailable"), host.unavailable)
		host.mutex.Unlock()
		return result, err
	}
	host.activeCancel = cancel
	host.mutex.Unlock()
	defer func() {
		host.mutex.Lock()
		host.activeCancel = nil
		host.mutex.Unlock()
	}()
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return result, err
	}
	nonce := hex.EncodeToString(entropy[:])
	deadline, _ := ctx.Deadline()
	request, err := host.request(argv, cwd, roots, nonce, deadline)
	if err != nil {
		return result, err
	}
	configuration, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	command, err := json.Marshal(argv)
	if err != nil {
		return result, err
	}
	current := &operation{host: host, request: request, receipt: Receipt{Version: 1, Name: "fg-goal-" + nonce, Role: host.config.Role, Nonce: nonce, ImageID: request.Image, ConfigSHA256: digest(configuration), CommandSHA256: digest(command), CreatedAt: time.Now().UTC(), Policy: request.HostConfig}}
	if err := current.record("creating", nil); err != nil {
		return result, err
	}
	defer func() {
		cleanupErr := current.cleanup()
		outcome = errors.Join(outcome, cleanupErr)
		host.mutex.Lock()
		if cleanupErr != nil {
			host.cleanupError = errors.Join(host.cleanupError, cleanupErr)
		}
		if cleanupErr != nil || (outcome != nil && !errors.Is(outcome, context.Canceled) && !errors.Is(outcome, context.DeadlineExceeded)) {
			host.unavailable = outcome
		}
		host.mutex.Unlock()
	}()
	var created struct {
		ID string `json:"Id"`
	}
	if err := host.api.request(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(current.receipt.Name), request, &created); err != nil {
		return result, fmt.Errorf("container create failed or acceptance is uncertain; execution will not be replayed: %w", err)
	}
	if !identityPattern.MatchString(created.ID) {
		return result, errors.New("container create returned an invalid identity")
	}
	current.receipt.ContainerID = created.ID
	observed, err := host.api.inspect(ctx, created.ID)
	if err != nil {
		return result, err
	}
	if err := verifyIdentity(observed, request, current.receipt.Name, created.ID); err != nil {
		return result, err
	}
	if err := verifyPolicy(observed, request); err != nil {
		return result, err
	}
	if observed.State.Running || observed.State.PID != 0 || observed.State.Status != "created" {
		return result, errors.New("new container has already executed; refusing to replay it")
	}
	if err := current.record("created", nil); err != nil {
		return result, err
	}
	connection, reader, err := host.api.attach(ctx, created.ID)
	if err != nil {
		return result, err
	}
	defer func() { _ = connection.Close() }()
	if err := current.record("starting", nil); err != nil {
		return result, err
	}
	if err := host.api.request(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return result, fmt.Errorf("container start was not confirmed; command will not be replayed: %w", err)
	}
	if err := current.record("running", nil); err != nil {
		return result, err
	}
	leaseContext, stopLease := context.WithCancel(ctx)
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		keepAlive(leaseContext, connection)
	}()
	defer func() { stopLease(); <-leaseDone }()
	output := make(chan struct {
		stdout, stderr string
		err            error
	}, 1)
	go func() {
		stdout, stderr, err := readOutput(reader)
		output <- struct {
			stdout, stderr string
			err            error
		}{stdout, stderr, err}
	}()
	select {
	case <-ctx.Done():
		_ = connection.Close()
		<-output
		return result, ctx.Err()
	case received := <-output:
		result.Stdout, result.Stderr = received.stdout, received.stderr
		if received.err != nil {
			return result, received.err
		}
	}
	observed, err = host.api.inspect(ctx, created.ID)
	if err != nil {
		return result, err
	}
	if err := verifyIdentity(observed, request, current.receipt.Name, created.ID); err != nil {
		return result, err
	}
	if err := verifyCommandExit(observed.State); err != nil {
		return result, err
	}
	result.ExitCode = observed.State.ExitCode
	return result, nil
}

func verifyCommandExit(state containerState) error {
	if state.Status != "exited" || state.Running || state.PID != 0 || state.Paused || state.Restarting || state.Dead || state.OOMKilled || state.Error != "" {
		return errors.New("container stream ended without a clean command exit")
	}
	return nil
}

func keepAlive(ctx context.Context, connection net.Conn) {
	ticker := time.NewTicker(leaseInterval)
	defer ticker.Stop()
	for {
		if err := connection.SetWriteDeadline(time.Now().Add(leaseInterval)); err != nil {
			_ = connection.Close()
			return
		}
		if _, err := connection.Write([]byte("alive\n")); err != nil {
			_ = connection.Close()
			return
		}
		select {
		case <-ctx.Done():
			_ = connection.Close()
			return
		case <-ticker.C:
		}
	}
}

func (current *operation) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	identity := current.receipt.ContainerID
	if identity == "" {
		identity = current.receipt.Name
	}
	observed, err := current.host.api.inspect(ctx, identity)
	if err != nil {
		return current.unknown(err)
	}
	if err := verifyIdentity(observed, current.request, current.receipt.Name, current.receipt.ContainerID); err != nil {
		return current.unknown(err)
	}
	current.receipt.ContainerID = observed.ID
	var evidenceError error
	if observed.State.Running || observed.State.PID != 0 {
		evidenceError = current.record("stopping", nil)
		killErr := current.host.api.request(ctx, http.MethodPost, "/containers/"+observed.ID+"/kill?signal=KILL", nil, nil)
		for {
			observed, err = current.host.api.inspect(ctx, current.receipt.ContainerID)
			if err != nil {
				return current.unknown(errors.Join(evidenceError, killErr, err))
			}
			if err := verifyIdentity(observed, current.request, current.receipt.Name, current.receipt.ContainerID); err != nil {
				return current.unknown(errors.Join(evidenceError, err))
			}
			if !observed.State.Running && observed.State.PID == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return current.unknown(errors.Join(evidenceError, killErr, ctx.Err()))
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	if observed.State.Paused || observed.State.Restarting || observed.State.Dead {
		return current.unknown(errors.Join(evidenceError, errors.New("container runtime did not confirm a stopped namespace")))
	}
	current.receipt.Stopped, current.receipt.State = true, observed.State
	if err := current.record("stopped", nil); err != nil || evidenceError != nil {
		return current.unknown(errors.Join(evidenceError, err))
	}
	if err := current.host.api.request(ctx, http.MethodDelete, "/containers/"+observed.ID, nil, nil); err != nil {
		return current.unknown(err)
	}
	_, err = current.host.api.inspect(ctx, observed.ID)
	var response apiError
	if !errors.As(err, &response) || response.status != http.StatusNotFound {
		return current.unknown(errors.New("owned container removal was not confirmed"))
	}
	current.receipt.Removed = true
	return current.record("removed", nil)
}

func (current *operation) unknown(err error) error {
	err = errors.Join(goalnative.ErrCleanupUnconfirmed, err)
	return errors.Join(err, current.record("unknown", err))
}

func (current *operation) record(phase string, cause error) error {
	current.sequence++
	current.receipt.Phase, current.receipt.UpdatedAt = phase, time.Now().UTC()
	if cause != nil {
		current.receipt.Error = cause.Error()
	}
	data, err := json.Marshal(current.receipt)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%02d-%s.json", current.receipt.Nonce, current.sequence, phase)
	file, err := privdir.OpenFileChecked(current.host.control, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr, unix.Fsync(current.host.control))
}

func (host *Host) Close(ctx context.Context) error {
	host.mutex.Lock()
	host.closed = true
	if host.activeCancel != nil {
		host.activeCancel()
	}
	host.mutex.Unlock()
	select {
	case <-ctx.Done():
		return errors.Join(goalnative.ErrCleanupUnconfirmed, ctx.Err())
	case <-host.gate:
	}
	defer func() { host.gate <- struct{}{} }()
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if host.control >= 0 {
		host.cleanupError = errors.Join(host.cleanupError, unix.Close(host.control))
		host.control = -1
	}
	host.api.transport.CloseIdleConnections()
	return host.cleanupError
}

func digest(value []byte) string {
	result := sha256.Sum256(value)
	return hex.EncodeToString(result[:])
}
