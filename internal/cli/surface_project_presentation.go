package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type projectPresentation interface {
	StartProjectWorkspace(context.Context, backend.StartSpec, func(backend.ServerID, backend.EndpointID) error) backend.StartResult
	FocusProjectWorkspace(context.Context, backend.Ref) error
	FocusThread(context.Context, backend.Ref, backend.Ref) error
	ThreadAdapter(backend.Ref) (backend.Capabilities, error)
}

type projectMetadataPresentation interface {
	ShowProjectWorkspace(context.Context, backend.Ref, string) error
	ShowProjectThread(context.Context, backend.Ref, backend.Ref, string) error
}

type projectHerdrPresentation struct{ *herdradapter.Adapter }

func (presentation projectHerdrPresentation) ThreadAdapter(parent backend.Ref) (backend.Capabilities, error) {
	return presentation.ForProjectTab(parent)
}

func (presentation projectHerdrPresentation) FocusThread(ctx context.Context, parent, child backend.Ref) error {
	adapter, err := presentation.ForProjectTab(parent)
	if err != nil {
		return err
	}
	return adapter.Focus(ctx, child)
}

func (presentation projectHerdrPresentation) ShowProjectWorkspace(ctx context.Context, ref backend.Ref, projectName string) error {
	return presentation.Adapter.ShowProjectWorkspace(ctx, ref, projectName)
}

func (presentation projectHerdrPresentation) ShowProjectThread(ctx context.Context, parent, child backend.Ref, threadName string) error {
	adapter, err := presentation.ForProjectTab(parent)
	if err != nil {
		return err
	}
	return adapter.ShowProjectThread(ctx, child, threadName)
}

var projectPresentationFor = newProjectPresentation

func newProjectPresentation(deps module.Deps, binary, session string, warnings io.Writer) (projectPresentation, error) {
	if deps.SensitiveRunner == nil {
		return nil, errors.New("project presentation requires the sensitive command runner")
	}
	info, err := os.Stat(binary)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("--herdr-binary must name an executable regular file")
	}
	adapter, err := herdradapter.New(deps.SensitiveRunner, binary, func(key string) string {
		if key == "HERDR_SESSION" {
			return session
		}
		return os.Getenv(key)
	}, herdradapter.WithWarnings(warnings))
	if err != nil {
		return nil, err
	}
	return projectHerdrPresentation{adapter}, nil
}

type projectPresentationOptions struct {
	Session string
	Binary  string
	JSON    bool
}

func (options *projectPresentationOptions) flags(command *cobra.Command) {
	command.Flags().StringVar(&options.Session, "session", "", "explicit named herdr session; never inferred from the caller")
	command.Flags().StringVar(&options.Binary, "herdr-binary", "", "absolute operator-selected herdr executable")
	command.Flags().BoolVar(&options.JSON, "json", false, "print persisted identity and presentation outcome, not native liveness")
}

func (options projectPresentationOptions) validate() error {
	if options.Session == "" || !filepath.IsAbs(options.Binary) {
		return projectUsage(errors.New("--session and an absolute --herdr-binary are required"))
	}
	if strings.HasPrefix(options.Session, "-") || len(options.Session) > 128 {
		return projectUsage(errors.New("--session must be a bounded plain herdr session name"))
	}
	for _, character := range options.Session {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return projectUsage(errors.New("--session must contain only letters, digits, dots, underscores, and hyphens"))
		}
	}
	return nil
}

type projectPresentationReport struct {
	Version         int                       `json:"version"`
	Revision        uint64                    `json:"revision"`
	ProjectID       string                    `json:"project_id"`
	ThreadID        string                    `json:"thread_id,omitempty"`
	RunID           string                    `json:"run_id,omitempty"`
	Action          string                    `json:"action"`
	Focused         bool                      `json:"focused"`
	Operation       *project.SurfaceOperation `json:"operation,omitempty"`
	Workspace       *project.Workspace        `json:"workspace,omitempty"`
	ProcessLiveness project.ProcessLiveness   `json:"process_liveness"`
	Warnings        []string                  `json:"warnings,omitempty"`
}

const projectMetadataWarning = "project display metadata could not be updated"

func projectWorkspaceMetadataWarning(presentation projectPresentation, ctx context.Context, ref backend.Ref, projectName string) string {
	metadata, ok := presentation.(projectMetadataPresentation)
	if !ok {
		return ""
	}
	if err := metadata.ShowProjectWorkspace(ctx, ref, projectMetadataName(projectName)); err != nil {
		return projectMetadataWarning
	}
	return ""
}

