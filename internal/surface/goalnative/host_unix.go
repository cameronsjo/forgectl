//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

const nativeConfigMaxBytes = 256 * 1024

type nativeConfigName struct {
	Section string
	Name    string
}

type guardianReceipt struct {
	Status       string    `json:"status"`
	NativePID    int       `json:"native_pid,omitempty"`
	ProcessGroup int       `json:"process_group,omitempty"`
	Socket       string    `json:"socket,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	SocketGone   bool      `json:"socket_gone"`
	GroupGone    bool      `json:"group_gone"`
	FinishedAt   time.Time `json:"finished_at"`
	Error        string    `json:"error,omitempty"`
}

func startSupervisor(binary, spec string, lease *os.File, cwd string, environment []string) (*os.Process, error) {
	command := exec.Command(binary, "surface", "project", "goal", "_host", "--spec", spec) //nolint:gosec,noctx // supervisor binary and private spec path are validated before launch
	command.Dir = cwd
	command.Stdin = lease
	command.Stdout = io.Discard
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if environment != nil {
		command.Env = append([]string(nil), environment...)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start goal supervisor: %w", err)
	}
	return command.Process, nil
}

func RunHost(ctx context.Context, specPath string, lease io.Reader, stderr io.Writer) error {
	spec, err := readHostSpec(specPath)
	if err != nil {
		return err
	}
	if lease == nil {
		return errors.New("goal supervisor lease is required")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if err := validateHostSpec(spec); err != nil {
		return err
	}
	environment := os.Environ()
	arguments := spec.Arguments
	if spec.Protocol == "" {
		arguments, err = nativeServerArguments(spec, environment)
		if err != nil {
			return err
		}
	}
	command := exec.Command(spec.Binary, arguments...) //nolint:gosec,noctx // native binary and socket are validated host-owned paths
	command.Dir = spec.Cwd
	command.Env = environment
	command.Stdout = stderr
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if spec.Protocol == "json-lines" {
		bridge, err := bridgeProcess(spec.Socket, command)
		if err != nil {
			return err
		}
		defer func() { _ = bridge.Close() }()
	}
	if err := command.Start(); err != nil {
		receiptErr := writeGuardianReceipt(spec.Receipt, guardianReceipt{Status: "failed", Socket: spec.Socket, FinishedAt: time.Now().UTC(), Error: err.Error()})
		return errors.Join(fmt.Errorf("start native app-server: %w", err), receiptErr)
	}
	nativePID := command.Process.Pid
	groupID := nativePID
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	startedAt := time.Now().UTC()
	if err := writeGuardianReceipt(startupReceiptPath(spec.ControlDir), guardianReceipt{Status: "started", NativePID: nativePID, ProcessGroup: groupID, Socket: spec.Socket, StartedAt: startedAt}); err != nil {
		stopErr := stopNative(groupID, wait, err)
		_ = removeSocket(spec.Socket)
		return errors.Join(errors.New("persist native guardian startup receipt"), stopErr)
	}
	leaseClosed := make(chan error, 1)
	go func() { _, readErr := io.Copy(io.Discard, lease); leaseClosed <- readErr }()
	var stopReason error
	processExited := false
	leaseStopped := false
	select {
	case err := <-wait:
		stopReason = err
		processExited = true
	case leaseErr := <-leaseClosed:
		leaseStopped = true
		stopReason = errors.New("goal lease closed")
		if leaseErr != nil {
			stopReason = errors.Join(stopReason, leaseErr)
		}
		stopReason = stopNative(groupID, wait, stopReason)
	case <-ctx.Done():
		stopReason = ctx.Err()
		stopReason = stopNative(groupID, wait, stopReason)
	}
	if cleanupErr := stopDescendants(groupID); cleanupErr != nil {
		stopReason = errors.Join(stopReason, cleanupErr)
	}
	groupGone := processGroupGone(groupID)
	socketGone := removeSocket(spec.Socket)
	receipt := guardianReceipt{Status: "stopped", NativePID: nativePID, ProcessGroup: groupID, Socket: spec.Socket, StartedAt: startedAt, SocketGone: socketGone, GroupGone: groupGone, FinishedAt: time.Now().UTC()}
	if processExited && stopReason != nil && !errors.Is(stopReason, os.ErrProcessDone) {
		receipt.Error = stopReason.Error()
	}
	if !processExited && !leaseStopped && stopReason != nil && !errors.Is(stopReason, context.Canceled) && !errors.Is(stopReason, syscall.EPIPE) {
		receipt.Error = stopReason.Error()
	}
	if !groupGone {
		receipt.Status = "unknown"
		if receipt.Error == "" {
			receipt.Error = "native process group remains"
		}
	}
	if err := writeGuardianReceipt(spec.Receipt, receipt); err != nil {
		return errors.Join(stopReason, err)
	}
	if !groupGone {
		return errors.New("native process group cleanup not confirmed")
	}
	if processExited && stopReason != nil && !errors.Is(stopReason, os.ErrProcessDone) {
		return fmt.Errorf("native app-server exited: %w", stopReason)
	}
	return nil
}

func nativeServerArguments(spec hostSpec, environment []string) ([]string, error) {
	names, err := nativeConfigNames(environment)
	if err != nil {
		return nil, err
	}
	arguments := []string{
		"app-server",
		"--disable", "apps",
		"--disable", "hooks",
		"--disable", "memories",
		"--disable", "multi_agent",
		"--disable", "plugins",
		"--config", fmt.Sprintf("projects={%q={trust_level=\"untrusted\"}}", spec.Cwd),
		"--config", "agents.enabled=false",
		"--config", `web_search="disabled"`,
		"--config", "notify=[]",
		"--config", "apps._default.enabled=false",
	}
	if spec.BrokerTools {
		for _, feature := range brokerDisabledFeatures {
			arguments = append(arguments, "--disable", feature)
		}
		arguments = append(arguments, "--config", "project_doc_max_bytes=0")
	}
	for _, section := range []string{"apps", "mcp_servers", "plugins"} {
		var entries []nativeConfigName
		for _, entry := range names {
			if entry.Section == section {
				entries = append(entries, entry)
			}
		}
		if len(entries) == 0 {
			continue
		}
		if allNativeConfigPathSafe(entries) {
			for _, entry := range entries {
				arguments = append(arguments, "--config", entry.Section+"."+entry.Name+".enabled=false")
			}
			continue
		}
		var values []string
		for _, entry := range entries {
			values = append(values, strconv.Quote(entry.Name)+"={enabled=false}")
		}
		arguments = append(arguments, "--config", section+"={"+strings.Join(values, ",")+"}")
	}
	arguments = append(arguments, "--listen", "unix://"+spec.Socket)
	return arguments, nil
}

func nativeConfigNames(environment []string) ([]nativeConfigName, error) {
	path, err := nativeConfigPath(environment)
	if err != nil {
		return nil, err
	}
	data, err := readNativeConfig(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var document map[string]any
	if _, err := toml.Decode(string(data), &document); err != nil {
		return nil, errors.New("native config is malformed")
	}
	var names []nativeConfigName
	for _, section := range []string{"apps", "mcp_servers", "plugins"} {
		value, present := document[section]
		if !present {
			continue
		}
		table, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("native config capability section is malformed")
		}
		for name, entry := range table {
			if !safeConfigName(name) {
				return nil, errors.New("native config capability name is unsafe")
			}
			if _, ok := entry.(map[string]any); !ok {
				return nil, errors.New("native config capability entry is malformed")
			}
			names = append(names, nativeConfigName{Section: section, Name: name})
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].Section != names[j].Section {
			return names[i].Section < names[j].Section
		}
		return names[i].Name < names[j].Name
	})
	return names, nil
}

func nativeConfigPath(environment []string) (string, error) {
	configHome := ""
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "CODEX_HOME" {
			configHome = value
			break
		}
	}
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("resolve native config home")
		}
		configHome = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(configHome) || filepath.Clean(configHome) != configHome {
		return "", errors.New("native config home must be absolute and clean")
	}
	return filepath.Join(configHome, "config.toml"), nil
}

func readNativeConfig(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil
		}
		return nil, errors.New("read native config")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open native config")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > nativeConfigMaxBytes {
		return nil, errors.New("native config must be a bounded private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, nativeConfigMaxBytes+1))
	if err != nil {
		return nil, errors.New("read native config")
	}
	if len(data) > nativeConfigMaxBytes {
		return nil, errors.New("native config exceeds size limit")
	}
	return data, nil
}

func safeConfigName(value string) bool {
	return len(value) > 0 && len(value) <= 256 && safeToken(value)
}

func allNativeConfigPathSafe(entries []nativeConfigName) bool {
	for _, entry := range entries {
		for _, character := range entry.Name {
			if !safeConfigNameCharacter(character) {
				return false
			}
		}
	}
	return true
}

func safeConfigNameCharacter(character rune) bool {
	return character == '_' || character == '-' || character >= '0' && character <= '9' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
}

func stopNative(groupID int, wait <-chan error, reason error) error {
	if err := syscall.Kill(-groupID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return errors.Join(reason, err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-wait:
		return errors.Join(reason, err)
	case <-timer.C:
	}
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return errors.Join(reason, err)
	}
	select {
	case err := <-wait:
		return errors.Join(reason, err)
	case <-time.After(2 * time.Second):
		return errors.Join(reason, errors.New("native process did not exit"))
	}
}

func stopDescendants(groupID int) error {
	if processGroupGone(groupID) {
		return nil
	}
	if err := syscall.Kill(-groupID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for !processGroupGone(groupID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processGroupGone(groupID) {
		return nil
	}
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline = time.Now().Add(2 * time.Second)
	for !processGroupGone(groupID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processGroupGone(groupID) {
		return errors.New("native descendants did not exit")
	}
	return nil
}

func processGroupGone(groupID int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(-groupID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func removeSocket(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false
	}
	return os.Remove(path) == nil
}

func readHostSpec(path string) (hostSpec, error) {
	if err := mustPrivatePath(path); err != nil {
		return hostSpec{}, err
	}
	info, err := os.Stat(path) //nolint:gosec // mustPrivatePath rejects symlinks before this owner-private spec read
	if err != nil {
		return hostSpec{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024 || info.Mode().Perm()&0077 != 0 {
		return hostSpec{}, errors.New("host spec must be a bounded private regular file")
	}
	data, err := os.ReadFile(path) //nolint:gosec // readGuardianReceipt verifies the private regular file before reading it
	if err != nil {
		return hostSpec{}, err
	}
	var spec hostSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return hostSpec{}, fmt.Errorf("decode goal host spec: %w", err)
	}
	return spec, nil
}

func validateHostSpec(spec hostSpec) error {
	for name, value := range map[string]string{"binary": spec.Binary, "control directory": spec.ControlDir, "cwd": spec.Cwd, "socket": spec.Socket, "receipt": spec.Receipt} {
		if value == "" {
			return fmt.Errorf("host spec %s is required", name)
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("host spec %s must be absolute and clean", name)
		}
	}
	if spec.Protocol != "" && spec.Protocol != "json-lines" {
		return errors.New("host protocol is unsupported")
	}
	if spec.Protocol == "json-lines" {
		if spec.ExecutionOnly || spec.Model != "" || len(spec.Arguments) > 128 {
			return errors.New("JSON process must not select an app-server mode")
		}
		for _, argument := range spec.Arguments {
			if len(argument) > 32*1024 || strings.ContainsRune(argument, 0) {
				return errors.New("JSON process argument is invalid")
			}
		}
	} else if (!spec.ExecutionOnly && (spec.Model == "" || !safeToken(spec.Model))) || (spec.ExecutionOnly && spec.Model != "") {
		return errors.New("host spec model is invalid")
	}
	if err := ensurePrivateDir(spec.ControlDir); err != nil {
		return err
	}
	if err := ensurePrivateDir(filepath.Dir(spec.Socket)); err != nil {
		return err
	}
	if err := ensurePrivateDir(filepath.Dir(spec.Receipt)); err != nil {
		return err
	}
	if filepath.Clean(spec.Receipt) != filepath.Join(spec.ControlDir, "guardian-receipt.json") {
		return errors.New("host receipt must stay in the control directory")
	}
	if len(spec.WritableRoots) == 0 {
		return errors.New("host spec has no writable roots")
	}
	for _, root := range spec.WritableRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return errors.New("host spec writable root is not absolute and clean")
		}
		if pathWithin(root, spec.ControlDir) || pathWithin(spec.ControlDir, root) || pathWithin(spec.Socket, root) || pathWithin(spec.Receipt, root) {
			return errors.New("host control paths overlap writable roots")
		}
	}
	return nil
}

func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("directory is not owner-private: %s", path)
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	// termsafe:allow-raw-json private native host record, never terminal output
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writePrivateBytes(path, append(data, '\n'))
}
func writePrivateBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + fmt.Sprintf(".%d.tmp", time.Now().UnixNano())
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // temporary is derived from an owner-private destination
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}

func writeGuardianReceipt(path string, receipt guardianReceipt) error {
	return writePrivateJSON(path, receipt)
}

func startupReceiptPath(controlDir string) string {
	return filepath.Join(controlDir, "guardian-startup.json")
}

func readGuardianReceipt(path string) (guardianReceipt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return guardianReceipt{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return guardianReceipt{}, errors.New("guardian receipt must be an owner-private regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return guardianReceipt{}, errors.New("guardian receipt must not be hard-linked")
	}
	data, err := os.ReadFile(path) //nolint:gosec // readHostSpec verifies the private regular file before reading it
	if err != nil {
		return guardianReceipt{}, err
	}
	var receipt guardianReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return guardianReceipt{}, fmt.Errorf("decode guardian receipt: %w", err)
	}
	return receipt, nil
}

func validateGuardianReceipts(controlDir, socket string) error {
	started, err := readGuardianReceipt(startupReceiptPath(controlDir))
	if err != nil {
		return fmt.Errorf("read guardian startup receipt: %w", err)
	}
	finished, err := readGuardianReceipt(filepath.Join(controlDir, "guardian-receipt.json"))
	if err != nil {
		return fmt.Errorf("read guardian receipt: %w", err)
	}
	if started.Status != "started" || started.NativePID <= 1 || started.ProcessGroup <= 1 || started.Socket != socket || started.StartedAt.IsZero() || started.Error != "" {
		return errors.New("guardian startup receipt does not identify the owned native process")
	}
	if finished.Status != "stopped" || finished.NativePID != started.NativePID || finished.ProcessGroup != started.ProcessGroup || finished.Socket != started.Socket || !finished.StartedAt.Equal(started.StartedAt) || !finished.SocketGone || !finished.GroupGone || finished.Error != "" {
		return errors.New("guardian receipt does not prove clean shutdown of the owned native process")
	}
	if !processGroupGone(finished.ProcessGroup) {
		return errors.New("guardian native process group is still present")
	}
	return nil
}

func waitForUnixSocket(ctx context.Context, socket string, child *os.Process, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("native app-server socket did not become ready")
		case <-ticker.C:
			info, err := os.Stat(socket) //nolint:gosec // socket was validated as an absolute private destination
			if err == nil && info.Mode()&os.ModeSocket != 0 {
				return nil
			}
			if child != nil {
				if err := syscall.Kill(child.Pid, 0); err != nil && errors.Is(err, syscall.ESRCH) {
					return errors.New("goal supervisor exited before native socket became ready")
				}
			}
		}
	}
}
