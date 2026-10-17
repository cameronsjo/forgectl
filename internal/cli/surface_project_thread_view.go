package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func newSurfaceProjectThreadOpenCmd(deps module.Deps, root, projectID *string) *cobra.Command {
	var options projectPresentationOptions
	var nativeOptions projectNativeViewOptions
	var view string
	command := &cobra.Command{
		Use:   "open <thread-id>",
		Short: "Open an explicitly selected view of a current enrolled thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if *projectID == "" || view == "" {
				return projectUsage(errors.New("--project and --view are required"))
			}
			if err := options.validate(); err != nil {
				return err
			}
			if view != "timeline" && view != "native" {
				return projectUsage(errors.New("--view must be timeline or native; fallback is never inferred"))
			}
			if err := nativeOptions.validate(view); err != nil {
				return err
			}
			store := project.Store{Root: *root}
			registry, record, observations, err := store.Inspect(command.Context(), *projectID)
			if err != nil {
				return projectError(err)
			}
			thread, binding, err := projectCurrentThread(record, observations, arguments[0])
			if err != nil {
				return projectError(err)
			}
			if record.Workspace == nil || record.Workspace.Session != options.Session {
				return projectError(fmt.Errorf("%w: open the project workspace in the exact named session first", project.ErrConflict))
			}
			selectedPresentation := project.SurfacePresentationTimeline
			if view == "native" {
				if err := nativeOptions.validateBinding(binding); err != nil {
					return projectError(err)
				}
				selectedPresentation = project.SurfacePresentationCodexRemote
			}
			presentation, err := projectPresentationFor(deps, options.Binary, options.Session, command.ErrOrStderr())
			if err != nil {
				return projectError(err)
			}
			for _, operation := range record.SurfaceOperations {
				if operation.Kind != project.SurfaceKindThread || operation.ThreadID != thread.ID || operation.RunID != binding.RunID || operation.Phase != project.SurfacePhaseVerified || operation.Retirement != nil {
					continue
				}
				if operation.Presentation != selectedPresentation {
					return projectError(fmt.Errorf("%w: existing tab uses a different presentation; close its view explicitly", project.ErrConflict))
				}
				if err := presentation.FocusThread(command.Context(), record.Workspace.Ref, *operation.Ref); err != nil {
					return projectError(err)
				}
				warnings := []string{}
				if warning := projectThreadMetadataWarning(presentation, command.Context(), record.Workspace.Ref, *operation.Ref, thread.Name); warning != "" {
					warnings = append(warnings, warning)
				}
				return writeProjectPresentation(command.OutOrStdout(), projectPresentationReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, ThreadID: thread.ID, RunID: binding.RunID, Action: "focused", Focused: true, Operation: &operation, Workspace: record.Workspace, ProcessLiveness: project.ProcessLivenessUnknown, Warnings: warnings}, options.JSON)
			}
			if view == "native" {
				return createProjectThreadNative(command, store, record, thread, binding, presentation, options, nativeOptions)
			}
			return createProjectThreadTimeline(command, store, record, thread, binding, presentation, options)
		},
	}
	options.flags(command)
	command.Flags().StringVar(&view, "view", "", "explicit presentation: timeline or supported native; never an automatic fallback")
	nativeOptions.flags(command)
	return withProjectRoot(root, command)
}

func projectCurrentThread(record project.Project, observations []project.Observation, threadID string) (project.Thread, project.Binding, error) {
	observation, err := projectObservation(observations, threadID)
	if err != nil {
		return project.Thread{}, project.Binding{}, err
	}
	if observation.Enrollment != project.EnrollmentCurrent {
		return project.Thread{}, project.Binding{}, fmt.Errorf("%w: thread enrollment is %s", project.ErrConflict, observation.Enrollment)
	}
	for _, thread := range record.Threads {
		if thread.ID == threadID {
			for _, binding := range thread.Runs {
				if binding.RunID == thread.ActiveRunID {
					return thread, binding, nil
				}
			}
		}
	}
	return project.Thread{}, project.Binding{}, fmt.Errorf("%w: current thread binding is unavailable", project.ErrConflict)
}

