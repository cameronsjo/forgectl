package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface merge <name>` merges a worker's PR when the merge policy passes,
// and `surface audit --pr` prints a PR's merge-audit.jsonl lines (atelier P4,
// T10.4). Both the CLI and the drain's autopilot merge through merge.Land.

// mergeTimeout bounds one merge: the reads, the re-read, gh pr merge and the
// landing check.
const mergeTimeout = 3 * time.Minute

// mergeView is `surface merge --json`. Additive changes only (ADR-0008 rule
// 2).
type mergeView struct {
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	DryRun bool   `json:"dry_run"`
	Mode   string `json:"mode"`
	merge.Outcome
}

type mergeOptions struct {
	Repo   string
	Name   string
	DryRun bool
	JSON   bool
}

// landFunc is merge.Lander.Land with its I/O already wired.
type landFunc func(ctx context.Context, s config.MergeSettings, by merge.By, row merge.Row, dryRun bool) merge.Outcome

// mergeConfirmer shows a person the merge `surface merge` is about to make
// and returns nil only when that person typed "yes" at a terminal. stdin is
// the command's input, which must itself be a terminal.
type mergeConfirmer func(stdin io.Reader, c merge.Confirmation) error

// mergeDeps are `surface merge`'s seams. newSurfaceMergeCmd fills confirm
// with confirmMergeAtTerminal and getenv with os.Getenv; tests build the
// command with their own. No flag or variable replaces the confirmer, so
// production has no path that skips it.
type mergeDeps struct {
	rows     func(ctx context.Context, warn io.Writer, repo, name string) (worker.Row, *worker.QueueRow, error)
	settings func() config.MergeSettings
	// land returns the merge path, asking confirm before a merge (as
	// merge.Lander.Confirm), or why its state cannot be opened.
	land    func(confirm func(context.Context, merge.Confirmation) error) (landFunc, error)
	confirm mergeConfirmer
	// getenv is the environment lookup the drain-worker refusal reads.
	getenv func(string) string
}

func newSurfaceMergeCmd(deps module.Deps) *cobra.Command {
	return newSurfaceMergeCmdWith(mergeDeps{
		rows: func(ctx context.Context, warn io.Writer, repo, name string) (worker.Row, *worker.QueueRow, error) {
			return statusRows(ctx, warn, deps, repo, name)
		},
		settings: localMergeSettings,
		land: func(confirm func(context.Context, merge.Confirmation) error) (landFunc, error) {
			l, err := realLander(deps.Runner)
			if err != nil {
				return nil, err
			}
			l.Confirm = confirm
			return l.Land, nil
		},
		confirm: confirmMergeAtTerminal,
		getenv:  os.Getenv,
	})
}

func newSurfaceMergeCmdWith(d mergeDeps) *cobra.Command {
	opts := mergeOptions{}
	cmd := &cobra.Command{
		Use:   "merge <name>",
		Short: "Merge a worker's PR when the merge policy passes",
		Long: `merge squash-merges the named worker's pull request when the merge policy
([surface.merge], ADR-0011) passes, and records an audit line either way.

The policy is resolved from the config file and GitHub is read again on every
call, with no cache: the PR is found as surface status finds it, and the
verdict must pass with mode manual or auto.

A person confirms every merge: "manual" means a person at a terminal, and
the drain's autopilot (mode auto) is the one unattended merge path. Once the
verdict passes and the subject is composed, merge shows the PR, its head,
the required check runs and reviewer markers counted, and the subject on
the terminal, and goes on only if "yes" is typed there; the re-read below
runs after the answer. The answer is read from /dev/tty, never from stdin,
and stdin must be a terminal as well, so a piped "yes" does not count. No
terminal, end of input, or any other answer merges nothing and is audited
as a refusal. No flag skips this; --dry-run asks nothing. The 3-minute cap
on one merge includes the wait at the question, so an answer after it
merges nothing. merge refuses to run at all, before reading GitHub, when
FORGECTL_DRAIN_WORKER is set, as it is in every drain worker.

