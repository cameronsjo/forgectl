package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	osexec "os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface status <name>` shows a worker's PR, its checks, the review
// markers at its head, the merge policy's verdict with reasons, and the
// session's cost (atelier P4, T10.2). It reads GitHub and writes nothing but
// its cache of head-immutable file data.

const (
	// statusTimeout bounds every GitHub read status makes.
	statusTimeout = 2 * time.Minute
	// usageTimeout bounds the cadence-hooks pricing run.
	usageTimeout = 15 * time.Second
	// maxStatusText caps GitHub text echoed to the terminal.
	maxStatusText = 200
)

// workerStatusView is `surface status --json`. Additive changes only (ADR-0008
// rule 2).
type workerStatusView struct {
	Name    string         `json:"name"`
	Repo    string         `json:"repo"`
	PR      *statusPR      `json:"pr"`
	Checks  []statusCheck  `json:"checks"`
	Reviews []statusReview `json:"reviews"`
	Policy  statusPolicy   `json:"policy"`
	Usage   *statusUsage   `json:"usage"`
	Notes   []string       `json:"notes,omitempty"`
}

type statusPR struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	HeadSha          string `json:"headSha"`
	BaseRef          string `json:"baseRef"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	ChangedFiles     int    `json:"changedFiles"`
}

type statusCheck struct {
	Name       string `json:"name"`
	Workflow   string `json:"workflow"`
	Event      string `json:"event"`
	Conclusion string `json:"conclusion"`
}

type statusReview struct {
	Reviewer string `json:"reviewer"`
	Sha      string `json:"sha"`
	Crit     int    `json:"crit"`
	Imp      int    `json:"imp"`
	URL      string `json:"url"`
}

type statusPolicy struct {
	Mode    string   `json:"mode"`
	Verdict string   `json:"verdict"`
	Reasons []string `json:"reasons"`
}

// statusUsage is the session's cost. Priced is false when the transcript
// used a model cadence-hooks has no price for: CostUSD then covers only the
// priced models and is not the session's cost.
type statusUsage struct {
	CostUSD        float64         `json:"costUsd"`
	ByModel        json.RawMessage `json:"byModel"`
	Priced         bool            `json:"priced"`
	UnpricedModels []string        `json:"unpricedModels"`
}

type statusOptions struct {
	Repo string
	Name string
	JSON bool
}

// statusDeps are status's seams. newSurfaceStatusCmd fills them with the
// real ones.
type statusDeps struct {
	module.Deps
	// rows returns the worker's ledger row and its queue row (nil when the
	// queue holds none).
	rows func(ctx context.Context, warn io.Writer, repo, name string) (worker.Row, *worker.QueueRow, error)
	// settings resolves [surface.merge] from the config file, fresh.
	settings func() config.MergeSettings
	// reader reads GitHub.
	reader func() merge.Reader
	// usage prices a transcript, or returns nil.
	usage func(ctx context.Context, transcript string) *statusUsage
}

func newSurfaceStatusCmd(deps module.Deps) *cobra.Command {
	return newSurfaceStatusCmdWith(statusDeps{
		Deps: deps,
		rows: func(ctx context.Context, warn io.Writer, repo, name string) (worker.Row, *worker.QueueRow, error) {
			return statusRows(ctx, warn, deps, repo, name)
		},
		settings: localMergeSettings,
		reader: func() merge.Reader {
			r := merge.Reader{GH: githubauth.Runner(deps.Runner, githubauth.DefaultHost)}
			if c, err := worker.OpenStatusCache(); err == nil {
				r.Cache = c
			}
			return r
		},
		usage: func(ctx context.Context, transcript string) *statusUsage {
			return priceTranscript(ctx, deps.Runner, osexec.LookPath, transcript)
		},
	})
}

func newSurfaceStatusCmdWith(d statusDeps) *cobra.Command {
	opts := statusOptions{}
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show a worker's PR, checks, review markers, merge verdict and cost",
		Long: `status reads the named worker's pull request from GitHub and shows what the
merge policy ([surface.merge], ADR-0011) decides about it, with every reason.

The PR is found by its head branch, worker/<name>, on the repository the
worker's ledger row recorded at launch (GitHub's own name and id, never
the checkout's origin). Only a PR from that repository, not a fork, by the
account gh is authenticated as counts; more than one open PR on the branch
is refused. A PR number from a worker's report is never used. Every read is
bound to one head commit; a push during the read makes status fail and ask
to be run again. The file list and modes are cached per head commit in the
state directory.

