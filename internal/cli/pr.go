package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
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
	client := pr.New(deps.Runner, pr.WithApprovalTheme(deps.Theme))
	netClient := netpkg.New(deps.Runner, netpkg.WithNetConfig(cfg.Net))
	// err discarded: a failed config-dir lookup yields "", which LoadReviewed
	// reads as an empty store and persist() rejects loudly — never a silent bad write.
	reviewedPath, _ := config.PrReviewedPath()
	return newPrCmdForClient(cfg, client, netClient, reviewedPath, deps.Theme)
}

func newPrCmdForClient(cfg config.Config, client *pr.Client, netClient *netpkg.Client, reviewedPath string, th theme.Theme) *cobra.Command {

	var (
		agent    string
		headless bool
		dryRun   bool
		noVerify bool
		queue    bool
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
  forgectl pr <ref> --queue        defer to the drainer instead of launching now
  forgectl pr local                offline review of local committed changes
  forgectl pr list                 list active review sessions
  forgectl pr attach <breadcrumb>  jump to a review window
  forgectl pr open <breadcrumb>    open a shell in the clean room
  forgectl pr teardown <breadcrumb>  discard a session or queue entry
  forgectl pr repair               settle sessions whose record and reality disagree
  forgectl pr cleanup <YYYY-MM-DD>   discard all sessions from a day
  forgectl pr queue                 list reviews waiting for the drainer
  forgectl pr drain                 launch queued reviews as cap slots free up
  forgectl pr findings list|cleanup  reclaim durable local-review findings
  forgectl pr keys                 tmux-review cheatsheet

The <ref> is validated by an anchored regex: owner/repo#N, a github.com PR
URL, or a bare number. Fetched PR content is treated as hostile input.

The concurrency cap ([pr] max_concurrent in config.toml) governs every launch
path — this command, 'pr local', and 'pr pick' alike. At the cap this command
refuses with nothing prepared; --queue writes a queued record instead and
exits 0, to be started later by 'forgectl pr drain --once'.`,
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

			// A named PR ref is a third party's content by construction,
			// whatever the owner spells and whoever opened it. There is no
			// flag to override this and there must not be one: an operator
			// reviewing their OWN branch locally uses `pr local
			// --operator-authored`, which reviews the tree in front of them
			// rather than a fetched head.
			prepOpts := pr.PrepareOpts{
				Agent:      resolveAgent(agent),
				DryRun:     dryRun,
				Headless:   headless,
				Provenance: pr.ReviewProvenanceThirdParty,
			}

			// Every pure policy refusal runs HERE, before any of the three
			// branches below, because none of them needs I/O to decide it and
			// all three owe the same answer.
			//
			// Ahead of --queue it closes the one gap where a queued record
			// could store an agent/provenance pairing the immediate path
			// refuses: the drainer would then hit the refusal hours later, on
			// someone else's command, instead of here on the operator's.
			// Ahead of Reserve it keeps the promise this command's own Long
			// text makes — at the cap it "refuses with nothing prepared", and
			// a refusal after Reserve would leave a `preparing` record holding
			// a slot. Prepare and Launch both re-check; these are the fast
			// fail, not the authoritative gate.
			if err := pr.CheckAgentForReview(prepOpts.Agent, pr.EffectiveProvenance(ref, prepOpts.Provenance)); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				sess, err := client.Prepare(ctx, ref, prepOpts)
				if err != nil {
					return err
				}
				displayAgent := agentDisplayLabel(resolveAgent(agent))
				fmt.Fprintf(out, "plan: review %s\n", sess.Ref.String())
				fmt.Fprintf(out, "  head: %s @ %s (%s)\n", sess.HeadRef, sess.HeadOid, sess.HeadRepo)
				fmt.Fprintf(out, "  agent: %s\n", displayAgent)
				fmt.Fprintln(out, "  (dry-run: no workspace, window, or breadcrumb created)")
				return nil
			}

			// --queue always defers to the drainer rather than attempting a
			// launch now: it is an explicit choice, not merely a fallback the
			// cap refusal below offers, so it never even attempts reserve —
			// and never touches tmux at all, so it needs no dispatch-capability
			// floor (the same reasoning `pr pick`'s cap-full branch already
			// applies).
			if queue {
				path, err := client.Queue(ctx, ref, prepOpts)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "queued %s\n", ref.String())
				_, _ = fmt.Fprintf(out, "  record: %s\n", path)
				_, _ = fmt.Fprintln(out, "  start it: forgectl pr drain --once")
				return nil
			}

			if err := client.CheckDispatchCapability(ctx); err != nil {
				return err
			}

			recordPath, err := client.Reserve(ctx, ref, cfg.Pr.MaxConcurrent, prepOpts)
			if err != nil {
				if maxN, liveN, ok := pr.ReviewCapReached(err); ok {
					return reviewCapRefusal(maxN, liveN, ref)
				}
				return err
			}
			prepOpts.RecordPath = recordPath

			sess, err := client.Prepare(ctx, ref, prepOpts)
			if err != nil {
				return parkReservation(ctx, client, recordPath, ref.String(), err)
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
	cmd.Flags().StringVar(&agent, "agent", "", "review agent (env "+prAgentEnv+"; default: claude; codex is refused for PR heads)")
	cmd.Flags().BoolVar(&headless, "headless", false, "stage only; never show the interactive approval gate or auto-post")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and print the plan without creating anything")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the delayed post-dispatch window check")
	cmd.Flags().BoolVar(&queue, "queue", false, "defer to 'forgectl pr drain' instead of launching now; writes a queued record and exits 0")

	cmd.AddCommand(
		newPrLocalCmd(client, cfg),
		newPrListCmd(client),
		newPrAttachCmd(client),
		newPrOpenCmd(client),
		newPrTeardownCmd(client),
		newPrRepairCmd(client),
		newPrCleanupCmd(client),
		newPrQueueCmd(client),
		newPrDrainCmd(client, cfg),
		newPrFindingsCmd(client, th),
		newPrKeysCmd(),
		newPrPrsCmd(client, th),
		newPrDashCmd(client, th),
		newPrPickCmd(client, cfg, th),
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

// reviewCapRefusal is the three-line admission refusal `pr <ref>` and `pr
// local` return at the cap: what's running, how to defer, how to raise it.
// The wording is pinned by the design doc so a script or a human reading the
// message finds the same two next steps every time.
func reviewCapRefusal(maxN, liveN int, ref pr.Ref) error {
	return fmt.Errorf("review cap reached (max %d, %d running) — nothing prepared.\n"+
		"  see them:   forgectl pr list\n"+
		"  queue it:   forgectl pr %s --queue   (then: forgectl pr drain --once)\n"+
		"  raise it:   [pr] max_concurrent in config.toml",
		maxN, liveN, ref.String())
}

// parkReservation settles a reservation whose prepare failed AFTER the slot
// was claimed, then returns the original prepare error unchanged — the single
// -ref launch paths' equivalent of PrepareMany's inline park (internal/pr's
// discover.go), so the two admission routes leak nothing on the same failure.
//
// Without the park, a failed prepare leaves a `preparing` record with no clean
// room behind it: it counts against the cap forever and blocks every retry of
// the same ref, so four transient `gh` failures would exhaust a default cap of
// four and refuse every later launch. A park makes the cost of a transient
// failure one retry.
//
// The prepare error is what the operator sees. A failure to park is logged and
// swallowed rather than returned, because replacing the real diagnosis with a
// bookkeeping error would hide why the review did not start.
func parkReservation(ctx context.Context, client *pr.Client, recordPath, ref string, cause error) error {
	if err := client.ParkFailedReservation(ctx, recordPath, "prepare failed: "+cause.Error()); err != nil {
		slog.Error("Failed to park a reservation whose prepare failed; its slot stays reserved.",
			"ref", ref, "path", recordPath, "error", err)
	}
	return cause
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
	case s.IsWorkspaceNone():
		// A queued or preparing record has no workspace by design; its phase
		// IS its status, and rendering "workspace missing" — the word for
		// damage — over a healthy intended state would send a user to teardown.
		return string(s.Phase())
	case s.IsWorkspaceMissing():
		return workspaceMissingStatus
	case s.IsWorkspaceLive():
		return windowStatus(live, s.Ref(), tmuxOK)
	default:
		return workspaceUnclassifiedStatus
	}
}

