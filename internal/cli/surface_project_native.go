package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

type projectNativeViewOptions struct {
	Binary   string
	Provider string
	Model    string
}

func (options *projectNativeViewOptions) flags(command *cobra.Command) {
	command.Flags().StringVar(&options.Binary, "codex-binary", "", "absolute operator-selected Codex client for --view native")
	command.Flags().StringVar(&options.Provider, "expect-provider", "", "expected existing native provider; currently openai only")
	command.Flags().StringVar(&options.Model, "expect-model", "", "exact expected existing native model; does not select or change it")
}

func (options projectNativeViewOptions) validate(view string) error {
	if view != "native" {
		if options.Binary != "" || options.Provider != "" || options.Model != "" {
			return projectUsage(errors.New("native client flags require --view native"))
		}
		return nil
	}
	if !filepath.IsAbs(options.Binary) || options.Provider != "openai" || options.Model == "" {
		return projectUsage(errors.New("native view requires an absolute --codex-binary, --expect-provider openai, and --expect-model"))
	}
	return nil
}

func (options projectNativeViewOptions) validateBinding(binding project.Binding) error {
	if binding.Harness != mail.HarnessCodex {
		return fmt.Errorf("%w: this harness has no verified concurrent native-client capability; select --view timeline explicitly", project.ErrConflict)
	}
	if binding.NativeSessionID == "" || binding.Socket == "" {
		return fmt.Errorf("%w: native view requires the exact bound Codex session and private app-server socket", project.ErrConflict)
	}
	return nil
}

var projectPrepareCodexView = func(ctx context.Context, worker mail.Worker) (mail.CodexRemoteView, error) {
	return (mail.CodexAdapter{}).PrepareRemoteView(ctx, worker)
}

var projectInspectCodexView = func(ctx context.Context, worker mail.Worker) (mail.CodexObservation, error) {
	return (mail.CodexAdapter{}).Inspect(ctx, worker)
}

var projectLaunchNative = func(ctx context.Context, adapter backend.Capabilities, request surface.LaunchRequest) (backend.Ref, error) {
	result, err := surface.NewService(adapter, surface.Policy{}, "").Launch(ctx, request)
	return result.Ref(), err
}

type projectJournaledAdapter struct {
	backend.Capabilities
	begin     func(context.Context, backend.StartSpec) (project.SurfaceOperation, error)
	operation project.SurfaceOperation
	result    backend.StartResult
	tag       backend.RecoveryTag
	beginErr  error
}

func (adapter *projectJournaledAdapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	adapter.tag = spec.Tag()
	adapter.operation, adapter.beginErr = adapter.begin(ctx, spec)
	if adapter.beginErr != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureTargetBusy, errors.New("project presentation intent could not be recorded")))
	}
	adapter.result = adapter.Capabilities.Start(ctx, spec)
	return adapter.result
}

