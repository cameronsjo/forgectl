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
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
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
	errNoChange     = errors.New("project: no change")
)

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

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
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Role        Role      `json:"role"`
	Brief       string    `json:"brief"`
	ActiveRunID string    `json:"active_run_id,omitempty"`
	Runs        []Binding `json:"runs,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Binding struct {
	RunID           string       `json:"run_id"`
	Member          string       `json:"member"`
	Harness         mail.Harness `json:"harness"`
	NativeSessionID string       `json:"native_session_id"`
	Worktree        string       `json:"worktree"`
	Socket          string       `json:"socket"`
	PaneID          string       `json:"pane_id"`
	BoundAt         time.Time    `json:"bound_at"`
}

type Enrollment string

const (
	EnrollmentUnbound Enrollment = "unbound"
	EnrollmentCurrent Enrollment = "current"
	EnrollmentStale   Enrollment = "stale"
)

type ProcessLiveness string

const ProcessLivenessUnknown ProcessLiveness = "unknown"

type Observation struct {
	ThreadID           string          `json:"thread_id"`
	Enrollment         Enrollment      `json:"enrollment"`
	NativeSessionKnown bool            `json:"native_session_known"`
	ProcessLiveness    ProcessLiveness `json:"process_liveness"`
	ObservedAt         time.Time       `json:"observed_at"`
	Reason             string          `json:"reason,omitempty"`
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
	registry, err := updateRegistry(ctx, root, true, func(registry *Registry, rootFD int) error {
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
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, rootFD int) error {
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

func (store Store) Bind(ctx context.Context, projectID, threadID, member, runID, expectedPreviousRunID string) (Registry, Thread, error) {
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
	if err := validateID(threadID, "thread ID"); err != nil {
		return Registry{}, Thread{}, err
	}
	if err := validateMember(member); err != nil {
		return Registry{}, Thread{}, err
	}
	if err := validateRunID(runID, "run ID"); err != nil {
		return Registry{}, Thread{}, err
	}
	if runID == "none" {
		return Registry{}, Thread{}, fmt.Errorf("%w: run ID none is reserved", ErrInvalidInput)
	}
	if expectedPreviousRunID != "" {
		if err := validateRunID(expectedPreviousRunID, "expected previous run ID"); err != nil {
			return Registry{}, Thread{}, err
		}
		if expectedPreviousRunID == "none" {
			return Registry{}, Thread{}, fmt.Errorf("%w: expected previous run ID none is reserved", ErrInvalidInput)
		}
	}
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, rootFD int) error {
		project, thread, err := registryThread(registry, projectID, threadID)
		if err != nil {
			return err
		}
		roster, err := readRoster(rootFD, projectID)
		if err != nil {
			return err
		}
		worker, found := roster.byName[member]
		if !roster.present || !found {
			return fmt.Errorf("%w: member is not enrolled", ErrConflict)
		}
		if worker.RunID == "" || worker.RunID != runID {
			return fmt.Errorf("%w: member run does not match", ErrConflict)
		}
		if !roleMatches(thread.Role, worker) {
			return fmt.Errorf("%w: member role does not match thread", ErrConflict)
		}
		candidate := bindingFromWorker(worker)
		if err := validateBinding(candidate); err != nil {
			return err
		}
		if thread.ActiveRunID == "" {
			if expectedPreviousRunID != "" {
				return fmt.Errorf("%w: expected previous run is not active", ErrConflict)
			}
			if projectHasRun(*project, runID) {
				return fmt.Errorf("%w: run ID was previously bound", ErrConflict)
			}
			thread.Runs = append(thread.Runs, candidate)
			thread.ActiveRunID = runID
			return nil
		}
		if expectedPreviousRunID != thread.ActiveRunID {
			return fmt.Errorf("%w: active run changed", ErrConflict)
		}
		activeIndex := activeBindingIndex(*thread)
		if activeIndex < 0 {
			return fmt.Errorf("%w: active binding is missing", ErrInvalidState)
		}
		active := &thread.Runs[activeIndex]
		if runID == thread.ActiveRunID {
			if !sameBindingIdentity(*active, candidate) {
				return fmt.Errorf("%w: active binding identity changed", ErrConflict)
			}
			if active.NativeSessionID != "" && active.NativeSessionID != candidate.NativeSessionID {
				return fmt.Errorf("%w: active native session changed", ErrConflict)
			}
			if active.NativeSessionID == "" && candidate.NativeSessionID != "" {
				active.NativeSessionID = candidate.NativeSessionID
				return nil
			}
			return errNoChange
		}
		if projectHasRun(*project, runID) {
			return fmt.Errorf("%w: run ID was previously bound", ErrConflict)
		}
		thread.Runs = append(thread.Runs, candidate)
		thread.ActiveRunID = runID
		return nil
	})
	if err != nil {
		return Registry{}, Thread{}, err
	}
	_, thread, err := registryThread(&registry, projectID, threadID)
	if err != nil {
		return Registry{}, Thread{}, err
	}
	return registry, *thread, nil
}

func (store Store) Inspect(ctx context.Context, projectID string) (Registry, Project, []Observation, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, Project{}, nil, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, Project{}, nil, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, Project{}, nil, err
	}
	return inspectStore(ctx, root, projectID)
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

func validateMember(value string) error {
	if err := mail.ValidateName(value); err != nil {
		return fmt.Errorf("%w: member: %v", ErrInvalidInput, err)
	}
	return nil
}

func validateRunID(value, label string) error {
	if !tokenPattern.MatchString(value) {
		return fmt.Errorf("%w: %s has an invalid token", ErrInvalidInput, label)
	}
	return nil
}

func validateOptionalToken(value, label string) error {
	if value != "" {
		return validateRunID(value, label)
	}
	return nil
}

func validateHarness(harness mail.Harness, label string) error {
	switch harness {
	case mail.HarnessClaude, mail.HarnessCodex, mail.HarnessHermes, mail.HarnessPi:
		return nil
	default:
		return fmt.Errorf("%w: %s has an unknown harness", ErrInvalidState, label)
	}
}

func validateBinding(binding Binding) error {
	if err := validateRunID(binding.RunID, "binding run ID"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if binding.RunID == "none" {
		return fmt.Errorf("%w: binding run ID none is reserved", ErrInvalidState)
	}
	if err := validateMember(binding.Member); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if err := validateHarness(binding.Harness, "binding harness"); err != nil {
		return err
	}
	if !filepath.IsAbs(binding.Worktree) {
		return fmt.Errorf("%w: binding worktree is not absolute", ErrInvalidState)
	}
	if binding.Socket != "" && !filepath.IsAbs(binding.Socket) {
		return fmt.Errorf("%w: binding socket is not absolute", ErrInvalidState)
	}
	if err := validateOptionalToken(binding.NativeSessionID, "binding native session ID"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if err := validateOptionalToken(binding.PaneID, "binding pane ID"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if binding.BoundAt.IsZero() {
		return fmt.Errorf("%w: binding has no bound time", ErrInvalidState)
	}
	return nil
}

func registryThread(registry *Registry, projectID, threadID string) (*Project, *Thread, error) {
	for projectIndex := range registry.Projects {
		project := &registry.Projects[projectIndex]
		if project.ID != projectID {
			continue
		}
		for threadIndex := range project.Threads {
			thread := &project.Threads[threadIndex]
			if thread.ID == threadID {
				return project, thread, nil
			}
		}
		return nil, nil, fmt.Errorf("%w: thread %s", ErrNotFound, threadID)
	}
	return nil, nil, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
}

func projectHasRun(project Project, runID string) bool {
	for _, thread := range project.Threads {
		for _, binding := range thread.Runs {
			if binding.RunID == runID {
				return true
			}
		}
	}
	return false
}

func activeBindingIndex(thread Thread) int {
	for bindingIndex := range thread.Runs {
		if thread.Runs[bindingIndex].RunID == thread.ActiveRunID {
			return bindingIndex
		}
	}
	return -1
}

func roleMatches(role Role, worker mail.Worker) bool {
	return (role == RoleCoordinator && worker.Coordinator) || (role == RoleWorker && !worker.Coordinator)
}

func bindingFromWorker(worker mail.Worker) Binding {
	return Binding{
		RunID:           worker.RunID,
		Member:          worker.Name,
		Harness:         worker.Harness,
		NativeSessionID: worker.ThreadID,
		Worktree:        worker.Worktree,
		Socket:          worker.Socket,
		PaneID:          worker.PaneID,
		BoundAt:         time.Now().UTC(),
	}
}

func sameBindingIdentity(binding Binding, candidate Binding) bool {
	return binding.RunID == candidate.RunID &&
		binding.Member == candidate.Member &&
		binding.Harness == candidate.Harness &&
		binding.Worktree == candidate.Worktree &&
		binding.Socket == candidate.Socket &&
		binding.PaneID == candidate.PaneID
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
		runs := make(map[string]struct{})
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
			if thread.ActiveRunID == "" && len(thread.Runs) != 0 {
				return fmt.Errorf("%w: thread %s has runs but no active run", ErrInvalidState, thread.ID)
			}
			if thread.ActiveRunID != "" && len(thread.Runs) == 0 {
				return fmt.Errorf("%w: thread %s has an active run but no history", ErrInvalidState, thread.ID)
			}
			for bindingIndex, binding := range thread.Runs {
				if err := validateBinding(binding); err != nil {
					return fmt.Errorf("%w: thread %s binding %d: %v", ErrInvalidState, thread.ID, bindingIndex, err)
				}
				if _, ok := runs[binding.RunID]; ok {
					return fmt.Errorf("%w: duplicate run ID %s in project %s", ErrInvalidState, binding.RunID, record.ID)
				}
				runs[binding.RunID] = struct{}{}
			}
			if thread.ActiveRunID != "" && thread.Runs[len(thread.Runs)-1].RunID != thread.ActiveRunID {
				return fmt.Errorf("%w: thread %s reactivates a retired run", ErrInvalidState, thread.ID)
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