// phaseLabel renders the recorded phase for the human table: the phase
// itself, or "-" for a legacy record that predates phases. Quiet ground —
// absence is the signal, not a glyph.
func phaseLabel(s pr.SessionSummary) string {
	if s.Phase() == "" {
		return "-"
	}
	return string(s.Phase())
}

// unreadableRecordsNote is what `pr list` prints on stderr when List skipped
// records it could not decode. It exists because the slog warning that also
// fires lands in a handler a default install discards, and an older binary
// reading newer records would otherwise show fewer rows, exit 0, and say
// nothing.
func unreadableRecordsNote(n int) string {
	return fmt.Sprintf("%d record(s) could not be read — they are not listed; an older forgectl cannot read records a newer one wrote", n)
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

// prListRowJSON is the --json wire shape for one `pr list` row — the same
// five fields as the human table's tab-separated columns, in the same order.
// phase was added last (ADR-0008: additive only); it is "" on a legacy record.
type prListRowJSON struct {
	Ref       string `json:"ref"`
	CreatedAt string `json:"created_at"`
	Path      string `json:"path"`
	Status    string `json:"status"`
	Phase     string `json:"phase"`
}

func newPrListCmd(client *pr.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active clean-room review sessions",
		Long: `list prints one tab-separated row per recorded review session:

  REF   CREATED   PATH   WINDOW   PHASE

WINDOW is what tmux reports RIGHT NOW — live, window gone, or ? when tmux
could not be read at all. PHASE is what the record SAYS about how far the
session got. The two are separate on purpose: a record reading 'launching'
beside 'no window' is a session that died between the two, and
'forgectl pr repair' is what settles that disagreement. PHASE is '-' on a
record written before phases existed.

Fields are append-only: PATH is field 3 and stays there, because it is the
operand 'forgectl pr teardown' takes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			summaries, unreadable, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			if unreadable > 0 {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), unreadableRecordsNote(unreadable))
			}
			out := cmd.OutOrStdout()
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
			live, tmuxOK := prListLiveness(cmd.Context(), client, summaries)
			if asJSON {
				return writePrListJSON(out, summaries, live, tmuxOK)
			}
			if len(summaries) == 0 {
				_, _ = fmt.Fprintln(out, "no active review sessions")
				return nil
			}
			for _, s := range summaries {
				// Status and phase are APPENDED, never inserted. The breadcrumb
				// path is field 3 and README documents it as what `pr teardown`
				// is fed, so a script cutting field 3 keeps working; shifting it
				// would hand those callers a timestamp. Phase is field 5:
				// status (field 4) is what tmux OBSERVES right now, phase is
				// what the record SAYS, and a disagreement between the two is
				// what `pr repair` settles.
				//
				// The path is a FILENAME chosen on disk, so it is the one field
				// here that can carry ANSI or bidi controls; Ref is
				// charset-constrained by ParseRef and the timestamp is
				// formatted. This row is BOTH rendered to a terminal and
				// parsed by scripts, so the escaping is conditional: an
				// ordinary path prints verbatim and field 3 stays exactly
				// what teardown is fed, while a control-bearing one prints
				// as a quoted literal instead of driving the terminal.
				_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
					s.Ref().String(), s.CreatedAt().Format(time.RFC3339),
					termsafe.QuotePathIfUnsafe(s.Path()),
					sessionStatus(live, s, tmuxOK),
					phaseLabel(s))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"ref":...,"created_at":...,"path":...,"status":...,"phase":...}] to stdout`)
	return cmd
}

