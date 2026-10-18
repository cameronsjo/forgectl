//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/surface/projectview"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tui"
)

type projectAgentsOptions struct {
	JSON         bool
	Live         bool
	ASCII        bool
	HerdrSession string
	HerdrBinary  string
}

func (options *projectAgentsOptions) flags(command *cobra.Command) {
	command.Flags().BoolVar(&options.JSON, "json", false, "print one bounded Agents snapshot as JSON")
	command.Flags().BoolVar(&options.Live, "live", false, "open the terminal-native Agents overview")
	command.Flags().BoolVar(&options.ASCII, "no-icons", false, "use the ASCII legend")
	command.Flags().StringVar(&options.HerdrSession, "herdr-session", "", "explicit named herdr session for live focus and evidence actions")
	command.Flags().StringVar(&options.HerdrBinary, "herdr-binary", "", "absolute operator-selected herdr executable for live focus and evidence actions")
}

func (options projectAgentsOptions) validate() error {
	if !options.Live {
		return nil
	}
	if options.HerdrSession == "" && options.HerdrBinary == "" {
		return nil
	}
	presentationOptions := projectPresentationOptions{Session: options.HerdrSession, Binary: options.HerdrBinary}
	return presentationOptions.validate()
}

func newSurfaceProjectAgentsCmd(deps module.Deps, root *string) *cobra.Command {
	var options projectAgentsOptions
	command := &cobra.Command{
		Use:   "agents <project-id>",
		Short: "Show bounded per-thread native activity without launching or resuming agents",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if options.Live && options.JSON {
				return projectUsage(errors.New("--live and --json cannot be combined"))
			}
			if options.Live && !deskHasTerminal() {
				return projectUsage(tui.ErrRunWatchNeedsTerminal)
			}
			if options.Live && termIsDumb() {
				return projectUsage(tui.DumbTerminalError("surface project agents --live", "surface project agents without --live"))
			}
			if err := options.validate(); err != nil {
				return err
			}
			store := project.Store{Root: *root}
			source := projectview.NewAgentsSource(store, arguments[0], projectAgentsObserver)
			if !options.Live {
				snapshot, err := source.Load(command.Context())
				if err != nil {
					return projectError(err)
				}
				if options.JSON {
					if err := writeJSON(command.OutOrStdout(), snapshot); err != nil {
						return err
					}
					return projectAgentsSnapshotStatus(snapshot)
				}
				if err := writeProjectAgentsText(command.OutOrStdout(), snapshot); err != nil {
					return err
				}
				return projectAgentsSnapshotStatus(snapshot)
			}
			var presentation projectPresentation
			if options.HerdrSession != "" || options.HerdrBinary != "" {
				var err error
				presentation, err = projectPresentationFor(deps, options.HerdrBinary, options.HerdrSession, command.ErrOrStderr())
				if err != nil {
					return projectError(err)
				}
			}
			presentationOptions := projectPresentationOptions{Session: options.HerdrSession, Binary: options.HerdrBinary}
			return tui.RunProjectAgents(command.Context(), tui.ProjectAgentsOptions{
				Theme: deps.Theme,
				Load: func(ctx context.Context) (projectview.AgentsSnapshot, error) {
					return source.Load(ctx)
				},
				Focus:        projectAgentsFocusCallback(store, presentation, arguments[0], presentationOptions),
				OpenEvidence: projectAgentsEvidenceCallback(store, presentation, arguments[0], presentationOptions),
				ASCII:        options.ASCII,
			})
		},
	}
	options.flags(command)
	return withProjectRoot(root, command)
}

func projectAgentsObserver(ctx context.Context, worker mail.Worker) (projectview.NativeActivity, error) {
	native, err := projectObserveNative(ctx, worker)
	if err != nil {
		return projectview.NativeActivity{}, err
	}
	return projectview.NativeActivity{NativeSessionID: native.NativeSessionID, Provider: native.Provider, Model: native.Model, Activity: native.Activity}, nil
}

func projectAgentsFocusCallback(store project.Store, presentation projectPresentation, projectID string, options projectPresentationOptions) func(context.Context, string, string) error {
	if presentation == nil {
		return nil
	}
	return func(ctx context.Context, threadID, runID string) error {
		return focusProjectAgent(ctx, store, presentation, projectID, threadID, runID, options)
	}
}

func projectAgentsEvidenceCallback(store project.Store, presentation projectPresentation, projectID string, options projectPresentationOptions) func(context.Context, string, string) error {
	if presentation == nil {
		return nil
	}
	return func(ctx context.Context, threadID, runID string) error {
		return openProjectAgentEvidence(ctx, store, presentation, projectID, threadID, runID, options)
	}
}

