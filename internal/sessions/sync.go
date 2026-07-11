package sessions

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// SyncOptions parameterizes one ETL run. Zero-valued fields fall back to the
// [sessions] config section, then to built-in defaults (paths under ~/.claude,
// machine = short hostname). DSN has no built-in default — it comes from
// --dsn, FORGECTL_SESSIONS_DSN, or config, resolved in the CLI layer.
type SyncOptions struct {
	DSN         string
	Machine     string
	MetricsDir  string
	RunbooksDir string
	// SyncthingConfig overrides the discovered Syncthing config.xml path
	// (tests, non-standard installs). Empty = platform default discovery.
	SyncthingConfig string
	DryRun          bool // read + transform + count; no DB connection
	Full            bool // bypass the lastMessageId watermark, re-upsert everything
}

// Receipt is the completeness accounting a sync prints — the contract that a
// silently-skipped session surfaces as MISSING instead of being swallowed.
type Receipt struct {
	SessionsFound     int
	SessionsUpserted  int
	SessionsUnchanged int      // watermark matched; skipped as already-synced
	InvalidRows       int      // sessions.jsonl rows with no sessionId — cannot index
	CommitRowsDropped int      // commits.jsonl rows with no session id — cannot attribute
	LedgerLinesBad    int      // unparseable JSONL lines across both ledgers
	Missing           []string // local sessions absent from the mart post-flush
	RunbooksFound     int
	RunbooksUpserted  int
	RunbooksPruned    int64
	DryRun            bool
}

// Complete reports whether the flush reconciled fully.
func (r *Receipt) Complete() bool { return len(r.Missing) == 0 }

// Resolve fills unset options from config, then built-in defaults.
func (o SyncOptions) Resolve(cfg config.SessionsConfig) (SyncOptions, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return o, fmt.Errorf("resolve home directory: %w", err)
	}
	if o.DSN == "" {
		o.DSN = cfg.DSN
	}
	if o.Machine == "" {
		o.Machine = cfg.Machine
	}
	if o.Machine == "" {
		host, hostErr := os.Hostname()
		if hostErr != nil {
			return o, fmt.Errorf("resolve machine name from hostname: %w", hostErr)
		}
		// Short label: "cameron-m5-mbp.local" -> "cameron-m5-mbp".
		o.Machine = strings.SplitN(host, ".", 2)[0]
	}
	if o.MetricsDir == "" {
		o.MetricsDir = cfg.MetricsDir
	}
	if o.MetricsDir == "" {
		o.MetricsDir = filepath.Join(home, ".claude", "metrics")
	}
	if o.RunbooksDir == "" {
		o.RunbooksDir = cfg.RunbooksDir
	}
	if o.RunbooksDir == "" {
		o.RunbooksDir = filepath.Join(home, ".claude", "cadence", "runbooks")
	}
	return o, nil
}

// Sync is one idempotent ETL run: drain the local JSONL WAL into the mart's
// session index and rebuild the runbook full-text index from the markdown
// corpus. Read-only against every local file; a failed or killed run leaves
// the WAL intact for the next drain.
func Sync(ctx context.Context, opts SyncOptions) (*Receipt, error) {
	slog.Info("Preparing sessions sync.", "metrics_dir", opts.MetricsDir,
		"runbooks_dir", opts.RunbooksDir, "machine", opts.Machine, "dry_run", opts.DryRun)

	if err := enforceSyncthingGuard(opts); err != nil {
		return nil, err
	}

	sessionRows, commitRows, receipt, err := extract(opts)
	if err != nil {
		slog.Error("Encountered ledger read failure while extracting the WAL.", "error", err, "metrics_dir", opts.MetricsDir)
		return nil, err
	}
	att := RootCostMap(commitRows)
	rows, invalid := BuildSessions(sessionRows, att, opts.Machine)
	receipt.SessionsFound = len(rows)
	receipt.InvalidRows = invalid
	receipt.CommitRowsDropped = att.Dropped

	runbooks, err := ScanRunbooks(opts.RunbooksDir, opts.Machine)
	if err != nil {
		slog.Error("Encountered corpus scan failure while indexing runbooks.", "error", err, "runbooks_dir", opts.RunbooksDir)
		return nil, err
	}
	receipt.RunbooksFound = len(runbooks)

	if opts.DryRun {
		receipt.DryRun = true
		slog.Info("Dry-run complete, no database connection made.",
			"sessions", receipt.SessionsFound, "runbooks", receipt.RunbooksFound)
		return receipt, nil
	}
	if opts.DSN == "" {
		return nil, fmt.Errorf("no mart DSN: set [sessions] dsn in config, FORGECTL_SESSIONS_DSN, or --dsn (or use --dry-run)")
	}

	mart, err := ConnectMart(ctx, opts.DSN)
	if err != nil {
		slog.Error("Failed to reach the mart — the JSONL WAL is untouched and drains on the next reachable run.", "error", err)
		return nil, err
	}
	defer mart.Close(ctx)

	toUpsert := rows
	if !opts.Full {
		toUpsert, receipt.SessionsUnchanged, err = skipUnchanged(ctx, mart, rows)
		if err != nil {
			slog.Error("Encountered watermark query failure while partitioning sessions.", "error", err, "sessions", len(rows))
			return nil, err
		}
	}
	if err := mart.UpsertSessions(ctx, toUpsert); err != nil {
		slog.Error("Encountered upsert failure while flushing sessions — nothing partial is committed.", "error", err, "attempted", len(toUpsert))
		return nil, err
	}
	receipt.SessionsUpserted = len(toUpsert)

	// Reconcile: every local session must now exist in the mart. Anything
	// absent is a MISSING line, never a silent skip.
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.SessionID
	}
	present, err := mart.PresentIDs(ctx, ids)
	if err != nil {
		slog.Error("Encountered reconcile failure while verifying flushed sessions.", "error", err, "sessions", len(ids))
		return nil, err
	}
	for _, id := range ids {
		if !present[id] {
			receipt.Missing = append(receipt.Missing, id)
		}
	}
	if len(receipt.Missing) > 0 {
		slog.Error("Reconcile found sessions absent from the mart after flush — expected every local session present.", "missing", len(receipt.Missing))
	}

	receipt.RunbooksPruned, err = mart.UpsertRunbooks(ctx, runbooks)
	if err != nil {
		slog.Error("Encountered runbook index failure while rebuilding the full-text index.", "error", err, "runbooks", len(runbooks))
		return nil, err
	}
	receipt.RunbooksUpserted = len(runbooks)

	slog.Info("Successfully completed sessions sync.",
		"found", receipt.SessionsFound, "upserted", receipt.SessionsUpserted,
		"unchanged", receipt.SessionsUnchanged, "missing", len(receipt.Missing),
		"runbooks", receipt.RunbooksUpserted, "pruned", receipt.RunbooksPruned)
	return receipt, nil
}

