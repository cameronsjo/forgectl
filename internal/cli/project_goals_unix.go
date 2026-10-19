//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/goal"
	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
	"github.com/cameronsjo/forgectl/internal/surface/goaltools"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tui"
)

const projectGoalInputLimit = 32 * 1024

type projectGoalConfig struct {
	Model string `toml:"model"`
}

type projectGoalJSON struct {
	Version int         `json:"version"`
	Kind    string      `json:"kind"`
	GoalID  string      `json:"goal_id,omitempty"`
	State   *goal.State `json:"state,omitempty"`
	Content string      `json:"content,omitempty"`
}

type projectGoalPreparedJSON struct {
	Version  int           `json:"version"`
	Prepared goal.Prepared `json:"prepared"`
}

func newProjectGoalsCmd(deps module.Deps, root *string) *cobra.Command {
	var modelOverride string
	command := &cobra.Command{
		Use:          "goals",
		Short:        "Prepare and supervise an isolated native-team project goal",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}
	goalRoot := projectGoalRootFlag(command, root)
	command.Flags().StringVar(&modelOverride, "model", "", "exact OpenAI Codex model ID; otherwise read only model from Codex config")
	command.RunE = func(command *cobra.Command, _ []string) error {
		if !projectGoalTerminal(command) {
			return projectGoalUsage(tui.ErrProjectGoalsNeedsTerminal)
		}
		model := strings.TrimSpace(modelOverride)
		if model == "" {
			defaultModel, err := projectGoalDefaultModel()
			if err != nil {
				return projectGoalUsage(err)
			}
			model = defaultModel
		}
		repository, err := os.Getwd()
		if err != nil {
			return projectGoalFailure(err)
		}
		repository, err = filepath.Abs(repository)
		if err != nil {
			return projectGoalFailure(err)
		}
		currentStore, err := projectGoalStore(goalRoot)
		if err != nil {
			return projectGoalUsage(err)
		}
		store := func() (goal.Store, error) { return currentStore, nil }
		return tui.RunProjectGoals(command.Context(), tui.ProjectGoalsOptions{
			Theme:      deps.Theme,
			Input:      command.InOrStdin(),
			Output:     command.OutOrStdout(),
			Root:       currentStore.Root,
			Repository: repository,
			Model:      model,
			List: func(ctx context.Context) ([]goal.State, error) {
				current, err := store()
				if err != nil {
					return nil, err
				}
				return current.List(ctx)
			},
			Prepare: func(ctx context.Context, spec goal.Spec) (goal.Prepared, error) {
				return goal.Prepare(ctx, spec)
			},
			Start: func(ctx context.Context, prepared goal.Prepared) (goal.State, error) {
				current, err := store()
				if err != nil {
					return goal.State{}, err
				}
				manager, err := projectGoalManager(current, deps)
				if err != nil {
					return goal.State{}, err
				}
				return manager.Start(ctx, prepared)
			},
			Load: func(ctx context.Context, id string) (goal.State, error) {
				current, err := store()
				if err != nil {
					return goal.State{}, err
				}
				return current.Load(ctx, id)
			},
			Respond: func(ctx context.Context, id, questionID, answer string) error {
				current, err := store()
				if err != nil {
					return err
				}
				_, err = current.Request(ctx, id, "respond", questionID, answer)
				return err
			},
			Cancel: func(ctx context.Context, id string) error {
				current, err := store()
				if err != nil {
					return err
				}
				_, err = current.Request(ctx, id, "cancel", "", "")
				return err
			},
			ReadArtifact: func(ctx context.Context, id, kind string, index int) (string, error) {
				current, err := store()
				if err != nil {
					return "", err
				}
				return current.ReadArtifact(ctx, id, kind, index)
			},
		})
	}
	return command
}

