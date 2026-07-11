package cli

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/sessions"
)

// newSessionsCmd builds `forgectl sessions` — the cross-machine operational
// mart ETL and its query surface. Mirrors the house pattern: this layer parses
// flags and prints receipts; internal/sessions owns the logic.
func newSessionsCmd(cfg config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Sync local session ledgers into the operational mart and query it",
		Long: `sessions drains this machine's local JSONL write-ahead log
(~/.claude/metrics/) and the runbook markdown corpus
(~/.claude/cadence/runbooks/) into the cross-machine operational mart — an
always-on Postgres session index — and queries the runbook full-text index.

JSONL is the WAL; Postgres is the index. Hooks only ever append locally, so an
offline machine keeps logging and drains on the next reachable sync. The DSN
comes from [sessions] dsn in config, overridden by FORGECTL_SESSIONS_DSN, then
--dsn; keep the password out of the DSN and in ~/.pgpass instead.`,
	}
	cmd.AddCommand(newSessionsSyncCmd(cfg))
	cmd.AddCommand(newSessionsSearchCmd(cfg))
	return cmd
}

// resolveDSN applies the documented precedence: --dsn > FORGECTL_SESSIONS_DSN
// > [sessions] dsn.
func resolveDSN(flagDSN string, cfg config.SessionsConfig) string {
	if flagDSN != "" {
		return flagDSN
	}
	if env := os.Getenv("FORGECTL_SESSIONS_DSN"); env != "" {
		return env
	}
	return cfg.DSN
}

func newSessionsSyncCmd(cfg config.Config) *cobra.Command {
	var opts sessions.SyncOptions
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Drain local JSONL + runbook markdown into the mart (idempotent)",
		Long: `sync upserts one operational row per local session (key: session_id alone;
machine is provenance) and rebuilds the runbook full-text index from the
markdown corpus. Idempotent: a second run reports the same sessions as
unchanged. lastMessageId is the incremental-sync watermark; --full bypasses it.

Cost attribution follows ADR-0017: a session with commits is priced from
commits.jsonl (grouped by parentSessionId//sessionId), never recomputed; the
SessionEnd total from sessions.jsonl is the fallback.

Every run enforces the Syncthing-blobs-only guard first: a Syncthing folder
covering ~/.claude/metrics or ~/.claude/cadence/sessions fails the sync (a
synced JSONL ledger forks into .sync-conflict-* divergence). A missing or
unreadable Syncthing config warns and proceeds.

The run ends with a completeness receipt:
  N local sessions found -> M upserted (K unchanged) -> reconciled
Any local session absent from the mart after the flush prints as MISSING and
the command exits non-zero — a skipped session is never silent.

  forgectl sessions sync --dry-run     read + count, no DB connection
  forgectl sessions sync               drain into the configured mart
  forgectl sessions sync --full        ignore watermarks, re-upsert everything`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.DSN = resolveDSN(opts.DSN, cfg.Sessions)
			resolved, err := opts.Resolve(cfg.Sessions)
			if err != nil {
				return err
			}
			receipt, err := sessions.Sync(cmd.Context(), resolved)
			if err != nil {
				return err
			}
			return printReceipt(cmd, receipt)
		},
	}
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "read + transform + count; no DB connection")
	cmd.Flags().BoolVar(&opts.Full, "full", false, "bypass the lastMessageId watermark and re-upsert every session")
	cmd.Flags().StringVar(&opts.DSN, "dsn", "", "mart DSN (default: FORGECTL_SESSIONS_DSN, then [sessions] dsn)")
	cmd.Flags().StringVar(&opts.Machine, "machine", "", "provenance label (default: [sessions] machine, then short hostname)")
	cmd.Flags().StringVar(&opts.MetricsDir, "metrics-dir", "", "JSONL WAL directory (default: ~/.claude/metrics)")
	cmd.Flags().StringVar(&opts.RunbooksDir, "runbooks-dir", "", "runbook markdown corpus (default: ~/.claude/cadence/runbooks)")
	cmd.Flags().StringVar(&opts.SyncthingConfig, "syncthing-config", "", "Syncthing config.xml for the blobs-only guard (default: platform discovery)")
	return cmd
}

// printReceipt renders the completeness receipt. MISSING sessions make the
// command fail loudly — the acceptance contract of the sync.
func printReceipt(cmd *cobra.Command, r *sessions.Receipt) error {
	out := cmd.OutOrStdout()
	mode := ""
	if r.DryRun {
		mode = " (dry-run: no database connection made)"
	}
	fmt.Fprintf(out, "%d local sessions found -> %d upserted (%d unchanged, %d invalid, %d dropped commit rows, %d bad ledger lines)%s\n",
		r.SessionsFound, r.SessionsUpserted, r.SessionsUnchanged, r.InvalidRows, r.CommitRowsDropped, r.LedgerLinesBad, mode)
	fmt.Fprintf(out, "%d runbooks found -> %d indexed, %d pruned\n",
		r.RunbooksFound, r.RunbooksUpserted, r.RunbooksPruned)
	if r.DryRun {
		return nil
	}
	if !r.Complete() {
		for _, id := range r.Missing {
			fmt.Fprintf(out, "MISSING %s\n", id)
		}
		return fmt.Errorf("reconcile failed: %d local sessions absent from the mart after flush", len(r.Missing))
	}
	fmt.Fprintln(out, "reconciled: every local session is present in the mart")
	return nil
}

func newSessionsSearchCmd(cfg config.Config) *cobra.Command {
	var (
		dsn     string
		project string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search the mart's runbook index",
		Long: `search runs a websearch-syntax full-text query over the mart's runbooks
index (title-weighted tsvector; trigram fallback for partial tokens), so any
machine can find a runbook or field report it did not author.

  forgectl sessions search "colima split brain"
  forgectl sessions search --project cadence "worktree guard"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved := resolveDSN(dsn, cfg.Sessions)
			if resolved == "" {
				return fmt.Errorf("no mart DSN: set [sessions] dsn in config, FORGECTL_SESSIONS_DSN, or --dsn")
			}
			mart, err := sessions.ConnectMart(cmd.Context(), resolved)
			if err != nil {
				return err
			}
			defer mart.Close(cmd.Context())
			hits, err := mart.SearchRunbooks(cmd.Context(), args[0], project, limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(hits) == 0 {
				fmt.Fprintln(out, "no runbooks matched")
				return nil
			}
			for _, h := range hits {
				// Indexed content is untrusted at print time — strip control
				// bytes so a hostile title/snippet can't smuggle terminal
				// escape sequences to the operator's shell.
				fmt.Fprintf(out, "%s\t%s\t[%s]\t(%s, indexed by %s)\n\t%s\n",
					sanitizeTerm(h.Path), sanitizeTerm(h.Title), sanitizeTerm(h.Type),
					sanitizeTerm(h.Project), sanitizeTerm(h.Machine), sanitizeTerm(h.Snippet))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "mart DSN (default: FORGECTL_SESSIONS_DSN, then [sessions] dsn)")
	cmd.Flags().StringVar(&project, "project", "", "restrict matches to one project")
	cmd.Flags().IntVar(&limit, "limit", 10, "maximum hits to return")
	return cmd
}

// sanitizeTerm replaces control bytes (everything unicode.IsControl except
// tab) with spaces so mart-indexed content renders inert in the terminal.
func sanitizeTerm(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return ' '
	}, s)
}
