//go:build unix

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

const (
	registryName = "registry.json"
	registryLock = "registry.lock"
	stateMode    = 0o600
	rootMode     = 0o700
	lockPoll     = 10 * time.Millisecond
	lockMaxWait  = 5 * time.Second
	maxJSONDepth = 64
)

func snapshotStore(ctx context.Context, root string) (Registry, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, err
	}
	dir, err := openRoot(root, false)
	if errors.Is(err, privdir.ErrAbsent) {
		return emptyRegistry(), nil
	}
	if err != nil {
		return Registry{}, err
	}
	defer func() { _ = unix.Close(dir) }()
	if err := checkRegistryLock(dir); err != nil {
		return Registry{}, err
	}
	return readRegistry(dir)
}

func updateRegistry(ctx context.Context, root string, createRoot bool, mutate func(*Registry) error) (Registry, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, err
	}
	dir, err := openRoot(root, createRoot)
	if err != nil {
		if errors.Is(err, privdir.ErrAbsent) {
			return Registry{}, fmt.Errorf("%w: registry root is absent", ErrNotFound)
		}
		return Registry{}, err
	}
	defer func() { _ = unix.Close(dir) }()

	lock, err := privdir.OpenFileChecked(dir, registryLock, unix.O_RDWR|unix.O_CREAT, stateMode)
	if err != nil {
		return Registry{}, stateError("open registry lock: %s", privdir.Reason(err))
	}
	defer func() { _ = lock.Close() }()
	if err := takeLock(ctx, lock); err != nil {
		return Registry{}, err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	registry, err := readRegistry(dir)
	if err != nil {
		return Registry{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Registry{}, err
	}
	if registry.Revision == ^uint64(0) {
		return Registry{}, fmt.Errorf("%w: registry revision overflow", ErrConflict)
	}
	next := registry
	if err := mutate(&next); err != nil {
		return Registry{}, err
	}
	next.Version = Version
	next.Revision++
	if err := validateRegistry(next); err != nil {
		return Registry{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Registry{}, err
	}
	// termsafe:allow-raw-json private registry data, never terminal output
	data, err := json.Marshal(next)
	if err != nil {
		return Registry{}, stateError("encode registry: %v", err)
	}
	if len(data) > maxRegistryBytes {
		return Registry{}, fmt.Errorf("%w: registry exceeds %d bytes", ErrInvalidInput, maxRegistryBytes)
	}
	if err := writeRegistry(dir, data); err != nil {
		return Registry{}, err
	}
	return next, nil
}

func emptyRegistry() Registry {
	return Registry{Version: Version, Projects: make([]Project, 0)}
}

func openRoot(root string, create bool) (int, error) {
	parent := filepath.Dir(root)
	leaf := filepath.Base(root)
	dir, err := privdir.OpenChecked(privdir.Spec{Base: parent, Leaf: leaf, Mode: rootMode})
	if err == nil {
		return checkRootMode(dir)
	}
	if !errors.Is(err, privdir.ErrAbsent) || !create {
		return -1, rootError(err)
	}

	parentFD, err := openParent(parent)
	if err != nil {
		if errors.Is(err, privdir.ErrAbsent) {
			return -1, fmt.Errorf("%w: registry root parent is absent", ErrNotFound)
		}
		return -1, rootError(err)
	}
	if err := unix.Mkdirat(parentFD, leaf, rootMode); err != nil && !errors.Is(err, unix.EEXIST) {
		_ = unix.Close(parentFD)
		return -1, stateError("create registry root: %v", err)
	}
	_ = unix.Close(parentFD)
	dir, err = privdir.OpenChecked(privdir.Spec{Base: parent, Leaf: leaf, Mode: rootMode})
	if err != nil {
		return -1, rootError(err)
	}
	return checkRootMode(dir)
}

func openParent(parent string) (int, error) {
	fd, err := unix.Open(parent, unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_RDONLY, 0)
	if errors.Is(err, unix.ENOENT) {
		return -1, privdir.ErrAbsent
	}
	if err != nil {
		return -1, stateError("open registry root parent: %v", err)
	}
	return fd, nil
}

func checkRootMode(dir int) (int, error) {
	var statResult unix.Stat_t
	if err := unix.Fstat(dir, &statResult); err != nil {
		_ = unix.Close(dir)
		return -1, stateError("stat registry root: %v", err)
	}
	if uint32(statResult.Mode)&0o7777 != rootMode {
		_ = unix.Close(dir)
		return -1, fmt.Errorf("%w: registry root must have mode 0700", ErrInvalidState)
	}
	return dir, nil
}

func rootError(err error) error {
	if errors.Is(err, privdir.ErrUnsafe) || errors.Is(err, privdir.ErrUnsupported) {
		return stateError("registry root: %s", privdir.Reason(err))
	}
	return err
}

func stateError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidState, fmt.Sprintf(format, args...))
}

func takeLock(ctx context.Context, lock *os.File) error {
	deadline := time.NewTimer(lockMaxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(lockPoll)
	defer ticker.Stop()
	for {
		if err := checkContext(ctx); err != nil {
			return err
		}
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("%w: lock registry: %v", ErrConflict, err)
		}
		select {
		case <-ctxDone(ctx):
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%w: timed out waiting for registry lock", ErrConflict)
		case <-ticker.C:
		}
	}
}

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func readRegistry(dir int) (Registry, error) {
	file, err := privdir.OpenFileChecked(dir, registryName, unix.O_RDONLY, stateMode)
	if errors.Is(err, os.ErrNotExist) {
		return emptyRegistry(), nil
	}
	if err != nil {
		return Registry{}, stateError("open registry: %s", privdir.Reason(err))
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRegistryBytes+1))
	if err != nil {
		return Registry{}, stateError("read registry: %v", err)
	}
	if len(data) > maxRegistryBytes {
		return Registry{}, stateError("registry exceeds %d bytes", maxRegistryBytes)
	}
	registry, err := decodeRegistry(data)
	if err != nil {
		return Registry{}, err
	}
	return registry, nil
}

