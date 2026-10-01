package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/audit"
	"github.com/cameronsjo/forgectl/internal/audit/gitleaks"
	"github.com/cameronsjo/forgectl/internal/audit/gitstate"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// --gitleaks modes.
const (
	gitleaksAuto    = "auto"
	gitleaksOff     = "off"
	gitleaksRequire = "require"
)

// gitleaks pass statuses on the wire, beyond the gitleaks package's own
// (ran, failed, timed_out) and Binary states (absent, refused, too_old,
// version_failed).
const gitleaksStatusOff = "off"

// auditSecretsDeps are the seams `audit secrets` runs through.
type auditSecretsDeps struct {
	resolveRoot func() (string, error)
	// runner runs git (through gitenv) and gitleaks.
	runner   exec.Runner
	lookPath func(string) (string, error)
	// gitStatusTimeout bounds each repo's git status; zero means
	// gitstate.DefaultTimeout. Tests shorten it.
	gitStatusTimeout time.Duration
}

// gitleaksOutcome is the gitleaks half of the report, whatever happened.
type gitleaksOutcome struct {
	mode    string
	status  string
	binary  gitleaks.Binary
	timeout time.Duration
	result  gitleaks.Result
}

func newAuditSecretsCmd(d auditSecretsDeps) *cobra.Command {
	var (
		asJSON  bool
		mode    string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Find stray .env files, private keys and committed secrets",
		Long: `secrets walks the projects root for secret-hygiene problems. Native checks
always run and read only metadata (one exception: a *.pem or *.key file has its
first 4 KiB checked for a private-key header, and only a yes/no is kept):

  env              .env, .env.*, .envrc (not .example/.sample/.template/.tmpl/.dist)
  key              id_rsa and the other ssh identities, *.p12, *.pfx, *.keystore,
                   and *.pem/*.key files holding a PEM private key
  scanner-config   .gitleaks.toml and .gitleaksignore, which can hide findings

A .env git ignores is counted, not listed. Each finding carries flags:
  tracked        git tracks it
  unignored      an untracked .env no ignore rule covers
  outside-repo   a .env outside any git working tree
  git-unknown    git could not say, so it is listed
  vendored       inside a dependency directory
  loose          readable or writable by group or others (mode & 0o077)
  foreign-owner  a key owned by another user

When gitleaks is installed (8.19.0 or later, an absolute path outside the
projects root), each repo's working tree is also scanned with ` + "`gitleaks dir`" + `
under forgectl's own config. History is not scanned. Every output states
what gitleaks did.

A repo whose root .gitleaksignore or .gitleaks.toml is not a regular file is
not given to gitleaks (gitleaks would block opening a FIFO there); the output
lists it as skipped.

Exit codes: 0 when the scan ran, whatever it found; 1 when the projects root
cannot be opened, when gitleaks was found but failed, timed out, skipped a
repo, or its ` + "`gitleaks version`" + ` failed, or when --gitleaks=require and
gitleaks did not run; 2 for a bad flag value.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch mode {
			case gitleaksAuto, gitleaksOff, gitleaksRequire:
			default:
				return WithExitCode(fmt.Errorf("--gitleaks must be auto, off or require, not %s", termsafe.QuoteTextMax(mode, labelMaxRunes)), 2)
			}
			if timeout <= 0 {
				return WithExitCode(errors.New("--gitleaks-timeout must be positive"), 2)
			}
			root, err := d.resolveRoot()
			if err != nil {
				return termsafe.Error(err)
			}
			report, err := audit.ScanSecrets(audit.SecretsOptions{Root: root})
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			gitTimeout := d.gitStatusTimeout
			if gitTimeout <= 0 {
				gitTimeout = gitstate.DefaultTimeout
			}
			report.ApplyGitStatus(gitstate.FuncWithTimeout(ctx, d.runner, gitTimeout))

			out := gitleaksOutcome{mode: mode, status: gitleaksStatusOff, timeout: timeout, result: gitleaks.Result{Findings: []gitleaks.Finding{}}}
			if mode != gitleaksOff {
				out.binary = gitleaks.Resolve(ctx, d.lookPath, d.runner, report.Root)
				out.status = out.binary.State
				if out.binary.State == gitleaks.StateAvailable {
					out.result = gitleaks.Scan(ctx, d.runner, out.binary.Path, report.Repos, gitleaksSkips(report), timeout)
					out.status = out.result.Status
				}
			}

			if asJSON {
				if err := writeAuditSecretsJSON(cmd.OutOrStdout(), report, out); err != nil {
					return err
				}
			} else {
				writeAuditSecretsText(cmd.OutOrStdout(), report, out)
			}
			return jsonVerdict(gitleaksVerdict(out), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"root","repos_scanned","entries_scanned","unreadable_dirs","unreadable_files","ignored_env_files","git_status_failed_repos","truncated","capped_by","depth_skipped","findings":[{"path","repo","kind","type","flags"}],"gitleaks":{"mode","status","reason","version","min_version","path","repos_scanned","repos_failed","findings_rejected","repos_skipped":[{"repo","reason"}],"truncated","findings":[{"path","repo","rule","line","fingerprint"}]}} to stdout`)
	cmd.Flags().StringVar(&mode, "gitleaks", gitleaksAuto, "run gitleaks: auto (when installed), off, or require (fail without it)")
	cmd.Flags().DurationVar(&timeout, "gitleaks-timeout", gitleaks.DefaultTimeout, "deadline for the whole gitleaks pass")
	return cmd
}