func createProjectThreadNative(command *cobra.Command, store project.Store, record project.Project, thread project.Thread, binding project.Binding, presentation projectPresentation, options projectPresentationOptions, nativeOptions projectNativeViewOptions) error {
	for _, operation := range record.SurfaceOperations {
		if operation.Kind != project.SurfaceKindThread || operation.ThreadID != thread.ID {
			continue
		}
		if operation.Retirement != nil && (operation.Retirement.Phase == project.RetirementClosed || operation.Retirement.Phase == project.RetirementAlreadyGone) {
			continue
		}
		if operation.Phase != project.SurfacePhaseFailed {
			return projectError(fmt.Errorf("%w: resolve or explicitly retire the existing thread view before native preparation", project.ErrConflict))
		}
	}
	adapter, err := presentation.ThreadAdapter(record.Workspace.Ref)
	if err != nil {
		return projectError(err)
	}
	self, err := surface.SelfPath()
	if err != nil {
		return projectError(err)
	}
	if err := (surface.Policy{}).AcceptBinary(launch.ResolvedBinary{Path: nativeOptions.Binary, Source: launch.BinaryCodexConfig}, self); err != nil {
		return projectError(err)
	}
	worker := projectBindingWorker(thread, binding)
	observed, err := projectInspectCodexView(command.Context(), worker)
	if err != nil {
		return projectError(err)
	}
	if observed.NativeSessionID != binding.NativeSessionID || observed.CWD != binding.Worktree || observed.Provider != nativeOptions.Provider || observed.Model != nativeOptions.Model {
		return projectError(fmt.Errorf("%w: observed native identity, provider, or model differs from the explicit expectation", project.ErrConflict))
	}
	prepared, err := projectPrepareCodexView(command.Context(), worker)
	if err != nil {
		return projectError(err)
	}
	if prepared.NativeSessionID != binding.NativeSessionID || prepared.CWD != binding.Worktree || prepared.Provider != nativeOptions.Provider || prepared.Model != nativeOptions.Model {
		return projectError(fmt.Errorf("%w: prepared native identity, provider, or model differs from the explicit expectation", project.ErrConflict))
	}
	home, err := os.MkdirTemp("", "fc-codex-view-")
	if err != nil {
		return projectError(err)
	}
	keepHome := false
	defer func() {
		if !keepHome {
			_ = os.RemoveAll(home)
		}
	}()
	codexHome := filepath.Join(home, "codex")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		return projectError(err)
	}
	built, err := launch.BuildCodexRemoteInvocation(launch.CodexRemoteRequest{
		BinaryPath: nativeOptions.Binary, Home: home, CodexHome: codexHome, Socket: binding.Socket,
		NativeSessionID: prepared.NativeSessionID, Model: prepared.Model, Provider: prepared.Provider,
		CWD: prepared.CWD, RuntimeWorkspaceRoots: prepared.RuntimeWorkspaceRoots,
		ApprovalPolicy: prepared.ApprovalPolicy, ApprovalsReviewer: prepared.ApprovalsReviewer,
		Sandbox: launch.CodexRemoteSandbox{Type: prepared.Sandbox.Type, NetworkAccess: prepared.Sandbox.NetworkAccess, WritableRoots: prepared.Sandbox.WritableRoots, ExcludeTmpdirEnvVar: prepared.Sandbox.ExcludeTmpdirEnvVar, ExcludeSlashTmp: prepared.Sandbox.ExcludeSlashTmp},
	})
	if err != nil {
		return projectError(err)
	}
	journaled := &projectJournaledAdapter{Capabilities: adapter}
	journaled.begin = func(ctx context.Context, spec backend.StartSpec) (project.SurfaceOperation, error) {
		_, operation, beginErr := store.BeginThread(ctx, record.ID, thread.ID, binding.RunID, options.Session, project.SurfacePresentationCodexRemote, spec.Tag(), record.Workspace.Ref)
		return operation, beginErr
	}
	request := surface.NewLaunchRequest("thread-"+thread.ID, built.Invocation)
	request.Self = self
	ref, launchError := projectLaunchNative(command.Context(), journaled, request)
	if journaled.operation.ID == "" {
		if journaled.beginErr != nil {
			return projectError(journaled.beginErr)
		}
		if launchError != nil {
			return projectError(launchError)
		}
		return projectError(errors.New("native launch returned without durable presentation intent"))
	}
	finish := projectNativeFinish(journaled, ref, launchError)
	finishContext, cancel := context.WithTimeout(context.WithoutCancel(command.Context()), 5*time.Second)
	defer cancel()
	registry, completed, err := store.FinishSurface(finishContext, record.ID, journaled.operation.ID, finish)
	keepHome = launchError == nil || completed.Phase == project.SurfacePhaseUnknown || err != nil
	if err != nil {
		return projectError(fmt.Errorf("native operation %s remains unresolved: %w", journaled.operation.ID, err))
	}
	report := projectPresentationReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, ThreadID: thread.ID, RunID: binding.RunID, Action: "start-native-client", Operation: &completed, Workspace: record.Workspace, ProcessLiveness: project.ProcessLivenessUnknown}
	var focusError error
	if launchError == nil && completed.Phase == project.SurfacePhaseVerified {
		_, freshProject, observations, inspectError := store.Inspect(command.Context(), record.ID)
		focusError = inspectError
		if focusError == nil {
			_, current, currentError := projectCurrentThread(freshProject, observations, thread.ID)
			focusError = currentError
			if focusError == nil && current != binding {
				focusError = fmt.Errorf("%w: binding changed after native client launch", project.ErrConflict)
			}
		}
		if focusError == nil {
			focusError = presentation.FocusThread(command.Context(), record.Workspace.Ref, ref)
		}
		report.Focused = focusError == nil
		report.Warnings = append(report.Warnings, "native client execution started; attachment and continued native policy are not established")
		if focusError == nil {
			if warning := projectThreadMetadataWarning(presentation, command.Context(), record.Workspace.Ref, ref, thread.Name); warning != "" {
				report.Warnings = append(report.Warnings, warning)
			}
		}
	}
	if err := writeProjectPresentation(command.OutOrStdout(), report, options.JSON); err != nil {
		return err
	}
	if launchError != nil {
		return projectError(launchError)
	}
	if completed.Phase != project.SurfacePhaseVerified {
		return projectError(fmt.Errorf("native operation %s ended %s; inspect before retrying", completed.ID, completed.Phase))
	}
	if focusError != nil {
		return projectError(fmt.Errorf("native client started but focus failed: %w", focusError))
	}
	return nil
}

func projectNativeFinish(adapter *projectJournaledAdapter, ref backend.Ref, launchError error) project.SurfaceFinish {
	if launchError == nil {
		return project.SurfaceVerified(ref)
	}
	var failure *surface.LaunchError
	if errors.As(launchError, &failure) {
		if failure.Outcome == backend.NotMutated {
			return project.SurfaceFailed(project.SurfaceErrorLaunch)
		}
		if failure.Cleanup == backend.CleanupClosed || failure.Cleanup == backend.CleanupAlreadyGone {
			if created, known := adapter.result.Ref(); known {
				return project.SurfaceCleaned(created, project.SurfaceErrorLaunch)
			}
		}
	}
	if created, known := adapter.result.Ref(); known {
		return project.SurfaceUnknown(adapter.tag, project.SurfaceErrorLaunch, &created)
	}
	return project.SurfaceUnknown(adapter.tag, project.SurfaceErrorLaunch, nil)
}