usage is the session's cost from "cadence-hooks metrics price" when
cadence-hooks is on PATH and the row has a transcript, else null. When the
transcript used a model with no price, priced is false and costUsd covers
only the priced models: it is not the session's cost.

--json prints {"name","repo","pr":{"number","url","state","isDraft",
"headSha","baseRef","mergeable","mergeStateStatus","changedFiles"},
"checks":[{"name","workflow","event","conclusion"}],"reviews":[{"reviewer",
"sha","crit","imp","url"}],"policy":{"mode","verdict","reasons"},"usage":
{"costUsd","byModel","priced","unpricedModels"}}; pr is null when the branch
has no PR, and usage is null when it could not be priced. verdict is pass,
refuse or off. reviews lists the markers by [surface.merge]
marker_author_id, the ones the verdict counts.

Exit 0: status was read, whatever the verdict (a recorded base GitHub does
not have is a refusal reason). Exit 1: GitHub could not be
read. Exit 2: a usage or setup error (no such worker, a row that records no
GitHub repository).

  forgectl surface status fix-login
  forgectl surface status gh1175-forgectl --repo forgectl --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceStatus(cmd, d, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","repo","pr","checks","reviews","policy","usage"} as JSON`)
	return cmd
}

// localMergeSettings resolves [surface.merge] from this machine's config
// file, for this host and user. Any failure resolves to off.
func localMergeSettings() config.MergeSettings {
	path, err := config.ConfigPath()
	if err != nil {
		return config.MergeSettings{Mode: config.MergeOff, OffReason: "the config file cannot be located: " + err.Error()}
	}
	host, err := os.Hostname()
	if err != nil {
		return config.MergeSettings{Mode: config.MergeOff, OffReason: "the host name cannot be read: " + err.Error()}
	}
	return config.ResolveMerge(path, host, config.LocalMergeFileCheck())
}

// statusRows reads the worker's ledger row, through the same ledger
// list and close read, and its queue row.
func statusRows(ctx context.Context, warn io.Writer, deps module.Deps, repo, name string) (worker.Row, *worker.QueueRow, error) {
	w, err := openWorkerLedger(ctx, warn, deps, repo)
	if err != nil {
		return worker.Row{}, nil, err
	}
	rows, err := w.led.Rows()
	if err != nil {
		return worker.Row{}, nil, WithExitCode(err, exitUsage)
	}
	row, ok := findRow(rows, name)
	if !ok {
		return worker.Row{}, nil, WithExitCode(fmt.Errorf("no worker named %q in %s's ledger", name, w.top), exitUsage)
	}
	q, err := worker.OpenQueue()
	if err != nil {
		return worker.Row{}, nil, WithExitCode(err, exitUsage)
	}
	qrows, err := q.Rows()
	if err != nil {
		return worker.Row{}, nil, WithExitCode(termsafe.Error(err), exitUsage)
	}
	for i := range qrows {
		if qrows[i].Name == name && qrows[i].Repo == w.top {
			return row, &qrows[i], nil
		}
	}
	return row, nil, nil
}

// mergeRow is the policy's view of the ledger and queue rows.
func mergeRow(row worker.Row, q *worker.QueueRow) merge.Row {
	r := merge.Row{
		Name: row.Name, Branch: row.Branch, BranchFrom: row.BranchFrom, LaunchID: row.LaunchID, Stage: string(row.Stage),
		Base: row.Base, GitHubRepo: row.GitHubRepo, GitHubRepoID: row.GitHubRepoID,
	}
	if q != nil {
		r.QueueLaunchID, r.QueueState = q.LaunchID, string(q.State)
	}
	return r
}

func runSurfaceStatus(cmd *cobra.Command, d statusDeps, opts statusOptions) error {
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	row, qrow, err := d.rows(cmd.Context(), cmd.ErrOrStderr(), opts.Repo, opts.Name)
	if err != nil {
		return err
	}
	settings := d.settings()
	ctx, cancel := context.WithTimeout(cmd.Context(), statusTimeout)
	defer cancel()
	snap, err := statusRead(ctx, d.reader(), mergeRow(row, qrow), func(f merge.Facts) bool {
		return merge.Evaluate(f, merge.Policy{Settings: settings}).Result == merge.Pass
	})
	switch {
	case errors.Is(err, merge.ErrNoRecordedRepo):
		return WithExitCode(fmt.Errorf("worker %q: %w; it was launched before forgectl recorded one, or its origin is not on github.com", opts.Name, err), exitUsage)
	case err != nil:
		return termsafe.Error(err)
	}
	view := buildWorkerStatus(row, snap, settings)
	if row.Transcript != "" {
		view.Usage = d.usage(cmd.Context(), row.Transcript)
	}
	if opts.JSON {
		return writeJSON(cmd.OutOrStdout(), view)
	}
	return renderWorkerStatus(cmd.OutOrStdout(), view)
}