// gitleaksVerdict is the exit decision. A gitleaks that was found but failed
// fails the verb under auto too (ADR-0008 rule 3): the scan the operator
// asked for did not complete. Absent, refused or too old under auto is the
// native-only scan auto promises, and the output says so.
func gitleaksVerdict(o gitleaksOutcome) error {
	switch o.status {
	case gitleaks.StatusRan:
		if n := len(o.result.Skipped); n > 0 {
			return fmt.Errorf("audit secrets: gitleaks skipped %d repos whose scanner config is not a regular file", n)
		}
		return nil
	case gitleaksStatusOff:
		return nil
	case gitleaks.StatusFailed:
		return fmt.Errorf("audit secrets: gitleaks failed in %d of %d repos", o.result.ReposFailed, o.result.ReposFailed+o.result.ReposScanned)
	case gitleaks.StatusTimedOut:
		return fmt.Errorf("audit secrets: gitleaks timed out after %s", o.timeout)
	case gitleaks.StateVersionFailed:
		return errors.New("audit secrets: gitleaks was found, but `gitleaks version` failed")
	}
	if o.mode == gitleaksRequire {
		return fmt.Errorf("audit secrets: --gitleaks=require, but %s", gitleaksSkipReason(o))
	}
	return nil
}

// gitleaksSkipReason words why gitleaks did not run.
func gitleaksSkipReason(o gitleaksOutcome) string {
	switch o.status {
	case gitleaks.StateAbsent:
		return "gitleaks was not found on PATH"
	case gitleaks.StateRefused:
		if o.binary.Reason == gitleaks.ReasonUnderScanRoot {
			return "the gitleaks on PATH lies inside the projects root, so it was not run"
		}
		return "gitleaks was found only through a relative PATH entry, so it was not run"
	case gitleaks.StateTooOld:
		if o.binary.Version == "" {
			return "gitleaks printed no version forgectl can read (needs " + gitleaks.MinVersion + " or later)"
		}
		return "gitleaks " + o.binary.Version + " is older than " + gitleaks.MinVersion
	case gitleaks.StateVersionFailed:
		return "`gitleaks version` failed"
	case gitleaksStatusOff:
		return "--gitleaks=off"
	}
	return "gitleaks did not run"
}

// gitleaksStatusLine is the one line every text output carries about
// gitleaks (ADR-0008 rule 4: no hidden mode).
func gitleaksStatusLine(o gitleaksOutcome) string {
	switch o.status {
	case gitleaks.StatusRan:
		line := fmt.Sprintf("gitleaks %s: ran over %d repos, %d findings", o.binary.Version, o.result.ReposScanned, len(o.result.Findings))
		if o.result.Truncated {
			line += fmt.Sprintf(" (stopped at the %d-finding cap)", gitleaks.MaxFindings)
		}
		if n := len(o.result.Skipped); n > 0 {
			line += fmt.Sprintf("; SKIPPED %d repos whose scanner config is not a regular file", n)
		}
		return line
	case gitleaks.StatusFailed:
		return fmt.Sprintf("gitleaks %s: FAILED in %d of %d repos; %d findings from the rest", o.binary.Version,
			o.result.ReposFailed, o.result.ReposFailed+o.result.ReposScanned, len(o.result.Findings))
	case gitleaks.StatusTimedOut:
		return fmt.Sprintf("gitleaks %s: TIMED OUT after %s; %d findings from the %d repos it finished", o.binary.Version,
			o.timeout, len(o.result.Findings), o.result.ReposScanned)
	}
	return "gitleaks: not run, " + gitleaksSkipReason(o) + "; native checks only"
}