// prListLiveness cross-checks each live-workspace summary against tmux's
// window list, exactly once, and returns the map both the human table and
// the --json rows read `sessionStatus` against. A stale-only list issues
// zero tmux calls, matching the human path's cost contract.
func prListLiveness(ctx context.Context, client *pr.Client, summaries []pr.SessionSummary) (live map[pr.Ref]bool, tmuxOK bool) {
	refs := make([]pr.Ref, 0, len(summaries))
	for _, s := range summaries {
		if s.IsWorkspaceLive() {
			refs = append(refs, s.Ref())
		}
	}
	tmuxOK = true
	if len(refs) > 0 {
		live, tmuxOK = client.WindowsLive(ctx, refs)
	}
	return live, tmuxOK
}

// writePrListJSON encodes the active review sessions as a JSON array through
// the sanctioned termsafe seam. An empty result encodes [], never null: the
// path is the one field here that can carry attacker-controlled bytes (a
// FILENAME chosen on disk), and the encoder's own escaping is what makes it
// terminal-safe on the way out — no QuotePathIfUnsafe pass is needed here.
func writePrListJSON(out io.Writer, summaries []pr.SessionSummary, live map[pr.Ref]bool, tmuxOK bool) error {
	rows := make([]prListRowJSON, 0, len(summaries))
	for _, s := range summaries {
		rows = append(rows, prListRowJSON{
			Ref:       s.Ref().String(),
			CreatedAt: s.CreatedAt().Format(time.RFC3339),
			Path:      s.Path(),
			Status:    sessionStatus(live, s, tmuxOK),
			Phase:     string(s.Phase()),
		})
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
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
		Short: "Discard a review session or queue entry",
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
    pr teardown <b> discard a session or queue entry
    pr repair       settle a session whose record and reality disagree
    pr queue        list reviews waiting for the drainer
    pr drain        launch queued reviews as cap slots free up

Nothing is posted to the PR without passing forgectl's approval gate.
`