func newProjectGoalCmd(deps module.Deps, root *string) *cobra.Command {
	command := &cobra.Command{
		Use:          "goal",
		Short:        "Prepare, start, and inspect isolated native-team goals",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE:         func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	goalRoot := projectGoalRootFlag(command, root)
	command.AddCommand(
		newProjectGoalPrepareCmd(goalRoot),
		newProjectGoalStartCmd(deps, goalRoot),
		newProjectGoalListCmd(goalRoot),
		newProjectGoalStatusCmd(goalRoot),
		newProjectGoalRespondCmd(goalRoot),
		newProjectGoalCancelCmd(goalRoot),
		newProjectGoalArtifactCmd(goalRoot),
		newProjectGoalSuperviseCmd(goalRoot),
		newProjectGoalHostCmd(),
		newProjectGoalToolsCmd(),
		newProjectGoalFileCmd(),
	)
	return command
}

func projectGoalRootFlag(command *cobra.Command, root *string) *string {
	if root != nil {
		return root
	}
	var ownRoot string
	command.PersistentFlags().StringVar(&ownRoot, "root", "", "absolute private goal registry root (default: platform state directory)")
	return &ownRoot
}

func projectGoalStore(root *string) (goal.Store, error) {
	value := ""
	if root != nil {
		value = strings.TrimSpace(*root)
	}
	if value == "" {
		var err error
		value, err = goal.DefaultRoot()
		if err != nil {
			return goal.Store{}, err
		}
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return goal.Store{}, errors.New("--root must name an absolute private goal registry directory, not the filesystem root")
	}
	return goal.Store{Root: value}, nil
}

func projectGoalManager(store goal.Store, deps module.Deps) (goal.Manager, error) {
	self, err := os.Executable()
	if err != nil {
		return goal.Manager{}, fmt.Errorf("locate this forgectl executable: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return goal.Manager{}, fmt.Errorf("resolve this forgectl executable: %w", err)
	}
	resolved, err := launch.ResolveBinary("codex", deps.Cfg.Launch.Defaults)
	if err != nil {
		return goal.Manager{}, err
	}
	return goal.Manager{Store: store, SelfBinary: self, CodexBinary: resolved.Path, NativeDefaults: deps.Cfg.Launch.Defaults}, nil
}

func projectGoalUsage(err error) error {
	return withJSONUsageCode(WithExitCode(termsafe.Error(err), classExit(classUsage)))
}

func projectGoalFailure(err error) error {
	return WithExitCode(termsafe.Error(err), classExit(classFailed))
}

func projectGoalRefused(err error) error {
	return WithExitCode(termsafe.Error(err), classExit(classRefused))
}

func projectGoalTerminal(command *cobra.Command) bool {
	input, inputOK := command.InOrStdin().(interface{ Fd() uintptr })
	output, outputOK := command.OutOrStdout().(interface{ Fd() uintptr })
	return inputOK && outputOK && term.IsTerminal(int(input.Fd())) && term.IsTerminal(int(output.Fd()))
}

func readProjectGoalJSON(path string, limit int64) ([]byte, error) {
	if path == "" {
		return nil, errors.New("--file is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("goal input must be a regular non-symlink file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("goal input exceeds %d bytes", limit)
	}
	file, err := openRegularNoFollow(absolute)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("goal input exceeds %d bytes", limit)
	}
	return contents, nil
}

func readProjectGoalSpec(path string) (goal.Spec, error) {
	contents, err := readProjectGoalJSON(path, projectGoalInputLimit)
	if err != nil {
		return goal.Spec{}, err
	}
	var spec goal.Spec
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return goal.Spec{}, fmt.Errorf("decode goal input: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return goal.Spec{}, errors.New("decode goal input: expected exactly one JSON value")
		}
		return goal.Spec{}, fmt.Errorf("decode goal input trailing data: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return goal.Spec{}, err
	}
	return spec, nil
}

func projectGoalOutput(command *cobra.Command, value any, asJSON bool, text string) error {
	if asJSON {
		return writeJSON(command.OutOrStdout(), value)
	}
	_, err := fmt.Fprintln(command.OutOrStdout(), safeText(text))
	return err
}

func newProjectGoalPrepareCmd(root *string) *cobra.Command {
	var file string
	var asJSON bool
	command := &cobra.Command{
		Use:   "prepare --file PATH",
		Short: "Validate a goal file against the current committed source HEAD without starting work",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			spec, err := readProjectGoalSpec(file)
			if err != nil {
				return projectGoalUsage(err)
			}
			prepared, err := goal.Prepare(command.Context(), spec)
			if err != nil {
				return projectGoalRefused(err)
			}
			return projectGoalOutput(command, projectGoalPreparedJSON{Version: goal.Version, Prepared: prepared}, asJSON, fmt.Sprintf("prepared source=%s branch=%s dirty=%t", prepared.Spec.BaseRevision, prepared.SourceBranch, prepared.SourceDirty))
		},
	}
	command.Flags().StringVar(&file, "file", "", "versioned JSON goal specification")
	command.Flags().BoolVar(&asJSON, "json", false, "print the versioned prepared goal JSON")
	return command
}

func newProjectGoalStartCmd(deps module.Deps, root *string) *cobra.Command {
	var file, expectHead string
	var approve, asJSON bool
	command := &cobra.Command{
		Use:   "start --file PATH --expect-head SHA --approve",
		Short: "Start an approved goal only after binding it to an exact source HEAD",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if file == "" || expectHead == "" || !approve {
				return projectGoalUsage(errors.New("start requires --file, --expect-head, and explicit --approve; no model or process was started"))
			}
			spec, err := readProjectGoalSpec(file)
			if err != nil {
				return projectGoalUsage(err)
			}
			spec.BaseRevision = strings.TrimSpace(expectHead)
			prepared, err := goal.Prepare(command.Context(), spec)
			if err != nil {
				return projectGoalRefused(err)
			}
			if prepared.Spec.BaseRevision != spec.BaseRevision {
				return projectGoalRefused(errors.New("source HEAD does not match --expect-head; no model or process was started"))
			}
			store, err := projectGoalStore(root)
			if err != nil {
				return projectGoalUsage(err)
			}
			manager, err := projectGoalManager(store, deps)
			if err != nil {
				return projectGoalRefused(err)
			}
			state, err := manager.Start(command.Context(), prepared)
			if err != nil {
				return projectGoalFailure(err)
			}
			return projectGoalOutput(command, state, asJSON, fmt.Sprintf("started %s phase=%s", state.ID, state.Phase))
		},
	}
	command.Flags().StringVar(&file, "file", "", "versioned JSON goal specification")
	command.Flags().StringVar(&expectHead, "expect-head", "", "exact committed source HEAD reviewed by the operator")
	command.Flags().BoolVar(&approve, "approve", false, "explicitly approve the prepared goal")
	command.Flags().BoolVar(&asJSON, "json", false, "print the versioned goal state JSON")
	return command
}

