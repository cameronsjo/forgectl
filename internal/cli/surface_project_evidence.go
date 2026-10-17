package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type projectNativeObservation struct {
	Source          project.EvidenceSource `json:"source"`
	NativeSessionID string                 `json:"native_session_id"`
	Provider        string                 `json:"provider"`
	Model           string                 `json:"model"`
	ActiveTools     []string               `json:"active_tools,omitempty"`
	SessionPath     string                 `json:"session_path,omitempty"`
	Turn            *projectNativeTurn     `json:"turn,omitempty"`
}

type projectNativeTurn struct {
	ID         string                 `json:"id"`
	Status     project.EvidenceStatus `json:"status"`
	ObservedAt time.Time              `json:"observed_at"`
}

type projectEvidenceReport struct {
	Version         int                       `json:"version"`
	Revision        uint64                    `json:"revision"`
	ProjectID       string                    `json:"project_id"`
	ThreadID        string                    `json:"thread_id"`
	RunID           string                    `json:"run_id"`
	Action          string                    `json:"action"`
	Native          *projectNativeObservation `json:"native,omitempty"`
	Evidence        *project.Evidence         `json:"evidence,omitempty"`
	ProcessLiveness project.ProcessLiveness   `json:"process_liveness"`
}

var projectObserveNative = observeProjectNative

func observeProjectNative(ctx context.Context, worker mail.Worker) (projectNativeObservation, error) {
	observation := projectNativeObservation{NativeSessionID: worker.ThreadID}
	var turnID, status string
	var observedAt time.Time
	switch worker.Harness {
	case mail.HarnessCodex:
		native, err := (mail.CodexAdapter{}).Inspect(ctx, worker)
		if err != nil {
			return projectNativeObservation{}, err
		}
		observation.Source = project.EvidenceSourceCodexThreadRead
		observation.NativeSessionID, observation.Provider, observation.Model = native.NativeSessionID, native.Provider, native.Model
		if native.Turn != nil {
			turnID, status, observedAt = native.Turn.ID, native.Turn.Status, native.Turn.ObservedAt
		}
	case mail.HarnessPi:
		native, err := (mail.PiAdapter{}).Inspect(ctx, worker)
		if err != nil {
			return projectNativeObservation{}, err
		}
		if !native.Known {
			return projectNativeObservation{}, errors.New("pi could not establish current native metadata")
		}
		observation.Source = project.EvidenceSourcePiInbox
		observation.NativeSessionID, observation.Provider, observation.Model = native.SessionID, native.Provider, native.ModelID
		observation.ActiveTools, observation.SessionPath = native.ActiveTools, native.SessionPath
		if native.Turn != nil {
			turnID, status, observedAt = native.Turn.ID, native.Turn.Status, native.Turn.ObservedAt
		}
	case mail.HarnessHermes:
		native, err := (mail.HermesAdapter{}).Inspect(ctx, worker)
		if err != nil {
			return projectNativeObservation{}, err
		}
		observation.Source = project.EvidenceSourceHermesSupervisor
		observation.Provider, observation.Model = native.Provider, native.Model
		if native.Turn != nil {
			turnID, status, observedAt = native.Turn.MessageID, native.Turn.Status, native.Turn.ObservedAt
		}
	default:
		return projectNativeObservation{}, fmt.Errorf("%w: native observation is unsupported for harness %s", project.ErrConflict, worker.Harness)
	}
	if observation.NativeSessionID != worker.ThreadID {
		return projectNativeObservation{}, fmt.Errorf("%w: native observation changed session identity", project.ErrConflict)
	}
	if turnID != "" {
		if observedAt.IsZero() {
			observedAt = time.Now().UTC()
		}
		observation.Turn = &projectNativeTurn{ID: turnID, Status: project.EvidenceStatus(status), ObservedAt: observedAt}
	}
	return observation, nil
}