func createProjectThreadTimeline(command *cobra.Command, store project.Store, record project.Project, thread project.Thread, binding project.Binding, presentation projectPresentation, options projectPresentationOptions) error {
	self, err := surface.SelfPath()
	if err != nil {
		return projectError(err)
	}
	bootstrap, err := projectThreadTimelineBootstrap(self, store.Root, record.ID, thread.ID)
	if err != nil {
		return projectError(err)
	}
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		return projectError(err)
	}
	spec, err := backend.NewStartSpec(binding.Worktree, "thread-"+thread.ID, tag, bootstrap)
	if err != nil {
		return projectError(err)
	}
	adapter, err := presentation.ThreadAdapter(record.Workspace.Ref)
	if err != nil {
		return projectError(err)
	}
	_, operation, err := store.BeginThread(command.Context(), record.ID, thread.ID, binding.RunID, options.Session, project.SurfacePresentationTimeline, tag, record.Workspace.Ref)
	if err != nil {
		return projectError(err)
	}
	result := adapter.Start(command.Context(), spec)
	finishContext, cancel := context.WithTimeout(context.WithoutCancel(command.Context()), 5*time.Second)
	defer cancel()
	registry, completed, err := store.FinishSurface(finishContext, record.ID, operation.ID, projectWorkspaceFinish(result, tag))
	if err != nil {
		return projectError(fmt.Errorf("thread operation %s remains unresolved: %w", operation.ID, err))
	}
	report := projectPresentationReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, ThreadID: thread.ID, RunID: binding.RunID, Action: "create-view", Operation: &completed, Workspace: record.Workspace, ProcessLiveness: project.ProcessLivenessUnknown}
	var focusError error
	warnings := []string{}
	if completed.Phase == project.SurfacePhaseVerified {
		_, freshProject, observations, inspectError := store.Inspect(command.Context(), record.ID)
		focusError = inspectError
		if focusError == nil {
			_, freshBinding, currentError := projectCurrentThread(freshProject, observations, thread.ID)
			focusError = currentError
			if focusError == nil && freshBinding.RunID != binding.RunID {
				focusError = fmt.Errorf("%w: thread run changed after view creation", project.ErrConflict)
			}
		}
		if focusError == nil {
			focusError = presentation.FocusThread(command.Context(), record.Workspace.Ref, *completed.Ref)
		}
		report.Focused = focusError == nil
		if focusError == nil {
			if warning := projectThreadMetadataWarning(presentation, command.Context(), record.Workspace.Ref, *completed.Ref, thread.Name); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}
	report.Warnings = warnings
	if err := writeProjectPresentation(command.OutOrStdout(), report, options.JSON); err != nil {
		return err
	}
	if completed.Phase != project.SurfacePhaseVerified {
		return projectError(fmt.Errorf("thread operation %s ended %s; inspect it before any retry", completed.ID, completed.Phase))
	}
	if focusError != nil {
		return projectError(fmt.Errorf("thread view was recorded but focus failed: %w", focusError))
	}
	return nil
}

func newSurfaceProjectThreadCloseViewCmd(deps module.Deps, root, projectID *string) *cobra.Command {
	var options projectPresentationOptions
	var operationID string
	command := &cobra.Command{
		Use:   "close-view <thread-id>",
		Short: "Close one exact presentation tab, not its enrolled native process or project workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if *projectID == "" || operationID == "" {
				return projectUsage(errors.New("--project and --operation are required"))
			}
			if err := options.validate(); err != nil {
				return err
			}
			store := project.Store{Root: *root}
			_, record, _, err := store.Inspect(command.Context(), *projectID)
			if err != nil {
				return projectError(err)
			}
			var selected *project.SurfaceOperation
			for _, operation := range record.SurfaceOperations {
				if operation.ID == operationID && operation.Kind == project.SurfaceKindThread && operation.ThreadID == arguments[0] {
					selected = &operation
				}
			}
			if selected == nil || selected.Parent == nil || selected.Ref == nil || selected.Session != options.Session {
				return projectError(fmt.Errorf("%w: exact thread view operation is unavailable in that named session", project.ErrConflict))
			}
			presentation, err := projectPresentationFor(deps, options.Binary, options.Session, command.ErrOrStderr())
			if err != nil {
				return projectError(err)
			}
			adapter, err := presentation.ThreadAdapter(*selected.Parent)
			if err != nil {
				return projectError(err)
			}
			_, retirement, err := store.BeginThreadClose(command.Context(), record.ID, selected.ID)
			if err != nil {
				return projectError(err)
			}
			result := adapter.Close(command.Context(), *selected.Ref)
			finish := project.RetirementUnknownOutcome(project.SurfaceErrorCleanup)
			if result.Validate() == nil {
				switch result.State() {
				case backend.CloseClosed:
					finish = project.RetirementClosedOutcome()
				case backend.CloseAlreadyGone:
					finish = project.RetirementAlreadyGoneOutcome()
				}
			}
			finishContext, cancel := context.WithTimeout(context.WithoutCancel(command.Context()), 5*time.Second)
			defer cancel()
			registry, retired, err := store.FinishThreadClose(finishContext, record.ID, selected.ID, retirement.ID, finish)
			if err != nil {
				return projectError(fmt.Errorf("view retirement %s remains unresolved: %w", retirement.ID, err))
			}
			selected.Retirement = &retired
			if err := writeProjectPresentation(command.OutOrStdout(), projectPresentationReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, ThreadID: selected.ThreadID, RunID: selected.RunID, Action: "close-view", Operation: selected, Workspace: record.Workspace, ProcessLiveness: project.ProcessLivenessUnknown}, options.JSON); err != nil {
				return err
			}
			if retired.Phase == project.RetirementUnknown {
				return projectError(fmt.Errorf("view retirement %s is unknown; no retry or native stop is implied", retired.ID))
			}
			return nil
		},
	}
	options.flags(command)
	command.Flags().StringVar(&operationID, "operation", "", "exact persisted thread-view operation ID")
	return withProjectRoot(root, command)
}
