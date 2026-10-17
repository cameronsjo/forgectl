package project

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// SurfaceFinish is the only accepted terminal outcome for a surface
// operation. Constructors keep the outcome matrix explicit at call sites.
type SurfaceFinish struct {
	phase    SurfacePhase
	ref      *backend.Ref
	tag      string
	category SurfaceErrorCategory
}

func SurfaceVerified(ref backend.Ref) SurfaceFinish {
	return SurfaceFinish{phase: SurfacePhaseVerified, ref: refPtr(ref)}
}

func SurfaceFailed(category SurfaceErrorCategory) SurfaceFinish {
	return SurfaceFinish{phase: SurfacePhaseFailed, category: category}
}

func SurfaceCleaned(ref backend.Ref, category SurfaceErrorCategory) SurfaceFinish {
	return SurfaceFinish{phase: SurfacePhaseFailed, ref: refPtr(ref), category: category}
}

func SurfaceUnknown(tag backend.RecoveryTag, category SurfaceErrorCategory, ref *backend.Ref) SurfaceFinish {
	return SurfaceFinish{phase: SurfacePhaseUnknown, tag: tag.String(), category: category, ref: ref}
}

// BeginWorkspace records intent before a native workspace is created.
func (store Store) BeginWorkspace(ctx context.Context, projectID, session string, tag backend.RecoveryTag, expectedServer backend.ServerID, expectedEndpoint backend.EndpointID) (Registry, SurfaceOperation, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateRunID(session, "surface session"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if !tag.Valid() {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: invalid recovery tag", ErrInvalidInput)
	}
	if !expectedServer.Valid() {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: invalid expected server", ErrInvalidInput)
	}
	if !expectedEndpoint.Valid() {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: invalid expected endpoint", ErrInvalidInput)
	}
	operationID, err := newID()
	if err != nil {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: generate surface operation ID: %v", ErrConflict, err)
	}
	operation := SurfaceOperation{
		ID: operationID, Kind: SurfaceKindWorkspace, Session: session, Tag: tag.String(),
		ExpectedServer: expectedServer.String(), ExpectedEndpoint: expectedEndpoint.String(),
		Phase: SurfacePhasePending, StartedAt: time.Now().UTC(),
	}
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, _ int) error {
		project, err := registryProject(registry, projectID)
		if err != nil {
			return err
		}
		if project.Workspace != nil {
			return fmt.Errorf("%w: project already has a verified workspace", ErrConflict)
		}
		for _, existing := range project.SurfaceOperations {
			if existing.Kind == SurfaceKindWorkspace && (existing.Phase == SurfacePhasePending || existing.Phase == SurfacePhaseUnknown || existing.Phase == SurfacePhaseVerified) {
				return fmt.Errorf("%w: project has an active workspace operation", ErrConflict)
			}
		}
		project.SurfaceOperations = append(project.SurfaceOperations, operation)
		return nil
	})
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	return registry, operation, nil
}

