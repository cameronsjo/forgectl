// Package project stores the durable identity of collaboration projects and
// their logical threads. It deliberately does not launch a harness, create a
// worktree, or enroll a mailbox run.
package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// Version is the registry schema version written by this package.
	Version = 1
	// MaxContextBytes bounds project instructions and thread briefs.
	MaxContextBytes  = 16 * 1024
	maxRegistryBytes = 4 * 1024 * 1024
	maxNameRunes     = 128
)

var (
	ErrInvalidInput = errors.New("project: invalid input")
	ErrNotFound     = errors.New("project: not found")
	ErrConflict     = errors.New("project: conflict")
	ErrInvalidState = errors.New("project: invalid state")
)

// Role is the durable role of a logical thread.
type Role string

const (
	RoleCoordinator Role = "coordinator"
	RoleWorker      Role = "worker"
)

// Store is the private registry root. Root must be an absolute path.
type Store struct {
	Root string
}

type Registry struct {
	Version  int       `json:"version"`
	Revision uint64    `json:"revision"`
	Projects []Project `json:"projects"`
}

type Project struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Worktree            string    `json:"worktree"`
	Instructions        string    `json:"instructions"`
	CoordinatorThreadID string    `json:"coordinator_thread_id"`
	CreatedAt           time.Time `json:"created_at"`
	Threads             []Thread  `json:"threads"`
}

type Thread struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	Brief     string    `json:"brief"`
	CreatedAt time.Time `json:"created_at"`
}

// Snapshot reads the current registry without creating or modifying anything.
// An absent root or registry is an empty versioned registry.
func (store Store) Snapshot(ctx context.Context) (Registry, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, err
	}
	return snapshotStore(ctx, root)
}

// CreateProject atomically creates a project and its one coordinator thread.
// It does not create a worktree, team directory, process, branch, or model
// request.
func (store Store) CreateProject(ctx context.Context, name, worktree, instructions string) (Registry, Project, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, Project{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, Project{}, err
	}
	if err := validateName(name, "project name"); err != nil {
		return Registry{}, Project{}, err
	}
	if err := validateContext(instructions, "project instructions"); err != nil {
		return Registry{}, Project{}, err
	}
	worktree, err = validateWorktree(worktree)
	if err != nil {
		return Registry{}, Project{}, err
	}

	projectID, err := newID()
	if err != nil {
		return Registry{}, Project{}, fmt.Errorf("%w: generate project ID: %v", ErrConflict, err)
	}
	coordinatorID, err := newID()
	if err != nil {
		return Registry{}, Project{}, fmt.Errorf("%w: generate thread ID: %v", ErrConflict, err)
	}
	now := time.Now().UTC()
	created := Project{
		ID:                  projectID,
		Name:                name,
		Worktree:            worktree,
		Instructions:        instructions,
		CoordinatorThreadID: coordinatorID,
		CreatedAt:           now,
		Threads: []Thread{{
			ID:        coordinatorID,
			Name:      name,
			Role:      RoleCoordinator,
			CreatedAt: now,
		}},
	}
	registry, err := updateRegistry(ctx, root, true, func(registry *Registry) error {
		registry.Projects = append(registry.Projects, created)
		return nil
	})
	if err != nil {
		return Registry{}, Project{}, err
	}
	return registry, created, nil
}

// CreateThread atomically creates a worker thread in projectID. A thread is
// only logical state; enrollment and native execution happen in later layers.
func (store Store) CreateThread(ctx context.Context, projectID, name, brief string) (Registry, Thread, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, Thread{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, Thread{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, Thread{}, err
	}
	if err := validateName(name, "thread name"); err != nil {
		return Registry{}, Thread{}, err
	}
	if err := validateContext(brief, "thread brief"); err != nil {
		return Registry{}, Thread{}, err
	}
	threadID, err := newID()
	if err != nil {
		return Registry{}, Thread{}, fmt.Errorf("%w: generate thread ID: %v", ErrConflict, err)
	}
	created := Thread{ID: threadID, Name: name, Role: RoleWorker, Brief: brief, CreatedAt: time.Now().UTC()}
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry) error {
		for projectIndex := range registry.Projects {
			if registry.Projects[projectIndex].ID == projectID {
				registry.Projects[projectIndex].Threads = append(registry.Projects[projectIndex].Threads, created)
				return nil
			}
		}
		return fmt.Errorf("%w: project %s", ErrNotFound, projectID)
	})
	if err != nil {
		return Registry{}, Thread{}, err
	}
	return registry, created, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validateRoot(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: registry root must be absolute", ErrInvalidInput)
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return "", fmt.Errorf("%w: registry root may not be the filesystem root", ErrInvalidInput)
	}
	return root, nil
}