The PR title becomes the commit
subject only when it matches
^(fix|feat|docs|refactor|test|chore)(\([a-z0-9-]+\))?: [ -~]{1,72}$ and holds
no issue reference, issue or PR link, or CI-skip directive ([skip ci] and
the like); otherwise the merge refuses. Just before merging, the PR's
head, base branch, draft flag, state, reviews, comments and required check
runs are read again, and any change refuses. forgectl then writes an audit line and runs
gh pr merge <n> -R github.com/<owner>/<repo> --squash
--match-head-commit <head> --subject <title> --body <body> from a new
temporary directory, never --admin. The body is forgectl's own: the audit
line's hash, the policy hash, the head, the checks and review markers
counted, and a Merged-By: forgectl-cli trailer; no PR text is copied into
it. After the merge, forgectl reads the PR back, on a 30-second timeout of
its own, and records merged only when GitHub says it merged, its merge
commit is on the default branch, and the commit's message carries this
attempt's Audit-line. A merge queue, if the branch has one, makes the
result merged-unconfirmed.

Every merge writes two lines to merge-audit.jsonl in the state directory (the
attempt, whose hash the commit body carries, and the outcome), and every
refusal one, unless the latest line for the same actor and PR (or worker,
with no PR) is the same refusal (head, reasons and policy). A merge starts
only with room in the file for its attempt line and 8 KiB for its outcome
line; otherwise it is refused. --dry-run reads and decides and writes nothing, not even an
audit line.

--json prints {"name","repo","dry_run","mode","result","pr","url","head",
"reasons","merge_commit","audit_line","audit_note"}; result is merged,
would-merge, refused, unreadable, merge-failed, merged-unconfirmed,
merged-elsewhere (GitHub says merged, by something else) or merge-unknown
(gh failed and GitHub could not be read after it).

Exit 0: merged, or a dry run that would merge. Exit 1: refused (the answer
not "yes" included), GitHub could not be read, the merge failed, could not
be confirmed, was made by something else, or is not known; the message says
which. Exit 2: a usage or setup error (no such worker, a row that records
no GitHub repository, no terminal to confirm at, run inside a drain worker).

  forgectl surface merge gh1175-forgectl --dry-run
  forgectl surface merge fix-login --repo forgectl`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceMerge(cmd, d, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "read and decide; merge nothing and write no audit line")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","repo","dry_run","mode","result","pr","url","head","reasons","merge_commit","audit_line","audit_note"} as JSON`)
	return cmd
}

func runSurfaceMerge(cmd *cobra.Command, d mergeDeps, opts mergeOptions) error {
	getenv := d.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// First of all, so a worker that runs merge reads nothing from GitHub.
	if err := refuseInDrainWorker(getenv, "surface merge"); err != nil {
		return err
	}
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	row, qrow, err := d.rows(cmd.Context(), cmd.ErrOrStderr(), opts.Repo, opts.Name)
	if err != nil {
		return err
	}
	if row.GitHubRepo == "" || row.GitHubRepoID == 0 {
		return WithExitCode(fmt.Errorf("worker %q: %w; it was launched before forgectl recorded one, or its origin is not on github.com", opts.Name, merge.ErrNoRecordedRepo), exitUsage)
	}
	// confirmErr is the confirmer's answer, kept for the exit code: no
	// terminal is a setup error (exit 2), any other answer a refusal.
	var confirmErr error
	confirm := func(_ context.Context, c merge.Confirmation) error {
		if d.confirm == nil {
			confirmErr = WithExitCode(errMergeNoTerminal, exitUsage)
		} else {
			confirmErr = d.confirm(cmd.InOrStdin(), c)
		}
		return confirmErr
	}
	land, err := d.land(confirm)
	if err != nil {
		return WithExitCode(termsafe.Error(fmt.Errorf("open the merge audit file: %w", err)), exitFailed)
	}
	settings := d.settings()
	ctx, cancel := context.WithTimeout(cmd.Context(), mergeTimeout)
	defer cancel()
	out := land(ctx, settings, merge.ByCLI, mergeRow(row, qrow), opts.DryRun)
	view := mergeView{Name: row.Name, Repo: row.GitHubRepo, DryRun: opts.DryRun, Mode: string(settings.Mode), Outcome: out}
	if view.Reasons == nil {
		view.Reasons = []string{}
	}
	ok := out.Result == merge.LandMerged || out.Result == merge.LandWouldMerge
	declined := out.Result == merge.LandRefused && confirmErr != nil
	if opts.JSON {
		if err := writeJSON(cmd.OutOrStdout(), view); err != nil {
			return err
		}
		switch {
		case declined:
			return newSilentCodedError(ExitCode(confirmErr))
		case !ok:
			return newSilentCodedError(exitFailed)
		}
		return nil
	}
	if err := renderMerge(cmd.OutOrStdout(), view); err != nil {
		return err
	}
	switch {
	case ok:
		return nil
	case declined:
		return WithExitCode(fmt.Errorf("worker %s: %w", termsafe.SafeLineMax(row.Name, 64), confirmErr), ExitCode(confirmErr))
	}
	return WithExitCode(fmt.Errorf("worker %s: %s", termsafe.SafeLineMax(row.Name, 64), mergeFailureSentence(out)), exitFailed)
}