func projectThreadMetadataWarning(presentation projectPresentation, ctx context.Context, parent, child backend.Ref, threadName string) string {
	metadata, ok := presentation.(projectMetadataPresentation)
	if !ok {
		return ""
	}
	if err := metadata.ShowProjectThread(ctx, parent, child, projectMetadataName(threadName)); err != nil {
		return projectMetadataWarning
	}
	return ""
}

func projectMetadataName(name string) string {
	name = strings.TrimSpace(name)
	safe := termsafe.SafeLine(name)
	if utf8.RuneCountInString(safe) > herdradapter.MaxTokenValueLen {
		safe = termsafe.SafeLineMax(name, herdradapter.MaxTokenValueLen-utf8.RuneCountInString(termsafe.TruncatedMarker))
	}
	return safe
}

func newSurfaceProjectOpenCmd(deps module.Deps, root *string) *cobra.Command {
	var options projectPresentationOptions
	command := &cobra.Command{
		Use:   "open <project-id>",
		Short: "Create or focus the project's exact herdr workspace and Agents overview",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if err := options.validate(); err != nil {
				return err
			}
			store := project.Store{Root: *root}
			_, record, _, err := store.Inspect(command.Context(), arguments[0])
			if err != nil {
				return projectError(err)
			}
			if record.Workspace != nil && record.Workspace.Session != options.Session {
				return projectError(fmt.Errorf("%w: requested session differs from the saved project workspace", project.ErrConflict))
			}
			presentation, err := projectPresentationFor(deps, options.Binary, options.Session, command.ErrOrStderr())
			if err != nil {
				return projectError(err)
			}
			if record.Workspace != nil {
				freshRegistry, freshRecord, workspaceOperation, err := inspectCurrentProjectWorkspace(command.Context(), store, record.ID, options.Session)
				if err != nil {
					return projectError(err)
				}
				if err := presentation.FocusProjectWorkspace(command.Context(), freshRecord.Workspace.Ref); err != nil {
					return projectError(err)
				}
				warnings := []string{}
				if warning := projectWorkspaceMetadataWarning(presentation, command.Context(), freshRecord.Workspace.Ref, freshRecord.Name); warning != "" {
					warnings = append(warnings, warning)
				}
				workspace := *freshRecord.Workspace
				return writeProjectPresentation(command.OutOrStdout(), projectPresentationReport{Version: freshRegistry.Version, Revision: freshRegistry.Revision, ProjectID: freshRecord.ID, Action: "focused", Focused: true, Operation: &workspaceOperation, Workspace: &workspace, ProcessLiveness: project.ProcessLivenessUnknown, Warnings: warnings}, options.JSON)
			}
			return createProjectWorkspace(command, store, record, presentation, options)
		},
	}
	options.flags(command)
	return withProjectRoot(root, command)
}