// statusRead reads row through r. The cache holds only the file list and
// modes, and any session on the operator's account can write it, so a pass
// is never shown on cached data: when a read that used the cache would
// pass, everything is read again without it. A refusal keeps the cached
// read.
func statusRead(ctx context.Context, r merge.Reader, row merge.Row, passes func(merge.Facts) bool) (merge.Snapshot, error) {
	snap, err := r.Read(ctx, row)
	if err != nil || !snap.Cached || !snap.HasPR || !passes(snap.Facts) {
		return snap, err
	}
	fresh := r
	fresh.Cache = nil
	return fresh.Read(ctx, row)
}

// buildWorkerStatus turns a read into the view, and decides the verdict: Evaluate
// for a PR, else off (mode off) or refuse naming why there is no PR.
func buildWorkerStatus(row worker.Row, snap merge.Snapshot, settings config.MergeSettings) workerStatusView {
	view := workerStatusView{
		Name: row.Name, Repo: row.GitHubRepo, Checks: []statusCheck{}, Reviews: []statusReview{},
		Policy: statusPolicy{Mode: string(settings.Mode)},
	}
	f := snap.Facts
	var v merge.Verdict
	if snap.HasPR {
		pr := f.PR
		view.PR = &statusPR{
			Number: pr.Number, URL: pr.URL, State: pr.State, IsDraft: pr.IsDraft, HeadSha: pr.HeadRefOid, BaseRef: pr.BaseRefName,
			Mergeable: pr.Mergeable, MergeStateStatus: pr.MergeStateStatus, ChangedFiles: pr.ChangedFiles,
		}
		for _, c := range f.Checks {
			if !c.HasWorkflowRun {
				continue
			}
			view.Checks = append(view.Checks, statusCheck{Name: c.Name, Workflow: c.WorkflowPath, Event: c.Event, Conclusion: c.Conclusion})
		}
		// The markers listed are the ones Evaluate counts: by
		// marker_author_id, never by whoever gh is logged in as.
		if settings.MarkerAuthorID > 0 {
			for _, r := range f.Reviews {
				if r.Author.Typename != "User" || r.Author.DatabaseID != settings.MarkerAuthorID {
					continue
				}
				if m, ok := merge.ParseMarker(r.Body); ok {
					view.Reviews = append(view.Reviews, statusReview{Reviewer: m.Reviewer, Sha: m.Head, Crit: m.Crit, Imp: m.Imp, URL: r.URL})
				}
			}
		} else {
			view.Notes = append(view.Notes, "no review markers listed: [surface.merge] marker_author_id is not set (or the policy did not resolve), so no account's markers count")
		}
		v = merge.Evaluate(f, merge.Policy{Settings: settings})
		if snap.Cached {
			view.Notes = append(view.Notes, "file list and modes from the cache for this head")
		}
	} else {
		v = merge.Evaluate(f, merge.Policy{Settings: settings})
		if v.Result != merge.Off {
			v = merge.Verdict{Result: merge.Refuse, Reasons: []string{"no PR: " + snap.NoPR}}
		}
	}
	view.Policy.Verdict, view.Policy.Reasons = string(v.Result), v.Reasons
	if view.Policy.Reasons == nil {
		view.Policy.Reasons = []string{}
	}
	return view
}