func newProjectGoalListCmd(root *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "list", Short: "List retained goals without starting or repairing them", Args: cobra.NoArgs}
	command.Flags().BoolVar(&asJSON, "json", false, "print versioned goal state JSON")
	command.RunE = func(command *cobra.Command, _ []string) error {
		store, err := projectGoalStore(root)
		if err != nil {
			return projectGoalUsage(err)
		}
		states, err := store.List(command.Context())
		if err != nil {
			return projectGoalFailure(err)
		}
		if asJSON {
			return writeJSON(command.OutOrStdout(), struct {
				Version int          `json:"version"`
				States  []goal.State `json:"states"`
			}{goal.Version, states})
		}
		for _, state := range states {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s %s %s\n", safeLabel(state.ID), safeLabel(string(state.Phase)), safeLabel(state.Spec.Title)); err != nil {
				return err
			}
		}
		return nil
	}
	return command
}

func newProjectGoalStatusCmd(root *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "status <goal-id>", Short: "Read one retained goal state without replaying it", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&asJSON, "json", false, "print versioned goal state JSON")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		store, err := projectGoalStore(root)
		if err != nil {
			return projectGoalUsage(err)
		}
		state, err := store.Load(command.Context(), arguments[0])
		if err != nil {
			return projectGoalFailure(err)
		}
		return projectGoalOutput(command, state, asJSON, fmt.Sprintf("%s phase=%s supervisor=%t cleanup=%t", state.ID, state.Phase, state.SupervisorActive, state.CleanupComplete))
	}
	return command
}

