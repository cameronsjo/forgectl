package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type projectReport struct {
	Version      int                   `json:"version"`
	Revision     uint64                `json:"revision"`
	Project      project.Project       `json:"project"`
	Ledger       string                `json:"ledger"`
	Observations []project.Observation `json:"observations,omitempty"`
}

type projectThreadReport struct {
	Version     int                  `json:"version"`
	Revision    uint64               `json:"revision"`
	ProjectID   string               `json:"project_id"`
	Thread      project.Thread       `json:"thread"`
	Observation *project.Observation `json:"observation,omitempty"`
}

func newSurfaceProjectCmd(deps module.Deps) *cobra.Command {
	var root string
	command := &cobra.Command{
		Use:          "project",
		Short:        "Keep persistent collaboration projects and threads without launching agents",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE:         func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	command.PersistentFlags().StringVar(&root, "root", "", "absolute private registry root (required; no global default)")
	command.AddCommand(newSurfaceProjectCreateCmd(&root))
	command.AddCommand(newSurfaceProjectListCmd(&root))
	command.AddCommand(newSurfaceProjectShowCmd(&root))
	command.AddCommand(newSurfaceProjectThreadCmd(deps, &root))
	command.AddCommand(newSurfaceProjectTimelineCmd(deps, &root))
	command.AddCommand(newSurfaceProjectOpenCmd(deps, &root))
	return command
}

func projectError(err error) error {
	if errors.Is(err, project.ErrInvalidInput) || errors.Is(err, project.ErrNotFound) {
		return projectUsage(err)
	}
	return WithExitCode(termsafe.Error(err), classExit(classFailed))
}

func projectUsage(err error) error {
	return withJSONUsageCode(WithExitCode(termsafe.Error(err), classExit(classUsage)))
}

func withProjectRoot(root *string, command *cobra.Command) *cobra.Command {
	previous := command.Args
	command.Args = func(command *cobra.Command, arguments []string) error {
		if previous != nil {
			if err := previous(command, arguments); err != nil {
				return err
			}
		}
		if !filepath.IsAbs(*root) || filepath.Clean(*root) == string(filepath.Separator) {
			return projectUsage(errors.New("--root must name an absolute private registry directory, not the filesystem root"))
		}
		return nil
	}
	return command
}

func projectContextFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(project.MaxContextBytes) {
		return "", errors.New("context input must be a regular file of at most 16 KiB")
	}
	file, err := openRegularNoFollow(absolute)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", errors.New("context input changed while it was being opened")
	}
	contents, err := io.ReadAll(io.LimitReader(file, int64(project.MaxContextBytes)+1))
	if err != nil {
		return "", err
	}
	if len(contents) > project.MaxContextBytes {
		return "", errors.New("context input exceeds 16 KiB")
	}
	return string(contents), nil
}

func newSurfaceProjectCreateCmd(root *string) *cobra.Command {
	var worktree, instructionsPath string
	var asJSON bool
	command := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project and its unbound coordinator thread; launch nothing",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			instructions, err := projectContextFile(instructionsPath)
			if err != nil {
				return projectUsage(err)
			}
			registry, created, err := (project.Store{Root: *root}).CreateProject(command.Context(), arguments[0], worktree, instructions)
			if err != nil {
				return projectError(err)
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), projectReport{Version: registry.Version, Revision: registry.Revision, Project: created, Ledger: projectLedger(*root, created.ID)})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "created project %s (%s), coordinator thread %s; no agent launched\n", created.ID, safeLabel(created.Name), created.CoordinatorThreadID)
			return err
		},
	}
	command.Flags().StringVar(&worktree, "worktree", "", "existing absolute working directory; creates no Git worktree")
	command.Flags().StringVar(&instructionsPath, "instructions", "", "snapshot project instructions from a regular UTF-8 file, at most 16 KiB")
	command.Flags().BoolVar(&asJSON, "json", false, "print version, revision, project, and its future private team ledger path")
	return withProjectRoot(root, command)
}

func newSurfaceProjectListCmd(root *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List persistent projects without creating or repairing state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			registry, err := (project.Store{Root: *root}).Snapshot(command.Context())
			if err != nil {
				return projectError(err)
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), registry)
			}
			for _, record := range registry.Projects {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s %s threads=%d\n", record.ID, safeLabel(record.Name), len(record.Threads)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print version, revision, and projects; an absent root is an empty list")
	return withProjectRoot(root, command)
}

func projectLedger(root, projectID string) string {
	return filepath.Join(filepath.Clean(root), projectID, "team")
}

func newSurfaceProjectShowCmd(root *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "show <project-id>",
		Short: "Show a project by exact ID, without inferring process liveness",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			registry, record, observations, err := (project.Store{Root: *root}).Inspect(command.Context(), arguments[0])
			if err != nil {
				return projectError(err)
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), projectReport{Version: registry.Version, Revision: registry.Revision, Project: record, Ledger: projectLedger(*root, record.ID), Observations: observations})
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s %s\n", record.ID, safeLabel(record.Name)); err != nil {
				return err
			}
			for _, thread := range record.Threads {
				observation, err := projectObservation(observations, thread.ID)
				if err != nil {
					return projectError(err)
				}
				if err := printProjectThread(command.OutOrStdout(), thread, observation); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print version, revision, project, and its private team ledger path")
	return withProjectRoot(root, command)
}