// BeginThread records intent for a child surface after verifying the enrolled
// member and the exact current project workspace identity.
func (store Store) BeginThread(ctx context.Context, projectID, threadID, runID, session string, tag backend.RecoveryTag, parent backend.Ref) (Registry, SurfaceOperation, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateID(threadID, "thread ID"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateRunID(runID, "surface run ID"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if runID == "none" {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: surface run ID none is reserved", ErrInvalidInput)
	}
	if err := validateRunID(session, "surface session"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if !tag.Valid() {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: invalid recovery tag", ErrInvalidInput)
	}
	if err := parent.Validate(); err != nil {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: invalid parent reference: %v", ErrInvalidInput, err)
	}
	if err := validateParentForThread(parent); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	operationID, err := newID()
	if err != nil {
		return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: generate surface operation ID: %v", ErrConflict, err)
	}
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, rootFD int) error {
		project, thread, err := registryThread(registry, projectID, threadID)
		if err != nil {
			return err
		}
		if project.Workspace == nil {
			return fmt.Errorf("%w: project has no verified workspace", ErrConflict)
		}
		if project.Workspace.Session != session || !refsEqual(project.Workspace.Ref, parent) {
			return fmt.Errorf("%w: parent or named session does not match project workspace", ErrConflict)
		}
		if thread.ActiveRunID != runID {
			return fmt.Errorf("%w: run is not the thread's active run", ErrConflict)
		}
		activeIndex := activeBindingIndex(*thread)
		if activeIndex < 0 {
			return fmt.Errorf("%w: active binding is missing", ErrInvalidState)
		}
		active := thread.Runs[activeIndex]
		roster, err := readRoster(rootFD, projectID)
		if err != nil {
			return err
		}
		worker, found := roster.byName[active.Member]
		if !roster.present || !found || worker.RunID != active.RunID || !roleMatches(thread.Role, worker) {
			return fmt.Errorf("%w: active member is not current in roster", ErrConflict)
		}
		candidate := bindingFromWorker(worker)
		if !sameBindingIdentity(active, candidate) || active.NativeSessionID != candidate.NativeSessionID {
			return fmt.Errorf("%w: active binding is stale", ErrConflict)
		}
		for _, existing := range project.SurfaceOperations {
			if existing.Kind != SurfaceKindThread || existing.ThreadID != threadID {
				continue
			}
			switch existing.Phase {
			case SurfacePhasePending, SurfacePhaseUnknown:
				return fmt.Errorf("%w: thread has an unresolved surface operation", ErrConflict)
			case SurfacePhaseVerified:
				return fmt.Errorf("%w: thread has an active verified surface; close it before starting another", ErrConflict)
			}
		}
		endpoint, _ := project.Workspace.Ref.Endpoint()
		operation := SurfaceOperation{
			ID: operationID, Kind: SurfaceKindThread, ThreadID: threadID, RunID: runID,
			Session: session, Tag: tag.String(), ExpectedServer: project.Workspace.Ref.Server().String(),
			ExpectedEndpoint: endpoint.String(), Phase: SurfacePhasePending,
			StartedAt: time.Now().UTC(), Parent: refPtr(parent),
		}
		project.SurfaceOperations = append(project.SurfaceOperations, operation)
		return nil
	})
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	return findOperation(registry, projectID, operationID)
}

// FinishSurface performs an exact compare-and-swap from pending to one
// terminal outcome. A finished operation cannot be replayed or overwritten.
func (store Store) FinishSurface(ctx context.Context, projectID, operationID string, result SurfaceFinish) (Registry, SurfaceOperation, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	if err := validateID(operationID, "surface operation ID"); err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, _ int) error {
		project, err := registryProject(registry, projectID)
		if err != nil {
			return err
		}
		operationIndex := -1
		for index := range project.SurfaceOperations {
			if project.SurfaceOperations[index].ID == operationID {
				operationIndex = index
				break
			}
		}
		if operationIndex < 0 {
			return fmt.Errorf("%w: surface operation %s", ErrNotFound, operationID)
		}
		operation := &project.SurfaceOperations[operationIndex]
		if operation.Phase != SurfacePhasePending {
			return fmt.Errorf("%w: surface operation is already %s", ErrConflict, operation.Phase)
		}
		if err := validateFinish(*operation, result); err != nil {
			return err
		}
		finishedAt := time.Now().UTC()
		operation.Phase, operation.FinishedAt, operation.Ref, operation.ErrorCategory = result.phase, &finishedAt, result.ref, result.category
		if result.phase == SurfacePhaseVerified && operation.Kind == SurfaceKindWorkspace {
			project.Workspace = &Workspace{Session: operation.Session, Ref: *result.ref}
		}
		return nil
	})
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	return findOperation(registry, projectID, operationID)
}

func validateFinish(operation SurfaceOperation, result SurfaceFinish) error {
	if result.ref != nil {
		if operation.Kind == SurfaceKindWorkspace {
			if err := validateCompleteWorkspaceReference(*result.ref); err != nil {
				return fmt.Errorf("%w: workspace reference: %v", ErrConflict, err)
			}
		} else if err := validateChildReference(operation, *result.ref); err != nil {
			return err
		}
	}
	if result.phase == SurfacePhaseVerified {
		if result.ref == nil || result.ref.Validate() != nil {
			return fmt.Errorf("%w: verified outcome needs a valid reference", ErrInvalidInput)
		}
		if result.ref.Tag().String() != operation.Tag || result.ref.Server().String() != operation.ExpectedServer {
			return fmt.Errorf("%w: reference server or tag does not match operation", ErrConflict)
		}
		endpoint, ok := result.ref.Endpoint()
		if !ok || endpoint.String() != operation.ExpectedEndpoint {
			return fmt.Errorf("%w: reference endpoint does not match operation", ErrConflict)
		}
		return nil
	}
	if result.phase != SurfacePhaseFailed && result.phase != SurfacePhaseUnknown {
		return fmt.Errorf("%w: invalid terminal outcome", ErrInvalidInput)
	}
	if !validErrorCategory(result.category) {
		return fmt.Errorf("%w: invalid error category", ErrInvalidInput)
	}
	if result.phase == SurfacePhaseFailed && result.ref != nil {
		if err := result.ref.Validate(); err != nil {
			return fmt.Errorf("%w: cleaned outcome reference: %v", ErrInvalidInput, err)
		}
		if err := validateOperationReference(operation, *result.ref); err != nil {
			return fmt.Errorf("%w: cleaned reference: %v", ErrConflict, err)
		}
	}
	if result.phase == SurfacePhaseUnknown {
		if result.tag != operation.Tag {
			return fmt.Errorf("%w: unknown outcome tag does not match operation", ErrConflict)
		}
		if result.ref != nil {
			if err := result.ref.Validate(); err != nil {
				return fmt.Errorf("%w: unknown outcome reference: %v", ErrInvalidInput, err)
			}
			if err := validateOperationReference(operation, *result.ref); err != nil {
				return fmt.Errorf("%w: unknown outcome reference: %v", ErrConflict, err)
			}
		}
	}
	return nil
}