func newProjectGoalRespondCmd(root *string) *cobra.Command {
	var questionID, answerFile string
	var asJSON bool
	command := &cobra.Command{Use: "respond <goal-id> --question ID --answer-file PATH", Short: "Send one exact answer to the current retained operator question", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&questionID, "question", "", "exact unanswered question ID")
	command.Flags().StringVar(&answerFile, "answer-file", "", "bounded regular file, or - to read the answer from stdin")
	command.Flags().BoolVar(&asJSON, "json", false, "print the versioned control receipt JSON")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if questionID == "" || answerFile == "" {
			return projectGoalUsage(errors.New("respond requires --question and --answer-file; use --answer-file - for stdin"))
		}
		answer, err := readProjectGoalAnswer(command, answerFile)
		if err != nil {
			return projectGoalUsage(err)
		}
		store, err := projectGoalStore(root)
		if err != nil {
			return projectGoalUsage(err)
		}
		control, err := store.Request(command.Context(), arguments[0], "respond", questionID, answer)
		if err != nil {
			return projectGoalRefused(err)
		}
		return projectGoalOutput(command, control, asJSON, fmt.Sprintf("queued response %s for %s", control.QuestionID, control.GoalID))
	}
	return command
}

func readProjectGoalAnswer(command *cobra.Command, path string) (string, error) {
	var contents []byte
	if path == "-" {
		var err error
		contents, err = io.ReadAll(io.LimitReader(command.InOrStdin(), 8193))
		if err != nil {
			return "", fmt.Errorf("read answer: %w", err)
		}
	} else {
		var err error
		contents, err = readProjectGoalJSON(path, 8192)
		if err != nil {
			return "", err
		}
	}
	if len(contents) > 8192 {
		return "", errors.New("answer exceeds 8192 bytes")
	}
	answer := strings.TrimSpace(string(contents))
	if answer == "" {
		return "", errors.New("answer must be nonempty")
	}
	return answer, nil
}

func newProjectGoalCancelCmd(root *string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "cancel <goal-id>", Short: "Request cancellation of one active goal without replaying it", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&asJSON, "json", false, "print the versioned control receipt JSON")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		store, err := projectGoalStore(root)
		if err != nil {
			return projectGoalUsage(err)
		}
		control, err := store.Request(command.Context(), arguments[0], "cancel", "", "")
		if err != nil {
			return projectGoalRefused(err)
		}
		return projectGoalOutput(command, control, asJSON, fmt.Sprintf("queued cancellation %s", control.GoalID))
	}
	return command
}

func newProjectGoalArtifactCmd(root *string) *cobra.Command {
	var kind string
	var index int
	var asJSON bool
	command := &cobra.Command{Use: "artifact <goal-id>", Short: "Read one retained patch, check output, or JSON state artifact", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&kind, "kind", "json", "artifact kind: patch, check, or json")
	command.Flags().IntVar(&index, "index", 0, "zero-based check receipt index")
	command.Flags().BoolVar(&asJSON, "json", false, "wrap the artifact in versioned JSON")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if kind != "patch" && kind != "check" && kind != "json" {
			return projectGoalUsage(errors.New("--kind must be patch, check, or json"))
		}
		if index < 0 || index > 255 {
			return projectGoalUsage(errors.New("--index must be between 0 and 255"))
		}
		store, err := projectGoalStore(root)
		if err != nil {
			return projectGoalUsage(err)
		}
		content, err := store.ReadArtifact(command.Context(), arguments[0], kind, index)
		if err != nil {
			return projectGoalFailure(err)
		}
		if asJSON {
			return writeJSON(command.OutOrStdout(), projectGoalJSON{Version: goal.Version, Kind: kind, GoalID: arguments[0], Content: content})
		}
		_, err = fmt.Fprint(command.OutOrStdout(), safeProjectGoalArtifact(content))
		return err
	}
	return command
}

func safeProjectGoalArtifact(content string) string {
	lines := strings.Split(content, "\n")
	for index := range lines {
		lines[index] = safeText(lines[index])
	}
	return strings.Join(lines, "\n")
}

