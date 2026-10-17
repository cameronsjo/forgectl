package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/surface/hermes"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func newSurfaceHermesCmd() *cobra.Command {
	var (
		commandPath string
		model       string
		ledger      string
		member      string
		worktree    string
		socket      string
		listen      string
		runID       string
		sessionID   string
		nativeArgs  []string
		asJSON      bool
	)
	command := &cobra.Command{
		Use:          "hermes",
		Short:        "Run a native Hermes ACP session behind the private surface socket",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(command *cobra.Command, _ []string) error {
			getenv := os.Getenv
			if ledger == "" {
				ledger = getenv(mail.EnvLedger)
			}
			if member == "" {
				member = getenv(mail.EnvWorker)
				if member == "" {
					member = getenv(mail.EnvCoordinator)
				}
			}
			if runID == "" {
				runID = getenv(mail.EnvRunID)
			}
			if worktree == "" {
				var err error
				worktree, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			if socket == "" {
				socket = getenv("FORGECTL_HERMES_SOCKET")
			}
			if sessionID == "" {
				sessionID = getenv("FORGECTL_HERMES_SESSION_ID")
			}
			if !asJSON {
				return errors.New("surface hermes emits JSON events; use --json")
			}
			if commandPath == "" || model == "" || ledger == "" || member == "" || socket == "" || runID == "" {
				return errors.New("--command, --model, --ledger, --member, --socket and --run-id are required (or provided by the team environment)")
			}
			directory, err := privateTeamLedger(ledger, false)
			if err != nil {
				return err
			}
			roster := mail.FileRoster{Dir: directory}
			self, err := mail.SelfName(roster, getenv)
			if err != nil {
				return err
			}
			if self != member {
				return errors.New("--member does not match the current team identity")
			}
			registered, err := roster.Get(member)
			if err != nil {
				return fmt.Errorf("hermes member: %w", err)
			}
			if registered.Harness != mail.HarnessHermes {
				return fmt.Errorf("hermes member %s is registered as %s", member, registered.Harness)
			}
			if registered.RunID != runID {
				return errors.New("hermes run id does not match the registered member")
			}
			if !filepath.IsAbs(worktree) || filepath.Clean(registered.Worktree) != filepath.Clean(worktree) {
				return errors.New("--worktree does not match the registered member")
			}
			if sessionID != "" && registered.ThreadID != sessionID {
				return errors.New("--session-id does not match the registered native session")
			}
			if filepath.Clean(registered.Socket) != filepath.Clean(socket) {
				return errors.New("hermes socket does not match the registered member")
			}
			if sessionID == "" {
				sessionID = registered.ThreadID
			}
			ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return hermes.Run(ctx, hermes.Config{
				Command:      commandPath,
				Args:         nativeArgs,
				Model:        model,
				Ledger:       directory,
				Member:       member,
				Worktree:     worktree,
				Socket:       socket,
				ListenSocket: listen,
				RunID:        runID,
				SessionID:    sessionID,
				OnSession: func(session string) error {
					return roster.Update(member, func(current *mail.Worker) error {
						if current.Harness != mail.HarnessHermes || current.RunID != runID || filepath.Clean(current.Worktree) != filepath.Clean(worktree) || filepath.Clean(current.Socket) != filepath.Clean(socket) {
							return errors.New("hermes roster member changed while starting")
						}
						if current.ThreadID != "" && current.ThreadID != session {
							return errors.New("hermes native session does not match the registered session")
						}
						current.ThreadID = session
						current.State = mail.StateUnknown
						return nil
					})
				},
				Stdout: command.OutOrStdout(),
				Stderr: command.ErrOrStderr(),
			})
		},
	}
	command.Flags().StringVar(&commandPath, "command", "", "absolute Hermes ACP executable")
	command.Flags().StringVar(&model, "model", "", "provider-qualified model (PROVIDER:MODEL)")
	command.Flags().StringVar(&ledger, "ledger", "", "private team ledger (defaults to FORGECTL_LEDGER)")
	command.Flags().StringVar(&member, "member", "", "registered Hermes member name")
	command.Flags().StringVar(&worktree, "worktree", "", "absolute native working directory (defaults to cwd)")
	command.Flags().StringVar(&socket, "socket", "", "registered route socket (defaults to FORGECTL_HERMES_SOCKET)")
	command.Flags().StringVar(&listen, "listen-socket", "", "socket this process binds; defaults to --socket")
	command.Flags().StringVar(&runID, "run-id", "", "registered process incarnation (defaults to FORGECTL_RUN_ID)")
	command.Flags().StringVar(&sessionID, "session-id", "", "existing native session to load")
	command.Flags().StringArrayVar(&nativeArgs, "arg", nil, "argument passed to the native ACP executable (repeatable)")
	command.Flags().BoolVar(&asJSON, "json", false, "emit one JSON event per line")
	return command
}