// enforceSyncthingGuard runs the Syncthing-blobs-only check on every sync
// (dry-run included — it needs no DB). A folder covering a JSONL root fails
// the sync loudly: proceeding would index a WAL that Syncthing is about to
// fork into .sync-conflict-* divergence. The guard's own faults (no config,
// unparseable config) fail open with a warning — a broken check must not
// block the operation it protects.
func enforceSyncthingGuard(opts SyncOptions) error {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("Syncthing guard could not resolve home directory — proceeding unguarded.", "error", err)
		return nil
	}
	configPath := opts.SyncthingConfig
	if configPath == "" {
		configPath = DefaultSyncthingConfigPath(home)
	}
	if configPath == "" {
		slog.Debug("No Syncthing config on this machine — nothing syncs, guard passes.")
		return nil
	}
	violations, err := CheckSyncthingFolders(configPath, home)
	if err != nil {
		slog.Warn("Syncthing guard could not inspect the config — proceeding unguarded; verify by hand.", "error", err, "config", configPath)
		return nil
	}
	if len(violations) > 0 {
		slog.Error("Syncthing is carrying a JSONL ledger root — refusing to sync until the share is removed.", "violations", len(violations), "config", configPath)
		return fmt.Errorf("syncthing-blobs-only guard: %s (JSONL is the WAL — a synced ledger silently forks into .sync-conflict-* divergence; remove the share, then re-run)", strings.Join(violations, "; "))
	}
	slog.Debug("Syncthing guard passed — no folder covers a JSONL root.", "config", configPath)
	return nil
}

// extract reads both JSONL ledgers. Split out so the WAL-read half is
// obviously connection-free (the --dry-run guarantee).
func extract(opts SyncOptions) (sessionRows, commitRows []LedgerRow, receipt *Receipt, err error) {
	receipt = &Receipt{}
	var skipped int
	sessionRows, skipped, err = ReadLedger(filepath.Join(opts.MetricsDir, "sessions.jsonl"))
	if err != nil {
		return nil, nil, nil, err
	}
	receipt.LedgerLinesBad += skipped
	commitRows, skipped, err = ReadLedger(filepath.Join(opts.MetricsDir, "commits.jsonl"))
	if err != nil {
		return nil, nil, nil, err
	}
	receipt.LedgerLinesBad += skipped
	return sessionRows, commitRows, receipt, nil
}

// skipUnchanged partitions rows by the mart's lastMessageId watermark: a
// session whose cursor already matches is an already-synced tail. The
// watermark is an optimization only — matching rows are skipped, everything
// else re-upserts idempotently on session_id.
func skipUnchanged(ctx context.Context, mart *Mart, rows []SessionRow) (toUpsert []SessionRow, unchanged int, err error) {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.SessionID
	}
	watermarks, err := mart.Watermarks(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	toUpsert = make([]SessionRow, 0, len(rows))
	for _, r := range rows {
		if wm, ok := watermarks[r.SessionID]; ok && wm != "" && wm == r.LastMessageID {
			unchanged++
			continue
		}
		toUpsert = append(toUpsert, r)
	}
	return toUpsert, unchanged, nil
}
