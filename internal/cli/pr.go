package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// prAgentEnv is the environment override for the review agent, honored when
// --agent is not passed. Mirrors FORGECTL_CLAUDE_BIN's env-over-config posture.
const prAgentEnv = "FORGECTL_PR_AGENT"

// prModule declares the clean-room PR review core module (ADR-0005): owns
// the [pr] section (the bulk-launch concurrency cap), and separately reads
// [net] for the reachability probe (net owns that section). No alias
// surface.
var prModule = module.Manifest{
	Name:      "pr",
	Tier:      module.TierCore,
	ConfigKey: "pr",
	New:       newPrCmd,
}

// newPrCmd builds `forgectl pr` over the registry Deps — the clean-room PR
// review command group, building its own pr/net clients from deps.Runner.
func newPrCmd(deps module.Deps) *cobra.Command {
	cfg := deps.Cfg
	client := pr.New(deps.Runner)
	netClient := netpkg.New(deps.Runner, netpkg.WithNetConfig(cfg.Net))
	// err discarded: a failed config-dir lookup yields "", which LoadReviewed
	// reads as an empty store and persist() rejects loudly — never a silent bad write.
	reviewedPath, _ := config.PrReviewedPath()
	return newPrCmdForClient(cfg, client, netClient, reviewedPath)
}

