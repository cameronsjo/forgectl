package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/sessions"
)

// sessionsModule declares the operational-mart ETL extension (ADR-0005):
// owns the [sessions] config section, no alias surface.
var sessionsModule = module.Manifest{
	Name:      "sessions",
	Tier:      module.TierExtension,
	ConfigKey: "sessions",
	New:       newSessionsCmd,
}

// newSessionsCmd builds `forgectl sessions` over the registry Deps — the
// cross-machine operational mart ETL and its query surface. Mirrors the house
// pattern: this layer parses flags and prints receipts; internal/sessions
// owns the logic. (No Runner use: the domain package speaks pgx, not argv.)
func newSessionsCmd(deps module.Deps) *cobra.Command {
	cfg := deps.Cfg
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
	cmd.AddCommand(newSessionsWhyCmd(cfg))
	cmd.AddCommand(newSessionsLastCmd(cfg))
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

// withMart runs fn against an open mart connection, closing it after — the
// resolve-DSN-or-die + connect-or-die preamble the query verbs (search, why,
// last) all share. An empty DSN is a usage error, never a nil connection.
func withMart(cmd *cobra.Command, dsn string, cfg config.SessionsConfig, fn func(*sessions.Mart) error) error {
	resolved := resolveDSN(dsn, cfg)
	if resolved == "" {
		return fmt.Errorf("no mart DSN: set [sessions] dsn in config, FORGECTL_SESSIONS_DSN, or --dsn")
	}
	mart, err := sessions.ConnectMart(cmd.Context(), resolved)
	if err != nil {
		return err
	}
	defer mart.Close(cmd.Context())
	return fn(mart)
}

// writeJSON encodes v as indented JSON to out — the shared --json emitter for
// the query verbs' stable, pipeable output.
func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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
			return withMart(cmd, dsn, cfg.Sessions, func(mart *sessions.Mart) error {
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
			})
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "mart DSN (default: FORGECTL_SESSIONS_DSN, then [sessions] dsn)")
	cmd.Flags().StringVar(&project, "project", "", "restrict matches to one project")
	cmd.Flags().IntVar(&limit, "limit", 10, "maximum hits to return")
	return cmd
}

func newSessionsWhyCmd(cfg config.Config) *cobra.Command {
	var (
		dsn     string
		project string
		limit   int
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "why <path|topic>",
		Short: "Recent sessions whose runbooks explain a path or topic, newest first",
		Long: `why answers "which predecessor sessions touched this, and why" by searching
the mart's runbook narrative corpus (field reports, handoffs, plans) for
<path|topic> and reporting the sessions that authored the matches, newest
first: session id, date, repo, model, the linking runbook, and a snippet.

Honest degradations — the mart ingests no per-file edit history, so this is a
NARRATIVE lookup, not a VCS touch history:
  - <path|topic> is matched against runbook TEXT. A literal path matches only
    where that path string appears in a runbook (the trigram fallback carries
    it) — there is no per-session list of edited files to match against.
  - "intent" is the matching runbook's title; "key decisions" is a text
    snippet around the match. The mart has no dedicated intent or decisions
    field.
  - the link is a local corpus-relative runbook path, not a URL.
  - a session appears only when a runbook carries its session_id; a session
    that left no such runbook is invisible here — use
    'forgectl sessions last <repo>' to see it.

  forgectl sessions why "worktree guard"
  forgectl sessions why internal/cli/sessions.go --project forgectl
  forgectl sessions why "colima" --json | jq .`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMart(cmd, dsn, cfg.Sessions, func(mart *sessions.Mart) error {
				hits, err := mart.WhySessions(cmd.Context(), args[0], project, limit)
				if err != nil {
					return err
				}
				// Degradation note to stderr keeps stdout clean for --json pipes.
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: narrative lookup over the runbook corpus — intent is a runbook title, "+
						"key decisions a text snippet; the mart indexes no per-file edit history")
				return printWhyHits(cmd, hits, asJSON)
			})
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "mart DSN (default: FORGECTL_SESSIONS_DSN, then [sessions] dsn)")
	cmd.Flags().StringVar(&project, "project", "", "restrict matches to one repo (exact)")
	cmd.Flags().IntVar(&limit, "limit", 10, "maximum sessions to return")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON to stdout (stable; notes go to stderr)")
	return cmd
}

func newSessionsLastCmd(cfg config.Config) *cobra.Command {
	var (
		dsn    string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "last <repo>",
		Short: "The most recent session in a repo and the artifacts it left behind",
		Long: `last reports the newest session (by end time) recorded for <repo> in the
mart, plus the runbook artifacts it authored — the closest signal of its
sign-off state.

Honest degradations:
  - <repo> matches the session's project EXACTLY.
  - the mart has no explicit outro/lifecycle flag. "state" is inferred:
    'committed' reports whether the session produced commits, and the listed
    artifacts are the runbooks it authored (a handoff or field-report among
    them is the sign it wrapped up cleanly). No artifacts means it left no
    narrative.

  forgectl sessions last forgectl
  forgectl sessions last cadence --json | jq .`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMart(cmd, dsn, cfg.Sessions, func(mart *sessions.Mart) error {
				summary, err := mart.LastSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: sign-off state is inferred from commits + authored runbooks; "+
						"the mart has no explicit outro flag")
				return printLastSession(cmd, args[0], summary, asJSON)
			})
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "mart DSN (default: FORGECTL_SESSIONS_DSN, then [sessions] dsn)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON to stdout (stable; notes go to stderr)")
	return cmd
}