func createProjectWorkspace(command *cobra.Command, store project.Store, record project.Project, presentation projectPresentation, options projectPresentationOptions) error {
	worktree, err := os.Stat(record.Worktree)
	if err != nil || !worktree.IsDir() {
		return projectError(errors.New("project working directory is unavailable; no workspace created"))
	}
	self, err := surface.SelfPath()
	if err != nil {
		return projectError(err)
	}
	bootstrap, err := projectAgentsBootstrap(self, store.Root, record.ID, options.Session, options.Binary)
	if err != nil {
		return projectError(err)
	}
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		return projectError(err)
	}
	spec, err := backend.NewStartSpec(record.Worktree, "project-"+record.ID, tag, bootstrap)
	if err != nil {
		return projectError(err)
	}
	var operation project.SurfaceOperation
	var beginError error
	result := presentation.StartProjectWorkspace(command.Context(), spec, func(server backend.ServerID, endpoint backend.EndpointID) error {
		_, operation, beginError = store.BeginWorkspace(command.Context(), record.ID, options.Session, tag, server, endpoint)
		return beginError
	})
	if beginError != nil {
		return projectError(beginError)
	}
	if operation.ID == "" {
		if cause, ok := result.Cause(); ok {
			return projectError(fmt.Errorf("workspace was not created: %w", cause))
		}
		return projectError(errors.New("workspace creation returned without a durable operation"))
	}
	finish := projectWorkspaceFinish(result, tag)
	finishContext, cancel := context.WithTimeout(context.WithoutCancel(command.Context()), 5*time.Second)
	defer cancel()
	registry, completed, err := store.FinishSurface(finishContext, record.ID, operation.ID, finish)
	if err != nil {
		return projectError(fmt.Errorf("workspace operation %s remains unresolved: %w", operation.ID, err))
	}
	report := projectPresentationReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, Action: "create", Operation: &completed, ProcessLiveness: project.ProcessLivenessUnknown}
	var focusError error
	warnings := []string{}
	if completed.Phase == project.SurfacePhaseVerified {
		freshRegistry, freshRecord, freshOperation, inspectError := inspectCurrentProjectWorkspace(command.Context(), store, record.ID, options.Session)
		focusError = inspectError
		if focusError == nil && freshOperation.ID != completed.ID {
			focusError = fmt.Errorf("%w: workspace operation changed after creation", project.ErrConflict)
		}
		if focusError == nil {
			workspace := *freshRecord.Workspace
			report.Version, report.Revision = freshRegistry.Version, freshRegistry.Revision
			report.Workspace = &workspace
			focusError = presentation.FocusProjectWorkspace(command.Context(), workspace.Ref)
		}
		report.Focused = focusError == nil
		if focusError == nil {
			if warning := projectWorkspaceMetadataWarning(presentation, command.Context(), report.Workspace.Ref, record.Name); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}
	report.Warnings = warnings
	if err := writeProjectPresentation(command.OutOrStdout(), report, options.JSON); err != nil {
		return err
	}
	if completed.Phase != project.SurfacePhaseVerified {
		return projectError(fmt.Errorf("workspace operation %s ended %s; inspect it before any retry", completed.ID, completed.Phase))
	}
	if focusError != nil {
		return projectError(fmt.Errorf("workspace was recorded but focus failed: %w", focusError))
	}
	return nil
}

func inspectCurrentProjectWorkspace(ctx context.Context, store project.Store, projectID, session string) (project.Registry, project.Project, project.SurfaceOperation, error) {
	registry, record, _, err := store.Inspect(ctx, projectID)
	if err != nil {
		return project.Registry{}, project.Project{}, project.SurfaceOperation{}, err
	}
	if record.Workspace == nil || record.Workspace.Session != session {
		return project.Registry{}, project.Project{}, project.SurfaceOperation{}, fmt.Errorf("%w: project has no verified workspace in that named session", project.ErrConflict)
	}
	var found project.SurfaceOperation
	for _, operation := range record.SurfaceOperations {
		if operation.Kind != project.SurfaceKindWorkspace || operation.Session != session || operation.Phase != project.SurfacePhaseVerified || operation.Ref == nil {
			continue
		}
		if operation.Ref.Validate() != nil {
			return project.Registry{}, project.Project{}, project.SurfaceOperation{}, fmt.Errorf("%w: saved workspace reference is invalid", project.ErrConflict)
		}
		equal, compareErr := sameProjectReference(*operation.Ref, record.Workspace.Ref)
		if compareErr != nil || !equal {
			return project.Registry{}, project.Project{}, project.SurfaceOperation{}, fmt.Errorf("%w: saved workspace operation does not match the project workspace", project.ErrConflict)
		}
		if found.ID != "" {
			return project.Registry{}, project.Project{}, project.SurfaceOperation{}, fmt.Errorf("%w: project has multiple verified workspace operations", project.ErrConflict)
		}
		found = operation
	}
	if found.ID == "" {
		return project.Registry{}, project.Project{}, project.SurfaceOperation{}, fmt.Errorf("%w: verified workspace operation is unavailable", project.ErrConflict)
	}
	return registry, record, found, nil
}

