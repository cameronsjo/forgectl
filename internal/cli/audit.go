package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/audit"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// auditModule declares the read-only posture-scan extension (ADR-0005,
// forgectl#14): no config section, no alias surface. `audit` is a pure
// grouping parent so each scan is its own leaf with its own --json shape;
// the secret-hygiene scan lands beside `injection` without changing it.
var auditModule = module.Manifest{
	Name: "audit",
	Tier: module.TierExtension,
	New:  newAuditCmd,
}

func newAuditCmd(module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read-only security posture scans across the projects root",
		Long: `audit maps what an agent could be steered by, across every repo under the
projects root ($PROJECTS_DIR, else ~/Projects). Every scan is read-only.

  forgectl audit injection          list every agent-instruction carrier
  forgectl audit injection --json   the same, machine-readable`,
	}
	cmd.AddCommand(newAuditInjectionCmd(projects.ResolveRoot, time.Now))
	return cmd
}

func newAuditInjectionCmd(resolveRoot func() string, now func() time.Time) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "injection",
		Short: "Inventory agent-instruction files (the prompt-injection surface)",
		Long: `injection walks the projects root and lists every file or directory an agent
reads instructions from: the same classes quarantine hides (CLAUDE.md,
AGENTS.md, .claude/, .mcp.json, .cursor/, .github/instructions/, …). It reports
paths, never contents, and follows no symlink.

Each carrier carries anomaly flags:
  vendored   inside a dependency directory (node_modules, vendor, …)
  off-root   a root-only carrier somewhere other than a git working-tree root
  recent     modified in the last 7 days
  symlink    a symlinked directory a carrier would live behind (not followed)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := audit.ScanInjection(audit.Options{Root: resolveRoot(), Now: now()})
			if err != nil {
				return err
			}
			if asJSON {
				return writeAuditInjectionJSON(cmd.OutOrStdout(), report)
			}
			writeAuditInjectionText(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"root","repos_scanned","entries_scanned","unreadable_dirs","truncated","capped_by","depth_skipped","carriers":[{"path","repo","target","type","modified","anomalies"}]} to stdout`)
	return cmd
}

// auditInjectionJSON is the --json wire shape. Additive changes only
// (ADR-0008).
type auditInjectionJSON struct {
	Root           string                `json:"root"`
	ReposScanned   int                   `json:"repos_scanned"`
	EntriesScanned int                   `json:"entries_scanned"`
	UnreadableDirs int                   `json:"unreadable_dirs"`
	Truncated      bool                  `json:"truncated"`
	CappedBy       []string              `json:"capped_by"`
	DepthSkipped   int                   `json:"depth_skipped"`
	Carriers       []auditCarrierRowJSON `json:"carriers"`
}

type auditCarrierRowJSON struct {
	Path      string   `json:"path"`
	Repo      string   `json:"repo"`
	Target    string   `json:"target"`
	Type      string   `json:"type"`
	Modified  string   `json:"modified"`
	Anomalies []string `json:"anomalies"`
}

func writeAuditInjectionJSON(w io.Writer, r audit.Report) error {
	out := auditInjectionJSON{
		Root:           r.Root,
		ReposScanned:   r.Repos,
		EntriesScanned: r.Entries,
		UnreadableDirs: r.Unreadable,
		Truncated:      r.Truncated,
		CappedBy:       r.CappedBy,
		DepthSkipped:   r.DepthSkipped,
		Carriers:       make([]auditCarrierRowJSON, 0, len(r.Findings)),
	}
	if out.CappedBy == nil {
		out.CappedBy = []string{}
	}
	for _, f := range r.Findings {
		row := auditCarrierRowJSON{
			Path:      f.Path,
			Repo:      f.Repo,
			Target:    f.Target,
			Type:      f.Type,
			Anomalies: f.Anomalies,
		}
		if row.Anomalies == nil {
			row.Anomalies = []string{}
		}
		if !f.ModTime.IsZero() {
			row.Modified = f.ModTime.UTC().Format(time.RFC3339)
		}
		out.Carriers = append(out.Carriers, row)
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// writeAuditInjectionText groups carriers by repo, each repo printed once
// even when a nested repo's carriers sort between its parent's. Every path is
// clone-derived, so each one goes through auditShowPath.
func writeAuditInjectionText(w io.Writer, r audit.Report) {
	groups := map[string][]audit.Finding{}
	var order []string
	for _, f := range r.Findings {
		if _, seen := groups[f.Repo]; !seen {
			order = append(order, f.Repo)
		}
		groups[f.Repo] = append(groups[f.Repo], f)
	}
	sort.Strings(order) // "" (outside any repo) sorts first
	_, _ = fmt.Fprintf(w, "%d agent-instruction carriers in %d locations under %s (%d repos, %d entries scanned)\n",
		len(r.Findings), len(order), auditShowPath(r.Root), r.Repos, r.Entries)
	for _, repo := range order {
		base := repo
		if repo == "" {
			base = r.Root
			_, _ = fmt.Fprintln(w, "(outside any git repo)")
		} else {
			_, _ = fmt.Fprintln(w, auditShowPath(repo))
		}
		for _, f := range groups[repo] {
			rel, err := filepath.Rel(base, f.Path)
			if err != nil {
				rel = f.Path
			}
			line := fmt.Sprintf("  %s  %s", auditShowPath(rel), f.Type)
			if len(f.Anomalies) > 0 {
				line += "  " + strings.Join(f.Anomalies, ",")
			}
			_, _ = fmt.Fprintln(w, line)
		}
	}
	if r.Unreadable > 0 {
		_, _ = fmt.Fprintf(w, "note: %d directories could not be read and were skipped\n", r.Unreadable)
	}
	for _, c := range r.CappedBy {
		switch c {
		case audit.CapEntries:
			_, _ = fmt.Fprintf(w, "note: the scan stopped at the %d-entry cap, so this list is incomplete\n", r.MaxEntries)
		case audit.CapFindings:
			_, _ = fmt.Fprintf(w, "note: the scan stopped at the %d-carrier cap, so this list is incomplete\n", r.MaxFindings)
		case audit.CapDepth:
			_, _ = fmt.Fprintf(w, "note: %d directories below the %d-level depth cap were not scanned\n", r.DepthSkipped, r.MaxDepth)
		}
	}
}

// auditShowPath prints an ordinary path bare and quotes (escaped, capped)
// any path holding a rune a terminal would act on, or one past the echo cap.
func auditShowPath(p string) string {
	if termsafe.QuoteText(p) == `"`+p+`"` && utf8.RuneCountInString(p) <= termsafe.PathEchoMaxRunes {
		return p
	}
	return termsafe.QuotePath(p)
}
