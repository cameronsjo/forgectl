package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prAgentEnv is the environment override for the review agent, honored when
// --agent is not passed. Mirrors FORGECTL_CLAUDE_BIN's env-over-config posture.
const prAgentEnv = "FORGECTL_PR_AGENT"

// newPrCmd builds `forgectl pr` — the clean-room PR review command group. It
// builds its own pr/net clients (mirrors newNetCmd/newWorkflowCmd) rather than
// sharing the tmux/projects client lifecycle.
func newPrCmd(cfg config.Config) *cobra.Command {
	client := pr.New(exec.OSRunner{})
	netClient := netpkg.New(exec.OSRunner{}, netpkg.WithNetConfig(cfg.Net))

	var (
		agent    string
		headless bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "pr <ref>",
		Short: "Clean-room review of a pull request",
		Long: `pr sets up an isolated, deny-by-default clean room for reviewing a pull
request: it sandboxes the PR head into a throwaway workspace, quarantines any
AI-instruction files, writes a read-only agent allowlist, and dispatches a
review agent into a tmux window. Nothing is ever posted without passing a
human approval gate.

  forgectl pr owner/repo#42        prepare + launch a review
  forgectl pr 42                   same, resolving owner/repo from origin
  forgectl pr <ref> --dry-run      resolve + print the plan, create nothing
  forgectl pr list                 list active review sessions
  forgectl pr attach <breadcrumb>  jump to a review window
  forgectl pr open <breadcrumb>    open a shell in the clean room
  forgectl pr teardown <breadcrumb>  discard a session (alias: close)
  forgectl pr cleanup <YYYY-MM-DD>   discard all sessions from a day
  forgectl pr keys                 tmux-review cheatsheet

The <ref> is validated by an anchored regex: owner/repo#N, a github.com PR
URL, or a bare number. Fetched PR content is treated as hostile input.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ref, err := client.ResolveRef(ctx, args[0])
			if err != nil {
				return err
			}

			// Warn (don't fail) when off-network before a gh round-trip.
			if reachable, err := netClient.Reachable(ctx); err == nil && !reachable {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: internal network unreachable; the gh round-trip may fail")
			}

			sess, err := client.Prepare(ctx, ref, pr.PrepareOpts{
				Agent:    resolveAgent(agent),
				DryRun:   dryRun,
				Headless: headless,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				displayAgent := resolveAgent(agent)
				if displayAgent == "" {
					displayAgent = "claude (default, inline-seeded)"
				}
				fmt.Fprintf(out, "plan: review %s\n", sess.Ref.String())
				fmt.Fprintf(out, "  head: %s @ %s (%s)\n", sess.HeadRef, sess.HeadOid, sess.HeadRepo)
				fmt.Fprintf(out, "  agent: %s\n", displayAgent)
				fmt.Fprintln(out, "  (dry-run: no workspace, window, or breadcrumb created)")
				return nil
			}

			if err := client.Launch(ctx, sess, cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "prepared clean-room review of %s\n", sess.Ref.String())
			fmt.Fprintf(out, "  workspace: %s\n", sess.Workspace)
			fmt.Fprintf(out, "  breadcrumb: %s\n", sess.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "review agent (env "+prAgentEnv+"; default: claude)")
	cmd.Flags().BoolVar(&headless, "headless", false, "stage only; never show the interactive approval gate or auto-post")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and print the plan without creating anything")

	cmd.AddCommand(
		newPrListCmd(client),
		newPrAttachCmd(client),
		newPrOpenCmd(client),
		newPrTeardownCmd(client),
		newPrCleanupCmd(client),
		newPrKeysCmd(),
	)
	return cmd
}

// resolveAgent applies the --agent flag, falling back to the env override.
func resolveAgent(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv(prAgentEnv)
}

func newPrListCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active clean-room review sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sessions, err := client.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(sessions) == 0 {
				fmt.Fprintln(out, "no active review sessions")
				return nil
			}
			for _, s := range sessions {
				fmt.Fprintf(out, "%s\t%s\t%s\n", s.Ref.String(), s.CreatedAt.Format(time.RFC3339), s.Path)
			}
			return nil
		},
	}
}

func newPrAttachCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <breadcrumb>",
		Short: "Jump to a review window",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Attach(cmd.Context(), args[0])
		},
	}
}

func newPrOpenCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "open <breadcrumb>",
		Short: "Open a shell window in the clean-room workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Open(cmd.Context(), args[0])
		},
	}
}

func newPrTeardownCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:     "teardown <breadcrumb>",
		Aliases: []string{"close"},
		Short:   "Discard a review session (restore + remove workspace)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client.Teardown(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "torn down %s\n", args[0])
			return nil
		},
	}
}

func newPrCleanupCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup <YYYY-MM-DD>",
		Short: "Discard every review session created on a given day",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := time.Parse("2006-01-02", args[0]); err != nil {
				return fmt.Errorf("invalid date %q: want YYYY-MM-DD", args[0])
			}
			if err := client.Cleanup(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cleaned up sessions from %s\n", args[0])
			return nil
		},
	}
}

func newPrKeysCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keys",
		Short: "tmux cheatsheet for driving a clean-room review",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(cmd.OutOrStdout(), prKeysText)
			return nil
		},
	}
}

// prKeysText is the static tmux-review cheatsheet — the keys that matter when
// driving a review window (model: tmux_cheat.go + tui.Cheatsheet, but scoped
// to the pr flow and self-contained so it needs no tui dependency).
const prKeysText = `clean-room review — tmux keys that matter

  prefix = Ctrl-b (default)

  navigate
    prefix w        window picker (pick the pr-<N> review window)
    prefix n / p    next / previous window
    prefix <N>      jump to window N

  read
    prefix [        enter copy-mode (scroll the review output)
    q               leave copy-mode
    prefix z        zoom the active pane fullscreen (toggle)

  forgectl pr
    pr attach <b>   jump to a review window by breadcrumb
    pr open <b>     open a shell in the clean-room workspace
    pr teardown <b> discard the session (restore + remove workspace)

Nothing is posted to the PR without passing forgectl's approval gate.
`