func inspectCurrentProjectThreadView(ctx context.Context, store project.Store, projectID, threadID, runID, session string, expectedPresentation project.SurfacePresentation, expectedOperationID string) (project.Registry, project.Project, project.Thread, project.Binding, project.SurfaceOperation, error) {
	registry, record, observations, err := store.Inspect(ctx, projectID)
	if err != nil {
		return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, err
	}
	thread, binding, err := projectCurrentThread(record, observations, threadID)
	if err != nil {
		return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, err
	}
	if binding.RunID != runID {
		return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, fmt.Errorf("%w: selected thread run changed before focus", project.ErrConflict)
	}
	if record.Workspace == nil || record.Workspace.Session != session {
		return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, fmt.Errorf("%w: project workspace changed before focus", project.ErrConflict)
	}
	var found project.SurfaceOperation
	for _, operation := range record.SurfaceOperations {
		if operation.Kind != project.SurfaceKindThread || operation.ThreadID != threadID || operation.RunID != runID || operation.Session != session || operation.Phase != project.SurfacePhaseVerified || operation.Retirement != nil {
			continue
		}
		if expectedPresentation != "" && expectedOperationID == "" && operation.Presentation != expectedPresentation {
			return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, fmt.Errorf("%w: existing tab uses a different presentation; close its view explicitly", project.ErrConflict)
		}
		if expectedOperationID != "" && operation.ID != expectedOperationID {
			continue
		}
		if expectedPresentation != "" && operation.Presentation != expectedPresentation {
			continue
		}
		if operation.Parent == nil || operation.Ref == nil || operation.Ref.Validate() != nil {
			return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, fmt.Errorf("%w: saved thread view reference is invalid", project.ErrConflict)
		}
		equal, compareErr := sameProjectReference(*operation.Parent, record.Workspace.Ref)
		if compareErr != nil || !equal {
			return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, fmt.Errorf("%w: saved thread view parent changed before focus", project.ErrConflict)
		}
		if found.ID != "" {
			return project.Registry{}, project.Project{}, project.Thread{}, project.Binding{}, project.SurfaceOperation{}, fmt.Errorf("%w: multiple verified views match the selected run", project.ErrConflict)
		}
		found = operation
	}
	return registry, record, thread, binding, found, nil
}

func sameProjectReference(left, right backend.Ref) (bool, error) {
	leftJSON, err := left.MarshalJSON()
	if err != nil {
		return false, err
	}
	rightJSON, err := right.MarshalJSON()
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftJSON, rightJSON), nil
}

func projectWorkspaceFinish(result backend.StartResult, tag backend.RecoveryTag) project.SurfaceFinish {
	if result.Validate() != nil {
		return project.SurfaceUnknown(tag, project.SurfaceErrorInternal, nil)
	}
	category := project.SurfaceErrorLaunch
	if cause, ok := result.Cause(); ok {
		switch cause.Class() {
		case backend.FailureCanceled:
			category = project.SurfaceErrorCanceled
		case backend.FailureIdentityMismatch:
			category = project.SurfaceErrorIdentity
		case backend.FailureUnavailable, backend.FailureIncompatible:
			category = project.SurfaceErrorServer
		case backend.FailurePermissionDenied, backend.FailureAuthentication:
			category = project.SurfaceErrorEndpoint
		case backend.FailureNameCollision, backend.FailureTargetBusy:
			category = project.SurfaceErrorConflict
		case backend.FailureInternal:
			category = project.SurfaceErrorInternal
		case backend.FailureMalformedResponse:
			category = project.SurfaceErrorUnknown
		}
	}
	switch result.Outcome() {
	case backend.NotMutated:
		return project.SurfaceFailed(category)
	case backend.RefKnown:
		ref, _ := result.Ref()
		if !result.Failed() {
			return project.SurfaceVerified(ref)
		}
		return project.SurfaceUnknown(tag, category, &ref)
	default:
		return project.SurfaceUnknown(tag, category, nil)
	}
}

func projectTimelineBootstrap(self, root, projectID string) (backend.BootstrapCommand, error) {
	return projectThreadTimelineBootstrap(self, root, projectID, "")
}

func projectThreadTimelineBootstrap(self, root, projectID, threadID string) (backend.BootstrapCommand, error) {
	words := []string{self, "surface", "project", "--root", root, "timeline", projectID, "--live"}
	if threadID != "" {
		words = append(words, "--thread", threadID)
	}
	for index, word := range words {
		quoted, err := shellQuote(word)
		if err != nil {
			return backend.BootstrapCommand{}, err
		}
		words[index] = quoted
	}
	return backend.NewBootstrapCommand(exec.Opaque(strings.Join(words, " ")))
}

func projectAgentsBootstrap(self, root, projectID, session, binary string) (backend.BootstrapCommand, error) {
	if !filepath.IsAbs(binary) {
		return backend.BootstrapCommand{}, errors.New("--herdr-binary must be absolute")
	}
	words := []string{self, "surface", "project", "--root", root, "agents", projectID, "--live", "--herdr-session", session, "--herdr-binary", binary}
	for index, word := range words {
		quoted, err := shellQuote(word)
		if err != nil {
			return backend.BootstrapCommand{}, err
		}
		words[index] = quoted
	}
	return backend.NewBootstrapCommand(exec.Opaque(strings.Join(words, " ")))
}

