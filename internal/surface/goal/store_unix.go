//go:build unix

package goal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/privdir"
)

type Store struct{ Root string }

func DefaultRoot() (string, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "forgectl", "projects"), nil
}

func (store Store) root(create bool) (int, error) {
	if !filepath.IsAbs(store.Root) || filepath.Clean(store.Root) != store.Root || store.Root == "/" {
		return -1, errors.New("goal root must be an absolute clean private directory")
	}
	spec := privdir.Spec{Base: filepath.Dir(store.Root), Leaf: filepath.Base(store.Root), Mode: 0o700}
	rootFD, err := privdir.OpenChecked(spec)
	if errors.Is(err, privdir.ErrAbsent) && create {
		spec.Create = true
		spec.AncestorMode = 0o700
		rootFD, err = privdir.Pin(spec)
	}
	return rootFD, err
}

func childDirectory(parent int, name string, create bool) (int, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return -1, errors.New("invalid private directory name")
	}
	if create {
		if err := unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
	}
	directory, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(directory, &info); err != nil {
		_ = unix.Close(directory)
		return -1, err
	}
	if int(info.Uid) != os.Geteuid() || uint32(info.Mode)&0o7777 != 0o700 {
		_ = unix.Close(directory)
		return -1, errors.New("goal directory must be owned by this user with mode 0700")
	}
	return directory, nil
}

func (store Store) directory(id string, create bool) (int, error) {
	if !idPattern.MatchString(id) {
		return -1, errors.New("invalid goal ID")
	}
	root, err := store.root(create)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(root) }()
	goals, err := childDirectory(root, "goals", create)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(goals) }()
	return childDirectory(goals, id, create)
}

func (store Store) RunDir(id string) string { return filepath.Join(store.Root, "goals", id) }
func (store Store) Workspace(id string) string {
	return filepath.Join(filepath.Dir(store.Root), filepath.Base(store.Root)+"-worktrees", id)
}

func readPrivate(directory int, name string, maximum int64) ([]byte, error) {
	file, err := privdir.OpenFileChecked(directory, name, unix.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("goal record exceeds its size bound")
	}
	return contents, nil
}

func writePrivate(directory int, name string, contents []byte, replace bool) error {
	identifier, err := newID()
	if err != nil {
		return err
	}
	temporary := "." + identifier + ".tmp"
	file, err := privdir.OpenFileChecked(directory, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(directory, temporary, 0) }()
	_, writeErr := file.Write(contents)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !replace {
		if err := unix.Linkat(directory, temporary, directory, name, 0); err != nil {
			return err
		}
		if err := unix.Unlinkat(directory, temporary, 0); err != nil {
			return err
		}
	} else {
		if err := unix.Renameat(directory, temporary, directory, name); err != nil {
			return err
		}
	}
	return unix.Fsync(directory)
}

func privateJSON(directory int, name string, value any, replace bool) error {
	// termsafe:allow-raw-json private state record, never terminal output
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(directory, name, append(contents, '\n'), replace)
}

func decodeJSON(contents []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("goal record must contain exactly one JSON document")
	}
	return nil
}

func lockFile(ctx context.Context, directory int, name string, wait bool) (*os.File, error) {
	file, err := privdir.OpenFileChecked(directory, name, unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !wait || (!errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN)) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			_ = file.Close()
			return nil, errors.New("goal record lock timed out")
		case <-ticker.C:
		}
	}
}

func releaseLock(file *os.File) { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }

func (store Store) Create(ctx context.Context, prepared Prepared) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := prepared.Spec.Validate(); err != nil {
		return State{}, err
	}
	if prepared.Spec.BaseRevision == "" {
		return State{}, errors.New("goal has not been prepared against an exact source HEAD")
	}
	id, err := newID()
	if err != nil {
		return State{}, err
	}
	directory, err := store.directory(id, true)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = unix.Close(directory) }()
	// termsafe:allow-raw-json private approved goal record, never terminal output
	goalBytes, err := json.Marshal(prepared.Spec)
	if err != nil {
		return State{}, err
	}
	if err := writePrivate(directory, "goal.json", goalBytes, false); err != nil {
		return State{}, err
	}
	now := time.Now().UTC()
	state := State{Version: Version, ID: id, Spec: prepared.Spec, GoalSHA256: Digest(goalBytes), Phase: PhasePreparing, Summary: "Preparing a new isolated worktree", Root: store.Root, RunDir: store.RunDir(id), Worktree: store.Workspace(id), Branch: "goal/" + strings.TrimPrefix(id, "goal-"), CreatedAt: now, UpdatedAt: now, HeartbeatAt: now, Deadline: now.Add(time.Duration(prepared.Spec.LimitMinutes) * time.Minute), Roles: []Role{}, Questions: []Question{}, Checks: []CheckResult{}}
	if err := privateJSON(directory, "state.json", state, false); err != nil {
		return State{}, err
	}
	return state, nil
}