func focusProjectAgent(ctx context.Context, store project.Store, presentation projectPresentation, projectID, threadID, runID string, options projectPresentationOptions) error {
	if presentation == nil {
		return errors.New("live Agents focus requires --herdr-session and --herdr-binary")
	}
	_, _, _, _, operation, err := inspectCurrentProjectThreadView(ctx, store, projectID, threadID, runID, options.Session, "", "")
	if err != nil {
		return projectError(err)
	}
	if operation.ID == "" || operation.Parent == nil || operation.Ref == nil {
		return projectError(fmt.Errorf("%w: selected run has no verified thread view; no native resume is attempted", project.ErrConflict))
	}
	if err := presentation.FocusThread(ctx, *operation.Parent, *operation.Ref); err != nil {
		return projectError(err)
	}
	return nil
}

func openProjectAgentEvidence(ctx context.Context, store project.Store, presentation projectPresentation, projectID, threadID, runID string, options projectPresentationOptions) error {
	if presentation == nil {
		return errors.New("live Agents evidence requires --herdr-session and --herdr-binary")
	}
	_, record, thread, binding, operation, err := inspectCurrentProjectThreadView(ctx, store, projectID, threadID, runID, options.Session, project.SurfacePresentationTimeline, "")
	if err != nil {
		return projectError(err)
	}
	if operation.ID != "" {
		if operation.Parent == nil || operation.Ref == nil {
			return projectError(fmt.Errorf("%w: selected timeline reference is invalid", project.ErrConflict))
		}
		if err := presentation.FocusThread(ctx, *operation.Parent, *operation.Ref); err != nil {
			return projectError(err)
		}
		return nil
	}
	if thread.ID == "" || binding.RunID == "" || record.Workspace == nil {
		return projectError(fmt.Errorf("%w: selected run changed before evidence view creation", project.ErrConflict))
	}
	command := &cobra.Command{}
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := createProjectThreadTimeline(command, store, record, thread, binding, presentation, options); err != nil {
		return err
	}
	return nil
}

func writeProjectAgentsText(output io.Writer, snapshot projectview.AgentsSnapshot) error {
	partial := ""
	if snapshot.Partial {
		partial = "; partial=true"
	}
	if _, err := fmt.Fprintf(output, "project %s %s: observed %s; threads=%d shown=%d dropped_threads=%d dropped_spans=%d%s", termsafe.SafeLineMax(snapshot.ProjectID, 128), termsafe.SafeLineMax(snapshot.ProjectName, 128), snapshot.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"), snapshot.TotalThreads, len(snapshot.Rows), snapshot.DroppedThreads, snapshot.DroppedSpans, partial); err != nil {
		return err
	}
	if snapshot.Reason != "" {
		if _, err := fmt.Fprintf(output, "; reason=%s", termsafe.SafeLineMax(snapshot.Reason, 80)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(output); err != nil {
		return err
	}
	for _, row := range snapshot.Rows {
		metadataParts := []string{}
		for _, value := range []string{row.Harness, row.Provider, row.Model} {
			if value != "" {
				metadataParts = append(metadataParts, value)
			}
		}
		metadata := strings.Join(metadataParts, " / ")
		if _, err := fmt.Fprintf(output, "- %s [%s] role=%s state=%s run=%s coverage=%s", termsafe.SafeLineMax(row.Name, 128), termsafe.SafeLineMax(row.ThreadID, 128), termsafe.SafeLineMax(row.Role, 64), termsafe.SafeLineMax(row.State, 64), termsafe.SafeLineMax(row.RunID, 128), termsafe.SafeLineMax(string(row.Coverage), 64)); err != nil {
			return err
		}
		if metadata != "" {
			if _, err := fmt.Fprintf(output, " (%s)", termsafe.SafeLineMax(metadata, 256)); err != nil {
				return err
			}
		}
		if row.Reason != "" {
			if _, err := fmt.Fprintf(output, " reason=%s", termsafe.SafeLineMax(row.Reason, 128)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(output); err != nil {
			return err
		}
		for _, span := range row.Spans {
			end := "unknown"
			if span.Span.EndedAt != nil {
				end = span.Span.EndedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			if _, err := fmt.Fprintf(output, "  span %s source=%s state=%s start=%s end=%s\n", termsafe.SafeLineMax(span.Span.ID, 128), termsafe.SafeLineMax(string(span.Source), 64), termsafe.SafeLineMax(string(span.Span.State), 64), span.Span.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), end); err != nil {
				return err
			}
		}
		for _, point := range row.Points {
			if _, err := fmt.Fprintf(output, "  point turn=%s source=%s state=%s at=%s\n", termsafe.SafeLineMax(point.TurnID, 128), termsafe.SafeLineMax(string(point.Source), 64), termsafe.SafeLineMax(string(point.State), 64), point.At.UTC().Format("2006-01-02T15:04:05Z")); err != nil {
				return err
			}
		}
	}
	return nil
}

func projectAgentsSnapshotStatus(snapshot projectview.AgentsSnapshot) error {
	if !snapshot.Partial && snapshot.DroppedThreads == 0 && snapshot.DroppedSpans == 0 {
		return nil
	}
	reason := termsafe.SafeLineMax(snapshot.Reason, 128)
	if reason == "" {
		reason = "bounded or incomplete source"
	}
	return projectError(fmt.Errorf("%w: Agents snapshot is partial (%s)", project.ErrInvalidState, reason))
}