func validateWorktree(worktree string) (string, error) {
	if worktree == "" || !filepath.IsAbs(worktree) {
		return "", fmt.Errorf("%w: worktree must be absolute", ErrInvalidInput)
	}
	worktree = filepath.Clean(worktree)
	info, err := os.Stat(worktree)
	if err != nil {
		return "", fmt.Errorf("%w: worktree: %v", ErrInvalidInput, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: worktree is not a directory", ErrInvalidInput)
	}
	return worktree, nil
}

func validateName(value, label string) error {
	if !utf8.ValidString(value) || value == "" {
		return fmt.Errorf("%w: %s must be non-empty UTF-8", ErrInvalidInput, label)
	}
	if utf8.RuneCountInString(value) > maxNameRunes {
		return fmt.Errorf("%w: %s exceeds %d characters", ErrInvalidInput, label, maxNameRunes)
	}
	for _, runeValue := range value {
		if unicode.IsControl(runeValue) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidInput, label)
		}
	}
	return nil
}

func validateContext(value, label string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidInput, label)
	}
	if len(value) > MaxContextBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidInput, label, MaxContextBytes)
	}
	return nil
}

func validateID(value, label string) error {
	if !isID(value) {
		return fmt.Errorf("%w: %s is not a lowercase UUID", ErrInvalidInput, label)
	}
	return nil
}

func isID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for byteIndex, runeValue := range value {
		if byteIndex == 8 || byteIndex == 13 || byteIndex == 18 || byteIndex == 23 {
			if runeValue != '-' {
				return false
			}
			continue
		}
		if (runeValue < '0' || runeValue > '9') && (runeValue < 'a' || runeValue > 'f') {
			return false
		}
	}
	return true
}

func validateRegistry(registry Registry) error {
	if registry.Version != Version {
		return fmt.Errorf("%w: unsupported registry version %d", ErrInvalidState, registry.Version)
	}
	if registry.Revision == 0 && len(registry.Projects) != 0 {
		return fmt.Errorf("%w: non-empty registry has no revision", ErrInvalidState)
	}
	seen := make(map[string]struct{})
	for _, record := range registry.Projects {
		if err := validateStateID(record.ID, "project ID"); err != nil {
			return err
		}
		if _, ok := seen[record.ID]; ok {
			return fmt.Errorf("%w: duplicate project ID %s", ErrInvalidState, record.ID)
		}
		seen[record.ID] = struct{}{}
		if err := validateStateName(record.Name, "project name"); err != nil {
			return err
		}
		if !filepath.IsAbs(record.Worktree) || filepath.Clean(record.Worktree) != record.Worktree {
			return fmt.Errorf("%w: project %s has an unnormalized worktree", ErrInvalidState, record.ID)
		}
		if err := validateStateContext(record.Instructions, "project instructions"); err != nil {
			return err
		}
		if record.CreatedAt.IsZero() {
			return fmt.Errorf("%w: project %s has no creation time", ErrInvalidState, record.ID)
		}
		if err := validateStateID(record.CoordinatorThreadID, "coordinator thread ID"); err != nil {
			return err
		}
		coordinators := 0
		foundCoordinator := false
		for _, thread := range record.Threads {
			if err := validateStateID(thread.ID, "thread ID"); err != nil {
				return err
			}
			if _, ok := seen[thread.ID]; ok {
				return fmt.Errorf("%w: duplicate thread ID %s", ErrInvalidState, thread.ID)
			}
			seen[thread.ID] = struct{}{}
			if err := validateStateName(thread.Name, "thread name"); err != nil {
				return err
			}
			if thread.Role != RoleCoordinator && thread.Role != RoleWorker {
				return fmt.Errorf("%w: thread %s has an unknown role", ErrInvalidState, thread.ID)
			}
			if thread.Role == RoleCoordinator {
				coordinators++
				if thread.ID == record.CoordinatorThreadID {
					foundCoordinator = true
				}
			}
			if err := validateStateContext(thread.Brief, "thread brief"); err != nil {
				return err
			}
			if thread.CreatedAt.IsZero() {
				return fmt.Errorf("%w: thread %s has no creation time", ErrInvalidState, thread.ID)
			}
		}
		if coordinators != 1 || !foundCoordinator {
			return fmt.Errorf("%w: project %s must have exactly one coordinator", ErrInvalidState, record.ID)
		}
	}
	return nil
}

func validateStateID(value, label string) error {
	if !isID(value) {
		return fmt.Errorf("%w: %s is not a lowercase UUID", ErrInvalidState, label)
	}
	return nil
}

func validateStateName(value, label string) error {
	if err := validateName(value, label); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, strings.TrimPrefix(err.Error(), ErrInvalidInput.Error()+": "))
	}
	return nil
}

func validateStateContext(value, label string) error {
	if err := validateContext(value, label); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, strings.TrimPrefix(err.Error(), ErrInvalidInput.Error()+": "))
	}
	return nil
}