// errMergeNoTerminal refuses a merge nobody can confirm.
var errMergeNoTerminal = errors.New("surface merge needs a person at a terminal to confirm; run it from a plain terminal, or use --dry-run (the drain's autopilot, with mode auto, is the one unattended merge path)")

// errMergeNotConfirmed reports an answer other than "yes".
var errMergeNotConfirmed = errors.New(`surface merge: the answer was not "yes"; nothing was merged`)

// confirmMergeAtTerminal is the production confirmer: stdin must be a
// terminal, /dev/tty must open as one, and the answer is read from /dev/tty
// (terminalConfirm).
func confirmMergeAtTerminal(stdin io.Reader, c merge.Confirmation) error {
	return productionTerminal().confirm(stdin, errMergeNoTerminal, func(in io.Reader, out io.Writer) error {
		return askMerge(in, out, c)
	})
}

// askMerge writes the merge and the question to out and reads one line from
// in. Only a complete line that is exactly "yes", once surrounding space is
// trimmed, confirms.
func askMerge(in io.Reader, out io.Writer, c merge.Confirmation) error {
	if err := writeMergeConfirmation(out, c); err != nil {
		return err
	}
	if _, err := fmt.Fprint(out, `Type "yes" to merge it, anything else to cancel: `); err != nil {
		return err
	}
	if !readYes(in) {
		return WithExitCode(errMergeNotConfirmed, exitFailed)
	}
	return nil
}

