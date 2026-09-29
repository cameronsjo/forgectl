package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newDocsCheckCmd builds `forgectl docs check [dir|file ...]` — reports broken
// links, broken anchors, ambiguous links, orphan pages, and deprecated or
// stale docs (OKF status / stale_after frontmatter) across the docs-kind
// roots, without binding a server.
//
// Exit contract: 0 clean; 1 error-severity findings (the report is complete on stdout);
// 2 the check could not run (bad root, deadline, no docs-kind root, bad flag).
func newDocsCheckCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "check [dir|file ...]",
		Short: "Report broken links, orphan pages, and deprecated or stale docs in docs roots",
		Args:  cobra.ArbitraryArgs,
		// Silenced for the same reason docs list is: a failure under --json
		// has already written its one JSON object to stderr.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fail := func(root string, err error) error {
				return docsFail(cmd, "docs check", root, err, 2, asJSON)
			}
			roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
			if err != nil {
				return fail("", err)
			}
			opts, err := docsIndexOptions(deps.Cfg.Docs)
			if err != nil {
				return fail("", err)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			idx, err := docspkg.NewIndexContext(ctx, roots, opts)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					fallback := ""
					if len(roots) > 0 {
						fallback = roots[0]
					}
					return fail(deadlineRoot(err, fallback), err)
				}
				return fail("", err)
			}

			report := idx.Check()

			checked := 0
			for _, r := range report.Roots {
				if r.Checked {
					checked++
					continue
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "skipping vault root %s: %s\n",
					termsafe.SafeLine(r.Label), termsafe.SafeLine(r.Skipped))
			}
			if checked == 0 {
				return fail("", errors.New("docs check: no docs-kind root to check (vault roots are skipped)"))
			}

			if asJSON {
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				if err := enc.Encode(report); err != nil {
					return fail("", fmt.Errorf("docs check: encode report: %w", err))
				}
			} else {
				out := cmd.OutOrStdout()
				for _, f := range report.Findings {
					line := termsafe.SafeLine(f.Root) + "/" + termsafe.SafeLine(f.Path)
					if f.Line > 0 {
						line += ":" + strconv.Itoa(f.Line)
					}
					line += ": " + string(f.Kind)
					if f.Target != "" {
						line += " " + termsafe.SafeLine(f.Target)
					}
					if f.StaleAfter != "" {
						line += " " + termsafe.SafeLine(f.StaleAfter)
					}
					_, _ = fmt.Fprintln(out, line)
				}
			}

			// Only an error-severity finding fails the check; a deprecated page
			// is informational and exits 0.
			info := len(report.Findings) - report.Errors()
			if n := report.Errors(); n > 0 {
				msg := fmt.Sprintf("docs check: %d finding(s)", len(report.Findings))
				if info > 0 {
					msg += fmt.Sprintf(", %d informational", info)
				}
				return WithExitCode(errors.New(msg), 1)
			}
			if info > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "docs check: %d informational finding(s), no errors\n", info)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "walk deadline, e.g. 15s or 2m")
	cmd.SetFlagErrorFunc(docsFlagError("docs check"))
	return cmd
}