func newSurfaceProjectThreadCmd(deps module.Deps, root *string) *cobra.Command {
	var projectID string
	command := &cobra.Command{
		Use:   "thread",
		Short: "Create and inspect logical workstreams, not native sessions",
		Args:  cobra.NoArgs,
		RunE:  func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	command.PersistentFlags().StringVar(&projectID, "project", "", "exact project ID (required)")
	command.AddCommand(newSurfaceProjectThreadCreateCmd(root, &projectID))
	command.AddCommand(newSurfaceProjectThreadReadCmd(root, &projectID, false))
	command.AddCommand(newSurfaceProjectThreadReadCmd(root, &projectID, true))
	command.AddCommand(newSurfaceProjectThreadBindCmd(root, &projectID))
	command.AddCommand(newSurfaceProjectThreadContextCmd(root, &projectID))
	command.AddCommand(newSurfaceProjectThreadFocusCmd(deps, root, &projectID))
	return command
}

func newSurfaceProjectThreadCreateCmd(root, projectID *string) *cobra.Command {
	var briefPath string
	var asJSON bool
	command := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an unbound worker thread without enrolling or launching a harness",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if *projectID == "" {
				return projectUsage(errors.New("--project must name an exact project ID"))
			}
			brief, err := projectContextFile(briefPath)
			if err != nil {
				return projectUsage(err)
			}
			registry, created, err := (project.Store{Root: *root}).CreateThread(command.Context(), *projectID, arguments[0], brief)
			if err != nil {
				return projectError(err)
			}
			if asJSON {
				return writeJSON(command.OutOrStdout(), projectThreadReport{Version: registry.Version, Revision: registry.Revision, ProjectID: *projectID, Thread: created})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "created thread %s (%s), project %s; no agent launched\n", created.ID, safeLabel(created.Name), *projectID)
			return err
		},
	}
	command.Flags().StringVar(&briefPath, "brief", "", "snapshot the thread brief from a regular UTF-8 file, at most 16 KiB")
	command.Flags().BoolVar(&asJSON, "json", false, "print version, revision, project ID, and the new thread")
	return withProjectRoot(root, command)
}

func newSurfaceProjectThreadReadCmd(root, projectID *string, one bool) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "list", Short: "List this project's logical threads without probing native sessions", Args: cobra.NoArgs}
	if one {
		command.Use, command.Short, command.Args = "show <thread-id>", "Show one thread by exact project and thread IDs", cobra.ExactArgs(1)
	}
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if *projectID == "" {
			return projectUsage(errors.New("--project must name an exact project ID"))
		}
		registry, record, observations, err := (project.Store{Root: *root}).Inspect(command.Context(), *projectID)
		if err != nil {
			return projectError(err)
		}
		threads := record.Threads
		if one {
			threads = nil
			for _, thread := range record.Threads {
				if thread.ID == arguments[0] {
					threads = []project.Thread{thread}
					break
				}
			}
			if len(threads) == 0 {
				return projectError(project.ErrNotFound)
			}
		}
		if asJSON {
			if one {
				observation, err := projectObservation(observations, threads[0].ID)
				if err != nil {
					return projectError(err)
				}
				return writeJSON(command.OutOrStdout(), projectThreadReport{Version: registry.Version, Revision: registry.Revision, ProjectID: record.ID, Thread: threads[0], Observation: &observation})
			}
			return writeJSON(command.OutOrStdout(), struct {
				Version      int                   `json:"version"`
				Revision     uint64                `json:"revision"`
				ProjectID    string                `json:"project_id"`
				Threads      []project.Thread      `json:"threads"`
				Observations []project.Observation `json:"observations"`
			}{registry.Version, registry.Revision, record.ID, threads, observations})
		}
		for _, thread := range threads {
			observation, err := projectObservation(observations, thread.ID)
			if err != nil {
				return projectError(err)
			}
			if err := printProjectThread(command.OutOrStdout(), thread, observation); err != nil {
				return err
			}
		}
		return nil
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print logical thread state and separate enrollment observations")
	return withProjectRoot(root, command)
}

func projectObservation(observations []project.Observation, threadID string) (project.Observation, error) {
	for _, observation := range observations {
		if observation.ThreadID == threadID {
			return observation, nil
		}
	}
	return project.Observation{}, fmt.Errorf("%w: thread observation is missing", project.ErrInvalidState)
}

func printProjectThread(writer io.Writer, thread project.Thread, observation project.Observation) error {
	_, err := fmt.Fprintf(writer, "%s %s %s enrollment=%s native_session_known=%t process=%s\n", thread.ID, thread.Role, safeLabel(thread.Name), observation.Enrollment, observation.NativeSessionKnown, observation.ProcessLiveness)
	return err
}