// writeMergeConfirmation shows the PR, its head, the verdict's evidence and
// the commit subject. Every field read from GitHub goes through termsafe.
func writeMergeConfirmation(out io.Writer, c merge.Confirmation) error {
	var b strings.Builder
	fmt.Fprintf(&b, "surface merge would squash-merge %s#%d (%s)\n", termsafe.SafeLineMax(c.Repo, 120), c.PR, termsafe.SafeLineMax(c.URL, 200))
	fmt.Fprintf(&b, "  head %s; the merge policy passes\n", termsafe.SafeLineMax(c.Head, 40))
	fmt.Fprintf(&b, "  commit subject: %s\n", termsafe.SafeLineMax(c.Subject, 120))
	for _, ch := range c.Checks {
		fmt.Fprintf(&b, "  check %s (run %d, %s): %s/%s\n", termsafe.SafeLineMax(ch.Name, 80), ch.RunID, termsafe.SafeLineMax(ch.Event, 40),
			termsafe.SafeLineMax(ch.Status, 40), termsafe.SafeLineMax(ch.Conclusion, 40))
	}
	for _, m := range c.Markers {
		fmt.Fprintf(&b, "  marker %s crit=%d imp=%d at head %s: %s\n", termsafe.SafeLineMax(m.Reviewer, 40), m.Crit, m.Imp,
			termsafe.SafeLineMax(shortSHA(m.Head), 12), termsafe.SafeLineMax(m.URL, 200))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// mergeFailureSentence says which kind of failure an outcome is.
func mergeFailureSentence(out merge.Outcome) string {
	switch out.Result {
	case merge.LandRefused:
		return fmt.Sprintf("merge refused (%d reasons above); nothing was merged", len(out.Reasons))
	case merge.LandUnreadable:
		return "GitHub could not be read; nothing was merged"
	case merge.LandFailed:
		return "the merge failed; nothing was merged"
	case merge.LandUnconfirmed:
		return "the merge could not be confirmed as this attempt's on the default branch; check it by hand"
	case merge.LandMergedElsewhere:
		return "GitHub says the PR merged, but not by this attempt: its merge commit does not carry this attempt's audit line"
	case merge.LandUnknown:
		return "gh pr merge failed and GitHub could not be read after it; the PR may have merged: check it by hand"
	}
	return "unexpected merge result " + strconv.Quote(out.Result)
}

func renderMerge(w io.Writer, v mergeView) error {
	safe := func(s string) string { return termsafe.SafeLineMax(s, maxStatusText) }
	var b strings.Builder
	switch {
	case v.PR > 0:
		fmt.Fprintf(&b, "%s  %s#%d head %s: %s\n", safe(v.Name), safe(v.Repo), v.PR, safe(shortSHA(v.Head)), v.Result)
	default:
		fmt.Fprintf(&b, "%s  %s: %s\n", safe(v.Name), safe(v.Repo), v.Result)
	}
	for _, r := range v.Reasons {
		fmt.Fprintf(&b, "  - %s\n", termsafe.SafeLineMax(r, 400))
	}
	if v.MergeCommit != "" {
		fmt.Fprintf(&b, "merge commit: %s\n", safe(v.MergeCommit))
	}
	if v.AuditLine != "" {
		fmt.Fprintf(&b, "audit line: sha256:%s\n", safe(v.AuditLine))
	}
	if v.AuditNote != "" {
		fmt.Fprintf(&b, "note: %s\n", safe(v.AuditNote))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// realLander wires merge.Land: GitHub through the host-pinned runner with no
// cache, merge-audit.jsonl in the state directory, and gh pr merge from a
// new temporary directory.
func realLander(run exec.Runner) (merge.Lander, error) {
	audit, err := worker.OpenMergeAudit()
	if err != nil {
		return merge.Lander{}, err
	}
	gh := githubauth.Runner(run, githubauth.DefaultHost)
	// No cache: a merge reads everything from GitHub.
	r := merge.Reader{GH: gh}
	return merge.Lander{
		Read: r.Read, Recheck: r.Recheck, Landed: r.Landed,
		Merge: func(ctx context.Context, args []string) error {
			return ghInNeutralDir(ctx, gh, args, os.MkdirTemp, os.RemoveAll)
		},
		Audit:    audit.Append,
		AuditCap: worker.MaxMergeAuditBytes,
		Now:      time.Now,
		Sleep:    sleepCtx,
	}, nil
}

// errNoDirRunner reports a runner that cannot run gh in a chosen directory.
var errNoDirRunner = errors.New("forgectl: the runner cannot run gh in a chosen directory; refusing to merge from the current one")

// ghInNeutralDir runs gh with args from a new private temporary directory,
// so gh reads no checkout (a worker's worktree included), and removes the
// directory after. run must implement exec.DirRunner.
func ghInNeutralDir(ctx context.Context, run exec.Runner, args []string, mkdirTemp func(dir, pattern string) (string, error), removeAll func(string) error) error {
	dr, ok := run.(exec.DirRunner)
	if !ok {
		return errNoDirRunner
	}
	dir, err := mkdirTemp("", "forgectl-merge-")
	if err != nil {
		return fmt.Errorf("forgectl: a temporary directory for gh pr merge: %w", err)
	}
	defer removeAll(dir) //nolint:errcheck // an empty temporary directory left behind costs nothing
	_, err = dr.RunWithEnvFilteredInDir(ctx, dir, nil, nil, "gh", args...)
	return err
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// auditPRPattern is `surface audit --pr`'s argument: owner/repo#N.
var auditPRPattern = regexp.MustCompile(`^([A-Za-z0-9._-]{1,100}/[A-Za-z0-9._-]{1,100})#([1-9][0-9]{0,9})$`)

// auditView is `surface audit --json`. Additive changes only.
type auditView struct {
	PR    string          `json:"pr"`
	Lines []auditViewLine `json:"lines"`
	Chain auditChain      `json:"chain"`
}

type auditViewLine struct {
	Line int    `json:"line"`
	Hash string `json:"hash"`
	merge.AuditLine
}

type auditChain struct {
	OK    bool              `json:"ok"`
	Lines int               `json:"lines"`
	Break *merge.ChainBreak `json:"break"`
}

func newSurfaceAuditCmd(_ module.Deps) *cobra.Command {
	return newSurfaceAuditCmdWith(func() ([]byte, error) {
		a, err := worker.OpenMergeAudit()
		if err != nil {
			return nil, err
		}
		return a.Read()
	})
}

func newSurfaceAuditCmdWith(read func() ([]byte, error)) *cobra.Command {
	var prArg string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "audit --pr <owner/repo#N>",
		Short: "Print a PR's merge-audit lines and check the audit chain",
		Long: `audit prints the merge-audit.jsonl lines for one pull request, written by
surface merge and the drain's autopilot, and checks the hash chain over the
whole file: each line names the SHA-256 of the line before it. The first
place the chain does not hold is reported.

The chain detects truncation (a write cut short) and accidental damage to
earlier lines (one edited or removed by mistake). It does not detect an edit
to the last line, or removing the last lines whole, since no line after them
names their hash; nor a deliberate rewrite by your own user, who can rewrite
the whole file and every hash in it. Each merge's commit body on the default
branch carries its attempt line's hash, which a rewritten file cannot
change.

--json prints {"pr","lines":[{"line","hash","time","actor","worker","repo",
"repo_id","pr","head","policy_hash","checks","markers","result","reasons",
"merge_commit","attempt","prev"}],"chain":{"ok","lines","break"}}; break is
{"line","reason"} or null.

Exit 0: printed, and the chain holds. Exit 1: the file could not be read, or
the chain is broken. Exit 2: a usage error.

  forgectl surface audit --pr cameronsjo/forgectl#1204`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m := auditPRPattern.FindStringSubmatch(prArg)
			if m == nil {
				return WithExitCode(fmt.Errorf("--pr: want owner/repo#N, got %s", strconv.Quote(termsafe.SafeLineMax(prArg, 120))), exitUsage)
			}
			number, err := strconv.Atoi(m[2])
			if err != nil {
				return WithExitCode(fmt.Errorf("--pr: %w", err), exitUsage)
			}
			data, err := read()
			if err != nil {
				return WithExitCode(termsafe.Error(fmt.Errorf("read merge-audit.jsonl: %w", err)), exitFailed)
			}
			entries, brk := merge.ParseAudit(data)
			view := auditView{PR: prArg, Lines: []auditViewLine{}, Chain: auditChain{OK: brk == nil, Lines: len(entries), Break: brk}}
			for _, e := range entries {
				if e.OK && strings.EqualFold(e.Line.Repo, m[1]) && e.Line.PR == number {
					view.Lines = append(view.Lines, auditViewLine{Line: e.N, Hash: e.Hash, AuditLine: e.Line})
				}
			}
			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), view); err != nil {
					return err
				}
			} else if err := renderAudit(cmd.OutOrStdout(), view); err != nil {
				return err
			}
			if brk != nil {
				if asJSON {
					return newSilentCodedError(exitFailed)
				}
				return WithExitCode(fmt.Errorf("merge-audit.jsonl: the chain is broken at line %d", brk.Line), exitFailed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&prArg, "pr", "", "the pull request, as owner/repo#N")
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"pr","lines","chain"} as JSON`)
	return cmd
}

func renderAudit(w io.Writer, v auditView) error {
	safe := func(s string) string { return termsafe.SafeLineMax(s, maxStatusText) }
	var b strings.Builder
	if len(v.Lines) == 0 {
		fmt.Fprintf(&b, "no audit lines for %s\n", safe(v.PR))
	}
	for _, l := range v.Lines {
		fmt.Fprintf(&b, "line %d  %s  %s  %s  head %s", l.Line, safe(l.Time), safe(string(l.Actor)), safe(l.Result), safe(shortSHA(l.Head)))
		if l.MergeCommit != "" {
			fmt.Fprintf(&b, "  merge %s", safe(shortSHA(l.MergeCommit)))
		}
		fmt.Fprintf(&b, "  hash %s\n", safe(shortSHA(l.Hash)))
		for _, r := range l.Reasons {
			fmt.Fprintf(&b, "  - %s\n", termsafe.SafeLineMax(r, 400))
		}
	}
	if v.Chain.Break != nil {
		fmt.Fprintf(&b, "chain: broken at line %d of %d: %s\n", v.Chain.Break.Line, v.Chain.Lines, termsafe.SafeLineMax(v.Chain.Break.Reason, 400))
	} else {
		fmt.Fprintf(&b, "chain: holds over %d lines\n", v.Chain.Lines)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