// gitleaksSkips names the repos the gitleaks pass must not scan: those whose
// root .gitleaksignore or .gitleaks.toml the walk found to be something
// other than a regular file. gitleaks opens a root .gitleaksignore
// unconditionally, and opening a FIFO blocks until a writer appears.
func gitleaksSkips(r audit.SecretsReport) map[string]string {
	skip := map[string]string{}
	for _, f := range r.Findings {
		if f.Kind == audit.KindScannerConfig && f.Repo != "" && filepath.Dir(f.Path) == f.Repo && f.Type != audit.TypeFile {
			skip[f.Repo] = gitleaks.SkipScannerConfigNotRegular
		}
	}
	return skip
}

// auditSecretsJSON is the --json wire shape. Additive changes only
// (ADR-0008).
type auditSecretsJSON struct {
	Root            string                 `json:"root"`
	ReposScanned    int                    `json:"repos_scanned"`
	EntriesScanned  int                    `json:"entries_scanned"`
	UnreadableDirs  int                    `json:"unreadable_dirs"`
	UnreadableFiles int                    `json:"unreadable_files"`
	IgnoredEnv      int                    `json:"ignored_env_files"`
	GitFailed       int                    `json:"git_status_failed_repos"`
	Truncated       bool                   `json:"truncated"`
	CappedBy        []string               `json:"capped_by"`
	DepthSkipped    int                    `json:"depth_skipped"`
	Findings        []auditSecretRowJSON   `json:"findings"`
	Gitleaks        auditGitleaksBlockJSON `json:"gitleaks"`
}

type auditSecretRowJSON struct {
	Path  string   `json:"path"`
	Repo  string   `json:"repo"`
	Kind  string   `json:"kind"`
	Type  string   `json:"type"`
	Flags []string `json:"flags"`
}

type auditGitleaksBlockJSON struct {
	Mode             string                  `json:"mode"`
	Status           string                  `json:"status"`
	Reason           string                  `json:"reason"`
	Version          string                  `json:"version"`
	MinVersion       string                  `json:"min_version"`
	Path             string                  `json:"path"`
	ReposScanned     int                     `json:"repos_scanned"`
	ReposFailed      int                     `json:"repos_failed"`
	FindingsRejected int                     `json:"findings_rejected"`
	ReposSkipped     []auditGitleaksSkipJSON `json:"repos_skipped"`
	Truncated        bool                    `json:"truncated"`
	Findings         []auditGitleaksRowJSON  `json:"findings"`
}

type auditGitleaksSkipJSON struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

type auditGitleaksRowJSON struct {
	Path        string `json:"path"`
	Repo        string `json:"repo"`
	Rule        string `json:"rule"`
	Line        int    `json:"line"`
	Fingerprint string `json:"fingerprint"`
}