func (store Store) Save(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.validateState(state); err != nil {
		return err
	}
	directory, err := store.directory(state.ID, false)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(directory) }()
	return privateJSON(directory, "state.json", state, true)
}

func (store Store) validateState(state State) error {
	if state.Version != Version || !idPattern.MatchString(state.ID) || state.Root != store.Root || state.RunDir != store.RunDir(state.ID) || state.Worktree != store.Workspace(state.ID) || state.Branch != "goal/"+strings.TrimPrefix(state.ID, "goal-") {
		return errors.New("goal state identity or paths do not match this store")
	}
	if !revisionPattern.MatchString(state.Spec.BaseRevision) {
		return errors.New("goal state has no exact source revision")
	}
	return state.Spec.Validate()
}

func (store Store) load(id string) (State, error) {
	directory, err := store.directory(id, false)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = unix.Close(directory) }()
	contents, err := readPrivate(directory, "state.json", 1024*1024)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := decodeJSON(contents, &state); err != nil {
		return State{}, err
	}
	if state.ID != id {
		return State{}, errors.New("goal state ID changed")
	}
	if err := store.validateState(state); err != nil {
		return State{}, err
	}
	goalBytes, err := readPrivate(directory, "goal.json", 32*1024)
	if err != nil {
		return State{}, err
	}
	// termsafe:allow-raw-json private approved goal comparison, never terminal output
	expected, err := json.Marshal(state.Spec)
	if err != nil {
		return State{}, err
	}
	if Digest(goalBytes) != state.GoalSHA256 || !bytes.Equal(goalBytes, expected) {
		return State{}, errors.New("approved goal record changed")
	}
	return state, nil
}

func (store Store) Load(ctx context.Context, id string) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	state, err := store.load(id)
	if err != nil {
		return State{}, err
	}
	state.SupervisorActive = false
	directory, err := store.directory(id, false)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = unix.Close(directory) }()
	file, lockErr := privdir.OpenFileChecked(directory, "supervisor.lock", unix.O_RDONLY, 0o600)
	if lockErr == nil {
		defer func() { _ = file.Close() }()
		lockErr = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		} else if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) {
			state.SupervisorActive = true
		} else {
			return State{}, lockErr
		}
	} else if !errors.Is(lockErr, fs.ErrNotExist) {
		return State{}, lockErr
	}
	if !state.Phase.Terminal() && time.Since(state.HeartbeatAt) > 15*time.Second {
		state.Phase = PhaseUnknown
		if state.SupervisorActive {
			state.Summary = "Supervisor lease is held but heartbeat is stale; no restart or replay attempted"
		} else {
			state.Summary = "Supervisor lease is absent; native cleanup is not yet established. Inspect retained receipts; no replay attempted"
		}
	}
	return state, nil
}