func newProjectGoalSuperviseCmd(root *string) *cobra.Command {
	var codexBinary string
	command := &cobra.Command{Use: "_supervise <goal-id>", Short: "internal goal supervisor", Hidden: true, Args: cobra.ExactArgs(1), SilenceUsage: true}
	command.Flags().StringVar(&codexBinary, "codex-binary", "", "resolved Codex executable")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if codexBinary == "" {
			return projectGoalUsage(errors.New("--codex-binary is required"))
		}
		store, err := projectGoalStore(root)
		if err != nil {
			return projectGoalUsage(err)
		}
		self, err := os.Executable()
		if err != nil {
			return projectGoalFailure(err)
		}
		self, err = filepath.EvalSymlinks(self)
		if err != nil {
			return projectGoalFailure(err)
		}
		return goal.Run(command.Context(), store, arguments[0], codexBinary, self, command.OutOrStdout())
	}
	return command
}

func newProjectGoalHostCmd() *cobra.Command {
	var specPath string
	command := &cobra.Command{Use: "_host", Short: "internal native Codex host", Hidden: true, Args: cobra.NoArgs, SilenceUsage: true}
	command.Flags().StringVar(&specPath, "spec", "", "private native host specification")
	command.RunE = func(command *cobra.Command, _ []string) error {
		if specPath == "" {
			return projectGoalUsage(errors.New("--spec is required"))
		}
		return goalnative.RunHost(command.Context(), specPath, command.InOrStdin(), command.ErrOrStderr())
	}
	return command
}

func newProjectGoalToolsCmd() *cobra.Command {
	var socket, capabilityFile string
	command := &cobra.Command{Use: "_tools", Short: "internal role-bound goal tools", Hidden: true, Args: cobra.NoArgs, SilenceUsage: true}
	command.Flags().StringVar(&socket, "socket", "", "private supervisor tool socket")
	command.Flags().StringVar(&capabilityFile, "capability-file", "", "private role capability file")
	command.RunE = func(command *cobra.Command, _ []string) error {
		if !filepath.IsAbs(socket) || !filepath.IsAbs(capabilityFile) || filepath.Clean(socket) != socket || filepath.Clean(capabilityFile) != capabilityFile {
			return errors.New("goal tools require absolute clean control paths")
		}
		return goaltools.RunMCP(command.Context(), command.InOrStdin(), command.OutOrStdout(), socket, capabilityFile)
	}
	return command
}

func newProjectGoalFileCmd() *cobra.Command {
	var root, path, content string
	var write bool
	command := &cobra.Command{Use: "_file", Short: "internal bounded role file helper", Hidden: true, Args: cobra.NoArgs, SilenceUsage: true}
	command.Flags().StringVar(&root, "root", "", "fixed file root")
	command.Flags().StringVar(&path, "path", "", "validated relative file path")
	command.Flags().BoolVar(&write, "write", false, "atomically replace the file")
	command.Flags().StringVar(&content, "content", "", "bounded UTF-8 file content")
	command.RunE = func(command *cobra.Command, _ []string) error {
		contentSet := command.Flags().Changed("content")
		if write != contentSet {
			return errors.New("goal _file requires --content only with --write")
		}
		result, err := goaltools.RunFile(root, path, write, content)
		if err != nil {
			return projectGoalRefused(err)
		}
		if !write {
			_, err = io.WriteString(command.OutOrStdout(), result)
		}
		return err
	}
	return command
}

func projectGoalDefaultModel() (string, error) {
	root := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("codex home must be an absolute clean directory")
	}
	path := filepath.Join(root, "config.toml")
	file, err := openRegularNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 1024*1024 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("codex config must be a bounded private regular file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return "", err
	}
	if len(contents) > 1024*1024 {
		return "", errors.New("codex config exceeds 1 MiB")
	}
	var config projectGoalConfig
	if _, err := toml.Decode(string(contents), &config); err != nil {
		return "", errors.New("codex config could not be read as TOML")
	}
	return strings.TrimSpace(config.Model), nil
}