func newPrCmdForClient(cfg config.Config, client *pr.Client, netClient *netpkg.Client, reviewedPath string) *cobra.Command {

	var (
		agent    string
		headless bool
		dryRun   bool
		noVerify bool
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
  forgectl pr local                offline review of local committed changes
  forgectl pr list                 list active review sessions
  forgectl pr attach <breadcrumb>  jump to a review window
  forgectl pr open <breadcrumb>    open a shell in the clean room
  forgectl pr teardown <breadcrumb>  discard a session
  forgectl pr cleanup <YYYY-MM-DD>   discard all sessions from a day
  forgectl pr findings list|cleanup  reclaim durable local-review findings
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
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: network unreachable; the gh round-trip may fail")
			}

			if !dryRun {
				if err := client.CheckDispatchCapability(ctx); err != nil {
					return err
				}
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
				displayAgent := agentDisplayLabel(resolveAgent(agent))
				fmt.Fprintf(out, "plan: review %s\n", sess.Ref.String())
				fmt.Fprintf(out, "  head: %s @ %s (%s)\n", sess.HeadRef, sess.HeadOid, sess.HeadRepo)
				fmt.Fprintf(out, "  agent: %s\n", displayAgent)
				fmt.Fprintln(out, "  (dry-run: no workspace, window, or breadcrumb created)")
				return nil
			}

			dispatch, err := client.Launch(ctx, sess, cfg)
			if err != nil {
				return err
			}
			if err := dispatchVerificationError(verifyReviewDispatches(ctx, client, []pr.Dispatch{dispatch}, noVerify)); err != nil {
				return err
			}
			// CLI-layer courtesy note: an explicitly named ref is always a
			// deliberate launch (never skipped — that's the picker's job), but
			// flag it if we've marked it reviewed before. No session.go change.
			if at, ok := pr.LoadReviewed(reviewedPath).ReviewedAt(ref); ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: previously marked reviewed (%s ago)\n",
					time.Since(at).Round(time.Minute))
			}
			fmt.Fprintf(out, "prepared clean-room review of %s\n", sess.Ref.String())
			fmt.Fprintf(out, "  workspace: %s\n", sess.Workspace)
			fmt.Fprintf(out, "  breadcrumb: %s\n", sess.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "review agent (env "+prAgentEnv+"; default: claude; codex is local-review only)")
	cmd.Flags().BoolVar(&headless, "headless", false, "stage only; never show the interactive approval gate or auto-post")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and print the plan without creating anything")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the delayed post-dispatch window check")

	cmd.AddCommand(
		newPrLocalCmd(client, cfg),
		newPrListCmd(client),
		newPrAttachCmd(client),
		newPrOpenCmd(client),
		newPrTeardownCmd(client),
		newPrCleanupCmd(client),
		newPrFindingsCmd(client),
		newPrKeysCmd(),
		newPrPrsCmd(client),
		newPrDashCmd(client),
		newPrPickCmd(client, cfg),
		newPrReviewedCmd(client),
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

// agentDisplayLabel renders agent for dry-run plan output, substituting a
// descriptive default when it's empty — shared by `pr <ref>` and `pr local`
// so the two dry-run plans can't silently diverge on this label.
func agentDisplayLabel(agent string) string {
	if agent == "" {
		return "claude (default, inline-seeded)"
	}
	return agent
}

// workspaceMissingStatus is the `pr list` / `pr dash` label for a review whose
// recorded workspace has been deleted. Presentation policy lives here, in the
// CLI: internal/pr models the state as a private enum with no labels, so this
// wording can change without touching that contract.
const workspaceMissingStatus = "workspace missing"

// workspaceUnclassifiedStatus is what BOTH human sinks say about a summary for
// which neither predicate holds. It is unreachable through List, which emits
// only live or missing summaries, so seeing it means a summary was constructed
// outside the loader.
//
// It is an internal-error string rather than a label because inventing a human
// wording for an unclassified state is how a fail-closed enum quietly becomes
// fail-open — and staying silent is worse still: an unmarked row reads as an
// ordinary LIVE review, which is the one thing this state cannot promise.
const workspaceUnclassifiedStatus = "internal error: unclassified workspace state"

// sessionStatus renders one summary's status field for `pr list`.
//
// A record whose workspace is gone reports that and nothing else: its tmux
// window is irrelevant, and it is never included in the liveness read at all.
// For a live record the behavior is unchanged and still FAILS SOFT — when tmux
// could not be read (tmuxOK false) every row reports "?", because an
// unreadable tmux says nothing about any individual window, and rendering
// those rows as "window gone" would flag every healthy review as dead the
// moment tmux hiccups.
//
// The final branch is unreachable through List; see workspaceUnclassifiedStatus
// for why it is an internal error rather than a label.
func sessionStatus(live map[pr.Ref]bool, s pr.SessionSummary, tmuxOK bool) string {
	switch {
	case s.IsWorkspaceMissing():
		return workspaceMissingStatus
	case s.IsWorkspaceLive():
		return windowStatus(live, s.Ref(), tmuxOK)
	default:
		return workspaceUnclassifiedStatus
	}
}

// windowStatus renders one live session's review-window liveness.
func windowStatus(live map[pr.Ref]bool, ref pr.Ref, tmuxOK bool) string {
	if !tmuxOK {
		return "?"
	}
	if live[ref] {
		return "live"
	}
	return "window gone"
}

func newPrListCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active clean-room review sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			summaries, err := client.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(summaries) == 0 {
				fmt.Fprintln(out, "no active review sessions")
				return nil
			}
			// A breadcrumb only proves a review was DISPATCHED, never that it
			// survived: tmux new-window exits 0 before the agent runs, and a
			// window whose agent dies is destroyed outright. Cross-check each
			// session against the live window list so a dead review stops
			// reading as "still thinking".
			//
			// Only LIVE rows go into that read. A stale record's window is not
			// a question worth asking, so a list of nothing but stale records
			// issues zero tmux calls — and in a mixed list, an unreadable tmux
			// degrades only the live rows.
			refs := make([]pr.Ref, 0, len(summaries))
			for _, s := range summaries {
				if s.IsWorkspaceLive() {
					refs = append(refs, s.Ref())
				}
			}
			var live map[pr.Ref]bool
			tmuxOK := true
			if len(refs) > 0 {
				live, tmuxOK = client.WindowsLive(cmd.Context(), refs)
			}
			for _, s := range summaries {
				// Status is APPENDED, never inserted. The breadcrumb path is
				// field 3 and README documents it as what `pr teardown` is fed,
				// so a script cutting field 3 keeps working; shifting it would
				// hand those callers a timestamp.
				//
				// The path is a FILENAME chosen on disk, so it is the one field
				// here that can carry ANSI or bidi controls; Ref is
				// charset-constrained by ParseRef and the timestamp is
				// formatted. This row is BOTH rendered to a terminal and
				// parsed by scripts, so the escaping is conditional: an
				// ordinary path prints verbatim and field 3 stays exactly
				// what teardown is fed, while a control-bearing one prints
				// as a quoted literal instead of driving the terminal.
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\n",
					s.Ref().String(), s.CreatedAt().Format(time.RFC3339),
					termsafe.QuotePathIfUnsafe(s.Path()),
					sessionStatus(live, s, tmuxOK))
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
	// Deliberately no "close" alias: it collides with Bash(gh pr close:*)
	// in the reviewer allowlist (internal/pr/allowlist.go) closely enough
	// to read as the same verb, even though forgectl pr close and gh pr
	// close are unrelated commands (forgectl#190). Do not re-add it.
	return &cobra.Command{
		Use:   "teardown <breadcrumb>",
		Short: "Discard a review session (restore + remove workspace)",
		Args:  cobra.ExactArgs(1),
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