func writeAuditSecretsJSON(w io.Writer, r audit.SecretsReport, o gitleaksOutcome) error {
	out := auditSecretsJSON{
		Root:            r.Root,
		ReposScanned:    len(r.Repos),
		EntriesScanned:  r.Entries,
		UnreadableDirs:  r.Unreadable,
		UnreadableFiles: r.UnreadableFiles,
		IgnoredEnv:      r.IgnoredEnv,
		GitFailed:       r.GitStatusFailed,
		Truncated:       r.Truncated,
		CappedBy:        r.CappedBy,
		DepthSkipped:    r.DepthSkipped,
		Findings:        make([]auditSecretRowJSON, 0, len(r.Findings)),
		Gitleaks: auditGitleaksBlockJSON{
			Mode:             o.mode,
			Status:           o.status,
			Reason:           o.binary.Reason,
			Version:          o.binary.Version,
			MinVersion:       gitleaks.MinVersion,
			Path:             o.binary.Path,
			ReposScanned:     o.result.ReposScanned,
			ReposFailed:      o.result.ReposFailed,
			FindingsRejected: o.result.Rejected,
			ReposSkipped:     make([]auditGitleaksSkipJSON, 0, len(o.result.Skipped)),
			Truncated:        o.result.Truncated,
			Findings:         make([]auditGitleaksRowJSON, 0, len(o.result.Findings)),
		},
	}
	if out.CappedBy == nil {
		out.CappedBy = []string{}
	}
	for _, f := range r.Findings {
		row := auditSecretRowJSON{Path: f.Path, Repo: f.Repo, Kind: f.Kind, Type: f.Type, Flags: f.Flags}
		if row.Flags == nil {
			row.Flags = []string{}
		}
		out.Findings = append(out.Findings, row)
	}
	for _, sk := range o.result.Skipped {
		out.Gitleaks.ReposSkipped = append(out.Gitleaks.ReposSkipped, auditGitleaksSkipJSON{Repo: sk.Repo, Reason: sk.Reason})
	}
	for _, f := range o.result.Findings {
		out.Gitleaks.Findings = append(out.Gitleaks.Findings, auditGitleaksRowJSON{
			Path: f.File, Repo: f.Repo, Rule: f.RuleID, Line: f.StartLine, Fingerprint: f.Fingerprint,
		})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// auditLine writes one line of text output. Every line passes redact.Stdout
// first: forgectl never decodes a secret, so this is the backstop, not the
// control.
func auditLine(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, redact.Stdout(line))
}

// writeAuditSecretsText prints the gitleaks status first, then the native
// findings and the gitleaks findings, each grouped by repo.
func writeAuditSecretsText(w io.Writer, r audit.SecretsReport, o gitleaksOutcome) {
	auditLine(w, gitleaksStatusLine(o))
	header := fmt.Sprintf("%d secret-hygiene findings under %s (%d repos, %d entries scanned)",
		len(r.Findings), auditShowPath(r.Root), len(r.Repos), r.Entries)
	auditLine(w, header)
	if r.IgnoredEnv > 0 {
		auditLine(w, fmt.Sprintf("  %d .env files git ignores are not listed", r.IgnoredEnv))
	}
	groupByRepo(w, r.Root, len(r.Findings), func(i int) (string, string) { return r.Findings[i].Repo, r.Findings[i].Path },
		func(i int, rel string) string {
			f := r.Findings[i]
			line := fmt.Sprintf("  %s  %s %s", auditShowPath(rel), f.Kind, f.Type)
			if len(f.Flags) > 0 {
				line += "  " + strings.Join(f.Flags, ",")
			}
			return line
		})
	if len(o.result.Findings) > 0 {
		auditLine(w, fmt.Sprintf("gitleaks findings (%d):", len(o.result.Findings)))
		groupByRepo(w, r.Root, len(o.result.Findings), func(i int) (string, string) { return o.result.Findings[i].Repo, o.result.Findings[i].File },
			func(i int, rel string) string {
				f := o.result.Findings[i]
				return fmt.Sprintf("  %s:%d  %s", auditShowPath(rel), f.StartLine, safeLabel(f.RuleID))
			})
	}
	for _, sk := range o.result.Skipped {
		auditLine(w, "note: gitleaks skipped "+auditShowPath(sk.Repo)+": its root .gitleaksignore or .gitleaks.toml is not a regular file")
	}
	if o.result.Rejected > 0 {
		auditLine(w, fmt.Sprintf("note: %d gitleaks findings named a file outside the repo scanned and were dropped", o.result.Rejected))
	}
	if r.GitStatusFailed > 0 {
		auditLine(w, fmt.Sprintf("note: git could not report tracked/ignored state in %d repos; their findings are flagged git-unknown", r.GitStatusFailed))
	}
	if r.UnreadableFiles > 0 {
		auditLine(w, fmt.Sprintf("note: %d .pem/.key files could not be read to check for a private key", r.UnreadableFiles))
	}
	if r.Unreadable > 0 {
		auditLine(w, fmt.Sprintf("note: %d directories could not be read and were skipped", r.Unreadable))
	}
	for _, c := range r.CappedBy {
		switch c {
		case audit.CapEntries:
			auditLine(w, fmt.Sprintf("note: the scan stopped at the %d-entry cap, so this list is incomplete", r.MaxEntries))
		case audit.CapFindings:
			auditLine(w, fmt.Sprintf("note: the scan stopped at the %d-finding cap, so this list is incomplete", r.MaxFindings))
		case audit.CapDepth:
			auditLine(w, fmt.Sprintf("note: %d directories below the %d-level depth cap were not scanned", r.DepthSkipped, r.MaxDepth))
		}
	}
}

// groupByRepo prints n rows grouped under their repo, each repo once, with
// rows outside any repo first. at returns row i's repo and path; row
// renders it given its path relative to the group's base.
func groupByRepo(w io.Writer, root string, n int, at func(i int) (repo, p string), row func(i int, rel string) string) {
	groups := map[string][]int{}
	var order []string
	for i := 0; i < n; i++ {
		repo, _ := at(i)
		if _, seen := groups[repo]; !seen {
			order = append(order, repo)
		}
		groups[repo] = append(groups[repo], i)
	}
	sort.Strings(order)
	for _, repo := range order {
		base := repo
		if repo == "" {
			base = root
			auditLine(w, "(outside any git repo)")
		} else {
			auditLine(w, auditShowPath(repo))
		}
		for _, i := range groups[repo] {
			_, p := at(i)
			rel, err := filepath.Rel(base, p)
			if err != nil {
				rel = p
			}
			auditLine(w, row(i, rel))
		}
	}
}