func renderWorkerStatus(out io.Writer, v workerStatusView) error {
	safe := func(s string) string { return termsafe.SafeLineMax(s, maxStatusText) }
	var b strings.Builder
	switch {
	case v.PR != nil:
		fmt.Fprintf(&b, "%s  %s#%d %s%s head %s\n", safe(v.Name), safe(v.Repo), v.PR.Number, safe(v.PR.State), draftWord(v.PR.IsDraft), safe(shortSHA(v.PR.HeadSha)))
	default:
		fmt.Fprintf(&b, "%s  %s  no PR\n", safe(v.Name), safe(v.Repo))
	}
	if len(v.Checks) > 0 {
		parts := make([]string, 0, len(v.Checks))
		for _, c := range v.Checks {
			parts = append(parts, safe(c.Name)+" "+safe(strings.ToLower(nonEmptyStr(c.Conclusion, "pending"))))
		}
		fmt.Fprintf(&b, "checks: %s\n", strings.Join(parts, ", "))
	}
	if len(v.Reviews) > 0 {
		parts := make([]string, 0, len(v.Reviews))
		for _, r := range v.Reviews {
			parts = append(parts, fmt.Sprintf("%s %s crit=%d imp=%d", safe(r.Reviewer), shortSHA(r.Sha), r.Crit, r.Imp))
		}
		fmt.Fprintf(&b, "markers: %s\n", strings.Join(parts, ", "))
	}
	for _, n := range v.Notes {
		fmt.Fprintf(&b, "note: %s\n", safe(n))
	}
	fmt.Fprintf(&b, "policy: %s, %s\n", safe(v.Policy.Mode), safe(v.Policy.Verdict))
	for _, r := range v.Policy.Reasons {
		fmt.Fprintf(&b, "  - %s\n", termsafe.SafeLineMax(r, 400))
	}
	if u := v.Usage; u != nil {
		if u.Priced {
			fmt.Fprintf(&b, "cost: $%.2f\n", u.CostUSD)
		} else {
			names := make([]string, len(u.UnpricedModels))
			for i, m := range u.UnpricedModels {
				names[i] = safe(m)
			}
			fmt.Fprintf(&b, "cost: partial, $%.2f for the priced models only; no price for %s\n", u.CostUSD, strings.Join(names, ", "))
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func draftWord(draft bool) string {
	if draft {
		return " (draft)"
	}
	return ""
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func nonEmptyStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// priceTranscript runs `cadence-hooks metrics price --transcript <path>
// --json` (cameronsjo/cadence-hooks, T10.1) when cadence-hooks resolves on
// PATH, with a timeout. Any failure (not on PATH, a non-zero exit, a
// timeout, output that is not the expected JSON) is nil: usage is a nicety
// and never fails status. Model names come from the transcript, so control,
// bidi and invisible characters are stripped from them before they are
// shown; two names that strip to the same text drop usage.
func priceTranscript(ctx context.Context, run exec.Runner, lookPath func(string) (string, error), transcript string) *statusUsage {
	bin, err := lookPath("cadence-hooks")
	if err != nil || bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	out, err := run.Run(ctx, bin, "metrics", "price", "--transcript", transcript, "--json")
	if err != nil {
		return nil
	}
	// The shape is cadence-hooks' sessions.jsonl one: byModel is an array of
	// {model, tokens, costUsd}, and unpricedModels lists the models the price
	// table lacks. A missing unpricedModels reads as not priced.
	var got struct {
		CostUSD        *float64                     `json:"costUsd"`
		ByModel        []map[string]json.RawMessage `json:"byModel"`
		UnpricedModels *[]string                    `json:"unpricedModels"`
	}
	if json.Unmarshal([]byte(out), &got) != nil || got.CostUSD == nil || *got.CostUSD < 0 || math.IsNaN(*got.CostUSD) || math.IsInf(*got.CostUSD, 0) {
		return nil
	}
	seen := make(map[string]bool, len(got.ByModel))
	clean := make([]map[string]json.RawMessage, 0, len(got.ByModel))
	for _, entry := range got.ByModel {
		var name string
		if raw, ok := entry["model"]; !ok || json.Unmarshal(raw, &name) != nil {
			return nil
		}
		cn := cleanModelName(name)
		if seen[cn] {
			return nil
		}
		seen[cn] = true
		// termsafe:allow-raw-json encodes one already-cleaned model name into the row; the document is written through writeJSON
		nameJSON, err := json.Marshal(cn)
		if err != nil {
			return nil
		}
		row := make(map[string]json.RawMessage, len(entry))
		for k, v := range entry {
			row[k] = v
		}
		row["model"] = nameJSON
		clean = append(clean, row)
	}
	// termsafe:allow-raw-json re-encodes already-decoded JSON values with each model name cleaned; the document is written through writeJSON
	cleanBy, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	unpriced := []string{}
	if got.UnpricedModels != nil {
		for _, m := range *got.UnpricedModels {
			unpriced = append(unpriced, cleanModelName(m))
		}
	}
	return &statusUsage{CostUSD: *got.CostUSD, ByModel: cleanBy, Priced: got.UnpricedModels != nil && len(unpriced) == 0, UnpricedModels: unpriced}
}

// maxModelName caps a model name shown in usage.
const maxModelName = 100

// cleanModelName drops control, bidi and invisible characters and invalid
// UTF-8 from a model name read from a transcript, and caps its length.
func cleanModelName(s string) string {
	kept := strings.Map(func(r rune) rune {
		if r == utf8.RuneError || termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(kept); len(r) > maxModelName {
		kept = string(r[:maxModelName])
	}
	if strings.TrimSpace(kept) == "" {
		return "(unnamed model)"
	}
	return kept
}