func newSurfaceProjectThreadObserveCmd(root, projectID *string) *cobra.Command {
	var runID string
	var asJSON bool
	command := &cobra.Command{
		Use:   "observe <thread-id>",
		Short: "Read native metadata and record exact settled-turn evidence without prompting",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if *projectID == "" || runID == "" {
				return projectUsage(errors.New("--project and --run-id are required"))
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
			if binding.RunID != runID || binding.NativeSessionID == "" {
				return projectError(fmt.Errorf("%w: the exact current run and native session must be bound before observation", project.ErrConflict))
			}
			native, err := projectObserveNative(command.Context(), projectBindingWorker(thread, binding))
			if err != nil {
				return projectError(err)
			}
			if native.NativeSessionID != binding.NativeSessionID {
				return projectError(fmt.Errorf("%w: native observation does not match the bound session", project.ErrConflict))
			}
			report := projectEvidenceReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, ThreadID: thread.ID, RunID: binding.RunID, Action: "observed", Native: &native, ProcessLiveness: project.ProcessLivenessUnknown}
			if native.Turn != nil {
				input := project.NativeEvidenceInput{NativeSessionID: native.NativeSessionID, Source: native.Source, TurnID: native.Turn.ID, Status: native.Turn.Status, ObservedAt: native.Turn.ObservedAt}
				updated, evidence, err := store.RecordNativeEvidence(command.Context(), record.ID, thread.ID, binding.RunID, input)
				if err != nil {
					return projectError(err)
				}
				report.Revision, report.Evidence = updated.Revision, &evidence
			} else {
				fresh, freshProject, freshObservations, err := store.Inspect(command.Context(), record.ID)
				if err != nil {
					return projectError(err)
				}
				_, current, err := projectCurrentThread(freshProject, freshObservations, thread.ID)
				if err != nil {
					return projectError(err)
				}
				if current != binding {
					return projectError(fmt.Errorf("%w: binding changed during native observation", project.ErrConflict))
				}
				report.Revision = fresh.Revision
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), report)
			}
			status := "no settled turn observed"
			if report.Evidence != nil {
				status = "native turn " + termsafe.SafeLineMax(report.Evidence.TurnID, 128) + "=" + string(report.Evidence.Status)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "thread %s run %s: %s; selected provider=%s model=%s; artifact correctness and process liveness not established\n", thread.ID, termsafe.SafeLineMax(binding.RunID, 128), status, termsafe.SafeLineMax(native.Provider, 256), termsafe.SafeLineMax(native.Model, 256))
			return err
		},
	}
	command.Flags().StringVar(&runID, "run-id", "", "exact current bound run ID; stale observations are refused")
	command.Flags().BoolVar(&asJSON, "json", false, "print native metadata and any persisted settled-turn evidence")
	return withProjectRoot(root, command)
}

func projectBindingWorker(thread project.Thread, binding project.Binding) mail.Worker {
	return mail.Worker{Name: binding.Member, Harness: binding.Harness, Worktree: binding.Worktree, Socket: binding.Socket, ThreadID: binding.NativeSessionID, RunID: binding.RunID, PaneID: binding.PaneID, Coordinator: thread.Role == project.RoleCoordinator}
}

func newSurfaceProjectThreadVerifyArtifactCmd(root, projectID *string) *cobra.Command {
	var runID, path, digest string
	var asJSON bool
	command := &cobra.Command{
		Use:   "verify-artifact <thread-id>",
		Short: "Record a matching file digest, not correctness or authorship",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if *projectID == "" || runID == "" || path == "" || digest == "" {
				return projectUsage(errors.New("--project, --run-id, --path, and --sha256 are required"))
			}
			registry, evidence, err := (project.Store{Root: *root}).RecordVerifiedArtifact(command.Context(), *projectID, arguments[0], runID, path, digest)
			if err != nil {
				return projectError(err)
			}
			report := projectEvidenceReport{Version: registry.Version, Revision: registry.Revision, ProjectID: *projectID, ThreadID: arguments[0], RunID: runID, Action: "verified-artifact-bytes", Evidence: &evidence, ProcessLiveness: project.ProcessLivenessUnknown}
			if asJSON {
				return writeJSON(command.OutOrStdout(), report)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "thread %s run %s artifact %s matched SHA-256 %s; correctness, authorship, and continued freshness not evaluated\n", evidence.ThreadID, termsafe.SafeLineMax(evidence.RunID, 128), termsafe.QuotePathMax(evidence.Artifact.RelativePath, 4096), evidence.Artifact.SHA256)
			return err
		},
	}
	command.Flags().StringVar(&runID, "run-id", "", "exact current bound run ID")
	command.Flags().StringVar(&path, "path", "", "regular artifact beneath this run's worktree; no traversal or links")
	command.Flags().StringVar(&digest, "sha256", "", "expected lowercase SHA-256 digest; mismatch records nothing")
	command.Flags().BoolVar(&asJSON, "json", false, "print persisted byte-identity evidence, not a correctness assertion")
	return withProjectRoot(root, command)
}