func (store Store) List(ctx context.Context) ([]State, error) {
	states := []State{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := store.root(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return states, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(root) }()
	directory, err := childDirectory(root, "goals", false)
	if errors.Is(err, fs.ErrNotExist) {
		return states, nil
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(directory), "goals")
	defer func() { _ = file.Close() }()
	names, err := file.Readdirnames(501)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > 500 {
		return nil, errors.New("goal list exceeds 500 retained runs; inspect and explicitly archive old runs")
	}
	for _, name := range names {
		if !idPattern.MatchString(name) {
			return nil, errors.New("unexpected record in private goal store")
		}
		state, err := store.Load(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("read goal %s: %w", name, err)
		}
		states = append(states, state)
	}
	slices.SortFunc(states, func(first, second State) int { return second.CreatedAt.Compare(first.CreatedAt) })
	return states, nil
}

func (store Store) Request(ctx context.Context, id, action, questionID, answer string) (Control, error) {
	if action != "cancel" && action != "respond" {
		return Control{}, errors.New("unknown goal control action")
	}
	if action == "respond" && !safeText(answer, 8192, true) {
		return Control{}, errors.New("answer must be nonempty text of at most 8 KiB")
	}
	directory, err := store.directory(id, false)
	if err != nil {
		return Control{}, err
	}
	defer func() { _ = unix.Close(directory) }()
	lock, err := lockFile(ctx, directory, "control.lock", true)
	if err != nil {
		return Control{}, err
	}
	defer releaseLock(lock)
	state, err := store.Load(ctx, id)
	if err != nil {
		return Control{}, err
	}
	if state.Phase.Terminal() || state.Phase == PhaseUnknown || !state.SupervisorActive {
		return Control{}, errors.New("goal has no active responsive supervisor; inspect retained evidence, do not replay")
	}
	if action == "respond" && (state.Phase != PhaseNeedsOperator || state.Question == nil || state.Question.ID != questionID || state.Question.Answer != "") {
		return Control{}, errors.New("question is no longer the current unanswered decision")
	}
	control := Control{Version: Version, GoalID: id, Sequence: state.LastControl + 1, Action: action, QuestionID: questionID, Answer: answer, CreatedAt: time.Now().UTC()}
	if err := privateJSON(directory, fmt.Sprintf("control-%06d.json", control.Sequence), control, false); err != nil {
		return Control{}, fmt.Errorf("another control is pending: %w", err)
	}
	return control, nil
}

func (store Store) nextControl(state State) (*Control, error) {
	directory, err := store.directory(state.ID, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()
	contents, err := readPrivate(directory, fmt.Sprintf("control-%06d.json", state.LastControl+1), 16*1024)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var control Control
	if err := decodeJSON(contents, &control); err != nil {
		return nil, err
	}
	if control.Version != Version || control.GoalID != state.ID || control.Sequence != state.LastControl+1 || (control.Action != "cancel" && control.Action != "respond") {
		return nil, errors.New("invalid or stale operator control record")
	}
	return &control, nil
}

func (store Store) AppendEvent(id string, event Event) error {
	directory, err := store.directory(id, false)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(directory) }()
	file, err := privdir.OpenFileChecked(directory, "events.jsonl", unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	// termsafe:allow-raw-json private evidence journal row, never terminal output
	contents, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(contents) > 1024*1024 || info.Size()+int64(len(contents)) > 32*1024*1024 {
		return errors.New("native evidence limit reached; stopping rather than dropping evidence")
	}
	_, err = file.Write(append(contents, '\n'))
	return err
}

func (store Store) WriteArtifact(id, name string, contents []byte) error {
	if len(contents) > 4*1024*1024 {
		return errors.New("goal artifact exceeds 4 MiB")
	}
	if filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return errors.New("invalid artifact name")
	}
	directory, err := store.directory(id, false)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(directory) }()
	return writePrivate(directory, name, contents, false)
}

func (store Store) ReadArtifact(ctx context.Context, id, kind string, index int) (string, error) {
	state, err := store.Load(ctx, id)
	if err != nil {
		return "", err
	}
	if kind == "json" {
		// termsafe:allow-raw-json state artifact is passed through the CLI's safe renderer
		contents, err := json.MarshalIndent(state, "", "  ")
		return string(contents), err
	}
	var name, expected string
	switch kind {
	case "patch":
		if state.Result == nil {
			return "", errors.New("no retained result patch yet")
		}
		name, expected = "result.patch", state.Result.PatchSHA256
	case "check":
		if index < 0 || index >= len(state.Checks) {
			return "", errors.New("verification receipt is not available yet")
		}
		name, expected = fmt.Sprintf("check-%02d.txt", index+1), state.Checks[index].OutputSHA256
	default:
		return "", errors.New("unknown goal artifact")
	}
	directory, err := store.directory(id, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(directory) }()
	contents, err := readPrivate(directory, name, 4*1024*1024)
	if err != nil {
		return "", err
	}
	if Digest(contents) != expected {
		return "", errors.New("retained artifact digest changed")
	}
	return string(contents), nil
}