func validateOperationReference(operation SurfaceOperation, ref backend.Ref) error {
	if ref.Tag().String() != operation.Tag || ref.Server().String() != operation.ExpectedServer {
		return errors.New("reference ownership or server does not match operation")
	}
	endpoint, ok := ref.Endpoint()
	if !ok || endpoint.String() != operation.ExpectedEndpoint {
		return errors.New("reference endpoint does not match operation")
	}
	return nil
}

func validateChildReference(operation SurfaceOperation, ref backend.Ref) error {
	if operation.Parent == nil {
		return fmt.Errorf("%w: thread operation has no parent", ErrInvalidState)
	}
	if err := validateCompleteWorkspaceReference(*operation.Parent); err != nil {
		return fmt.Errorf("%w: thread parent: %v", ErrConflict, err)
	}
	if err := validateCompleteWorkspaceReference(ref); err != nil {
		return fmt.Errorf("%w: thread reference: %v", ErrConflict, err)
	}
	child, _ := ref.HerdrIdentity()
	parent, _ := operation.Parent.HerdrIdentity()
	if parent.Workspace() != child.Workspace() {
		return fmt.Errorf("%w: thread reference belongs to another workspace", ErrConflict)
	}
	if ref.Tag() == operation.Parent.Tag() {
		return fmt.Errorf("%w: thread reference reuses the parent tag", ErrConflict)
	}
	if child.Tab() == parent.Tab() || child.Pane() == parent.Pane() || child.Terminal() == parent.Terminal() {
		return fmt.Errorf("%w: thread reference reuses a parent tab, pane, or terminal", ErrConflict)
	}
	return nil
}

func validateParentForThread(parent backend.Ref) error {
	if err := validateCompleteWorkspaceReference(parent); err != nil {
		return fmt.Errorf("%w: thread parent: %v", ErrConflict, err)
	}
	return nil
}

func validateCompleteWorkspaceReference(ref backend.Ref) error {
	if ref.Kind() != backend.KindHerdr || ref.Source() != backend.HerdrNamedSessionServer() {
		return errors.New("reference is not from a named herdr session")
	}
	identity, err := ref.HerdrIdentity()
	if err != nil || identity.Workspace() == "" || identity.Tab() == "" || identity.Pane() == "" || identity.Terminal() == "" {
		return errors.New("reference is not a complete workspace identity")
	}
	if _, ok := ref.Endpoint(); !ok {
		return errors.New("reference has no endpoint")
	}
	return nil
}

func registryProject(registry *Registry, projectID string) (*Project, error) {
	for index := range registry.Projects {
		if registry.Projects[index].ID == projectID {
			return &registry.Projects[index], nil
		}
	}
	return nil, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
}

func findOperation(registry Registry, projectID, operationID string) (Registry, SurfaceOperation, error) {
	project, err := registryProject(&registry, projectID)
	if err != nil {
		return Registry{}, SurfaceOperation{}, err
	}
	for _, operation := range project.SurfaceOperations {
		if operation.ID == operationID {
			return registry, operation, nil
		}
	}
	return Registry{}, SurfaceOperation{}, fmt.Errorf("%w: surface operation %s", ErrNotFound, operationID)
}

func refsEqual(left, right backend.Ref) bool {
	leftData, leftErr := left.MarshalJSON()
	rightData, rightErr := right.MarshalJSON()
	return leftErr == nil && rightErr == nil && string(leftData) == string(rightData)
}

func refPtr(ref backend.Ref) *backend.Ref {
	return &ref
}