// whyDTO is the stable --json shape for `sessions why`, decoupled from the
// internal WhyHit so the CLI contract doesn't drift with the query struct.
type whyDTO struct {
	SessionID    string `json:"session_id"`
	Date         string `json:"date,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Model        string `json:"model,omitempty"`
	Committed    bool   `json:"committed"`
	RunbookType  string `json:"runbook_type,omitempty"`
	Intent       string `json:"intent,omitempty"`
	Link         string `json:"link,omitempty"`
	KeyDecisions string `json:"key_decisions,omitempty"`
}

// printWhyHits renders `sessions why` results. Both paths strip control bytes
// from mart-sourced fields: encoding/json only escapes 0x00–0x1F, so DEL and
// the C1 range (0x80–0x9F, including 0x9B = single-byte CSI) would otherwise
// reach a terminal raw through --json. sanitizeTerm's unicode.IsControl check
// catches them; the JSON path must call it explicitly.
func printWhyHits(cmd *cobra.Command, hits []sessions.WhyHit, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		dto := make([]whyDTO, 0, len(hits))
		for _, h := range hits {
			dto = append(dto, whyDTO{
				SessionID: sanitizeTerm(h.SessionID), Date: fmtTs(h.LastTs), Repo: sanitizeTerm(h.Project),
				Model: sanitizeTerm(h.Model), Committed: h.Committed, RunbookType: sanitizeTerm(h.Type),
				Intent: sanitizeTerm(h.Title), Link: sanitizeTerm(h.Path), KeyDecisions: sanitizeTerm(h.Snippet),
			})
		}
		if len(hits) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "Found %d sessions\n", len(hits))
		}
		return writeJSON(out, dto)
	}
	if len(hits) == 0 {
		fmt.Fprintln(out, "no sessions matched")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(out, "%s\t%s\t[%s]\t%s\n",
			sanitizeTerm(h.SessionID), humanTs(h.LastTs), sanitizeTerm(h.Project), sanitizeTerm(h.Model))
		fmt.Fprintf(out, "\t%s · %s\n", sanitizeTerm(h.Type), sanitizeTerm(h.Title))
		fmt.Fprintf(out, "\t%s\n", sanitizeTerm(h.Path))
		fmt.Fprintf(out, "\t%s\n", sanitizeTerm(h.Snippet))
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Found %d sessions\n", len(hits))
	return nil
}

// artifactDTO / lastDTO are the stable --json shapes for `sessions last`.
type artifactDTO struct {
	Type  string `json:"type,omitempty"`
	Title string `json:"title,omitempty"`
	Path  string `json:"path"`
}

type lastDTO struct {
	SessionID string        `json:"session_id"`
	Repo      string        `json:"repo,omitempty"`
	Branch    string        `json:"branch,omitempty"`
	Model     string        `json:"model,omitempty"`
	Machine   string        `json:"machine,omitempty"`
	FirstTs   string        `json:"first_ts,omitempty"`
	LastTs    string        `json:"last_ts,omitempty"`
	Committed bool          `json:"committed"`
	Artifacts []artifactDTO `json:"artifacts"`
}

// printLastSession renders `sessions last`. A repo with no session is a clean
// miss: null in --json (any jq can test it), a friendly line otherwise.
// Mart-sourced fields are control-byte-stripped on both paths (see printWhyHits
// for why the JSON encoder alone is insufficient).
func printLastSession(cmd *cobra.Command, repo string, s *sessions.SessionSummary, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if s == nil {
			return writeJSON(out, nil)
		}
		arts := make([]artifactDTO, 0, len(s.Artifacts))
		for _, a := range s.Artifacts {
			arts = append(arts, artifactDTO{Type: sanitizeTerm(a.Type), Title: sanitizeTerm(a.Title), Path: sanitizeTerm(a.Path)})
		}
		return writeJSON(out, lastDTO{
			SessionID: sanitizeTerm(s.SessionID), Repo: sanitizeTerm(s.Project), Branch: sanitizeTerm(s.GitBranch), Model: sanitizeTerm(s.Model),
			Machine: sanitizeTerm(s.Machine), FirstTs: fmtTs(s.FirstTs), LastTs: fmtTs(s.LastTs),
			Committed: s.Committed, Artifacts: arts,
		})
	}
	if s == nil {
		fmt.Fprintf(out, "no sessions recorded for %q\n", sanitizeTerm(repo))
		return nil
	}
	committed := "no commits"
	if s.Committed {
		committed = "committed"
	}
	fmt.Fprintf(out, "%s\t%s\t[%s]\t%s\t%s\n",
		sanitizeTerm(s.SessionID), humanTs(s.LastTs), sanitizeTerm(s.Project), sanitizeTerm(s.GitBranch), committed)
	if s.Model != "" || s.Machine != "" {
		fmt.Fprintf(out, "\t%s on %s\n", sanitizeTerm(s.Model), sanitizeTerm(s.Machine))
	}
	if len(s.Artifacts) == 0 {
		fmt.Fprintln(out, "\tno field report or handoff recorded")
		return nil
	}
	for _, a := range s.Artifacts {
		fmt.Fprintf(out, "\t%s · %s\n\t  %s\n",
			sanitizeTerm(a.Type), sanitizeTerm(a.Title), sanitizeTerm(a.Path))
	}
	return nil
}

// fmtTs renders a nullable mart timestamp as RFC3339 UTC, or "" when the
// ledger carried no timestamp — omitempty then drops it from --json.
func fmtTs(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// humanTs is fmtTs for the terminal: an absent timestamp reads as "unknown"
// rather than a blank column.
func humanTs(t *time.Time) string {
	if s := fmtTs(t); s != "" {
		return s
	}
	return "unknown"
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