func newSurfaceProjectThreadFocusCmd(deps module.Deps, root, projectID *string) *cobra.Command {
	var options projectPresentationOptions
	command := &cobra.Command{
		Use:   "focus <thread-id>",
		Short: "Focus a current thread's verified tab; never launch or resume a native session",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if *projectID == "" {
				return projectUsage(errors.New("--project must name an exact project ID"))
			}
			if err := options.validate(); err != nil {
				return err
			}
			store := project.Store{Root: *root}
			_, record, observations, err := store.Inspect(command.Context(), *projectID)
			if err != nil {
				return projectError(err)
			}
			observation, err := projectObservation(observations, arguments[0])
			if err != nil {
				return projectError(err)
			}
			if observation.Enrollment != project.EnrollmentCurrent {
				return projectError(fmt.Errorf("%w: thread enrollment is %s; no focus attempted", project.ErrConflict, observation.Enrollment))
			}
			if record.Workspace == nil || record.Workspace.Session != options.Session {
				return projectError(fmt.Errorf("%w: project has no verified workspace in that named session", project.ErrConflict))
			}
			var runID, threadDisplayName string
			for _, thread := range record.Threads {
				if thread.ID == arguments[0] {
					runID = thread.ActiveRunID
					threadDisplayName = thread.Name
					break
				}
			}
			var operation *project.SurfaceOperation
			for index := range record.SurfaceOperations {
				candidate := &record.SurfaceOperations[index]
				if candidate.Kind == project.SurfaceKindThread && candidate.ThreadID == arguments[0] && candidate.RunID == runID && candidate.Phase == project.SurfacePhaseVerified && candidate.Retirement == nil {
					operation = candidate
				}
			}
			if operation == nil || operation.Ref == nil {
				return projectError(fmt.Errorf("%w: this run has no verified tab; native attachment is not inferred", project.ErrConflict))
			}
			presentation, err := projectPresentationFor(deps, options.Binary, options.Session, command.ErrOrStderr())
			if err != nil {
				return projectError(err)
			}
			freshRegistry, freshRecord, _, _, freshOperation, inspectError := inspectCurrentProjectThreadView(command.Context(), store, record.ID, arguments[0], runID, options.Session, "", operation.ID)
			if inspectError != nil {
				return projectError(inspectError)
			}
			if freshOperation.ID == "" || freshOperation.Parent == nil || freshOperation.Ref == nil {
				return projectError(fmt.Errorf("%w: this run has no verified tab; native attachment is not inferred", project.ErrConflict))
			}
			if err := presentation.FocusThread(command.Context(), *freshOperation.Parent, *freshOperation.Ref); err != nil {
				return projectError(err)
			}
			warnings := []string{}
			if warning := projectThreadMetadataWarning(presentation, command.Context(), *freshOperation.Parent, *freshOperation.Ref, threadDisplayName); warning != "" {
				warnings = append(warnings, warning)
			}
			workspace := freshRecord.Workspace
			return writeProjectPresentation(command.OutOrStdout(), projectPresentationReport{Version: freshRegistry.Version, Revision: freshRegistry.Revision, ProjectID: freshRecord.ID, ThreadID: arguments[0], RunID: runID, Action: "focused", Focused: true, Operation: &freshOperation, Workspace: workspace, ProcessLiveness: project.ProcessLivenessUnknown, Warnings: warnings}, options.JSON)
		},
	}
	options.flags(command)
	return withProjectRoot(root, command)
}

func writeProjectPresentation(output io.Writer, report projectPresentationReport, asJSON bool) error {
	if asJSON {
		return writeJSON(output, report)
	}
	phase := "verified surface identity"
	if report.Operation != nil {
		phase = string(report.Operation.Phase)
		if report.Operation.Retirement != nil {
			phase += "; view retirement=" + string(report.Operation.Retirement.Phase)
		}
	}
	_, err := fmt.Fprintf(output, "project %s: %s (%s), focused=%t; process liveness remains unknown\n", report.ProjectID, report.Action, phase, report.Focused)
	if err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		if _, err := fmt.Fprintf(output, "warning: %s\n", termsafe.SafeLineMax(warning, 256)); err != nil {
			return err
		}
	}
	return nil
}