func checkRegistryLock(dir int) error {
	lock, err := privdir.OpenFileChecked(dir, registryLock, unix.O_RDONLY, stateMode)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return stateError("open registry lock: %s", privdir.Reason(err))
	}
	if err := lock.Close(); err != nil {
		return stateError("close registry lock: %v", err)
	}
	return nil
}

func writeRegistry(dir int, data []byte) error {
	for attempt := 0; attempt < 8; attempt++ {
		id, err := newID()
		if err != nil {
			return fmt.Errorf("%w: generate temporary registry name: %v", ErrConflict, err)
		}
		name := ".registry.tmp-" + id
		file, err := privdir.OpenFileChecked(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, stateMode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return stateError("create temporary registry: %s", privdir.Reason(err))
		}
		var temporaryStat unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &temporaryStat); err != nil {
			_ = file.Close()
			return stateError("stat temporary registry: %v", err)
		}
		cleanup := true
		defer func() {
			if cleanup {
				removeOwnedTemporary(dir, name, temporaryStat)
			}
		}()
		if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
			_ = file.Close()
			return stateError("write registry: %v", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return stateError("sync registry: %v", err)
		}
		if err := file.Close(); err != nil {
			return stateError("close registry: %v", err)
		}
		if err := unix.Renameat(dir, name, dir, registryName); err != nil {
			return stateError("replace registry: %v", err)
		}
		if err := unix.Fsync(dir); err != nil {
			return stateError("sync registry directory: %v", err)
		}
		cleanup = false
		return nil
	}
	return fmt.Errorf("%w: temporary registry name collision", ErrConflict)
}

func removeOwnedTemporary(dir int, name string, expected unix.Stat_t) {
	file, err := privdir.OpenFileChecked(dir, name, unix.O_RDONLY, stateMode)
	if err != nil {
		return
	}
	var observed unix.Stat_t
	statErr := unix.Fstat(int(file.Fd()), &observed)
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || observed.Dev != expected.Dev || observed.Ino != expected.Ino {
		return
	}
	_ = unix.Unlinkat(dir, name, 0)
}

func decodeRegistry(data []byte) (Registry, error) {
	if !utf8.Valid(data) {
		return Registry{}, stateError("registry is not valid UTF-8")
	}
	if err := scanJSON(data); err != nil {
		return Registry{}, stateError("decode registry: %v", err)
	}
	if err := checkSchemaKeys(data); err != nil {
		return Registry{}, stateError("decode registry: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var registry Registry
	if err := dec.Decode(&registry); err != nil {
		return Registry{}, stateError("decode registry: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Registry{}, stateError("decode registry: trailing data")
		}
		return Registry{}, stateError("decode registry: trailing data: %v", err)
	}
	if err := validateRegistry(registry); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

func scanJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(dec, 0); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing data")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func scanJSONValue(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch delim := token.(type) {
	case json.Delim:
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				if err := scanJSONValue(dec, depth+1); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				if err == nil {
					return errors.New("malformed object")
				}
				return err
			}
		case '[':
			for dec.More() {
				if err := scanJSONValue(dec, depth+1); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				if err == nil {
					return errors.New("malformed array")
				}
				return err
			}
		default:
			return errors.New("unexpected closing delimiter")
		}
	}
	return nil
}

func checkSchemaKeys(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("registry must be an object")
	}
	if err := checkKeys(raw, "version", "revision", "projects"); err != nil {
		return err
	}
	if err := requireFields(raw, "version", "revision", "projects"); err != nil {
		return err
	}
	var projects []json.RawMessage
	if err := json.Unmarshal(raw["projects"], &projects); err != nil {
		return fmt.Errorf("projects: %w", err)
	}
	for projectIndex, projectRaw := range projects {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(projectRaw, &object); err != nil {
			return fmt.Errorf("projects[%d]: %w", projectIndex, err)
		}
		if err := checkKeys(object, "id", "name", "worktree", "instructions", "coordinator_thread_id", "created_at", "threads"); err != nil {
			return fmt.Errorf("projects[%d]: %w", projectIndex, err)
		}
		if err := requireFields(object, "id", "name", "worktree", "instructions", "coordinator_thread_id", "created_at", "threads"); err != nil {
			return fmt.Errorf("projects[%d]: %w", projectIndex, err)
		}
		var threads []json.RawMessage
		if err := json.Unmarshal(object["threads"], &threads); err != nil {
			return fmt.Errorf("projects[%d].threads: %w", projectIndex, err)
		}
		for threadIndex, threadRaw := range threads {
			var thread map[string]json.RawMessage
			if err := json.Unmarshal(threadRaw, &thread); err != nil {
				return fmt.Errorf("projects[%d].threads[%d]: %w", projectIndex, threadIndex, err)
			}
			if err := checkKeys(thread, "id", "name", "role", "brief", "created_at"); err != nil {
				return fmt.Errorf("projects[%d].threads[%d]: %w", projectIndex, threadIndex, err)
			}
			if err := requireFields(thread, "id", "name", "role", "brief", "created_at"); err != nil {
				return fmt.Errorf("projects[%d].threads[%d]: %w", projectIndex, threadIndex, err)
			}
		}
	}
	return nil
}

func requireFields(object map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		value, present := object[key]
		if !present {
			return fmt.Errorf("missing field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("field %q is null", key)
		}
	}
	return nil
}

func checkKeys(object map[string]json.RawMessage, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range object {
		if _, ok := set[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}
