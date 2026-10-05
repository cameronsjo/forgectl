package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newDocsCheckCmd builds `forgectl docs check [dir|file ...]` — reports broken
// links, broken anchors, ambiguous links, orphan pages, and deprecated or
// stale docs (OKF status / stale_after frontmatter) across every
// root (vault roots by the reader's vault rules, links only), without binding a server.
//
// Exit contract: 0 clean, or only info findings; 1 error-severity findings (the
// report is complete on stdout); 2 the check could not run (bad root, deadline,
// bad flag) under the shared docsFail contract, or could not
// vouch for the tree: the index walk skipped an unreadable path, so docs inside
// it went unchecked and links into it read as broken. The skipped paths are
// listed (human output) or in report.skipped (--json).
func newDocsCheckCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "check [dir|file ...]",
		Short: "Report broken links, orphan pages, and deprecated or stale docs",
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

			if asJSON {
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				if err := enc.Encode(report); err != nil {
					return fail("", fmt.Errorf("docs check: encode report: %w", err))
				}
			} else {
				out := cmd.OutOrStdout()
				for _, f := range report.Findings {
					line := safeLabel(f.Root) + "/" + safeColumnPath(f.Path)
					if f.Line > 0 {
						line += ":" + strconv.Itoa(f.Line)
					}
					line += ": " + string(f.Kind)
					if f.Target != "" {
						line += " " + safeText(f.Target)
					}
					if f.StaleAfter != "" {
						line += " " + safeLabel(f.StaleAfter)
					}
					_, _ = fmt.Fprintln(out, line)
				}
				for _, sp := range report.Skipped {
					_, _ = fmt.Fprintf(out, "%s/%s: skipped (%s)\n",
						safeLabel(sp.Root), safeColumnPath(sp.Rel), safeText(sp.Reason))
				}
			}

			// Checked after the report is written, so the report is complete;
			// exit 2 outranks findings because a partial tree cannot be vouched
			// for either way.
			if n := len(report.Skipped); n > 0 {
				// A finding-level exit 2, not a pre-work failure: the check ran and
				// its report is already on stdout, so docsFail's stderr error
				// object (for verbs that produced nothing) is not emitted under
				// --json. Human mode keeps a one-line reason on stderr.
				if asJSON {
					return newSilentCodedError(2)
				}
				msg := fmt.Sprintf("docs check: %d path(s) could not be read; the result covers a partial tree", n)
				if e := report.Errors(); e > 0 {
					msg += fmt.Sprintf(" (and %d error finding(s))", e)
				}
				return WithExitCode(errors.New(msg), 2)
			}

			// Only an error-severity finding fails the check; a deprecated page
			// is informational and exits 0.
			info := len(report.Findings) - report.Errors()
			if n := report.Errors(); n > 0 {
				msg := fmt.Sprintf("docs check: %d finding(s)", len(report.Findings))
				if info > 0 {
					msg += fmt.Sprintf(", %d informational", info)
				}
				// Under --json the report on stdout is the verdict: no
				// second object on stderr (forgectl#862).
				return jsonVerdict(WithExitCode(errors.New(msg), 1), asJSON)
			}
			if info > 0 && !asJSON {
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

// noteSkippedPaths prints one stderr line per root that lost paths to an
// unreadable-path skip. It ignores the log level on purpose: the skip is
// otherwise signalled only through slog, which the default log_level "off"
// discards. Callers skip it under --json, where stderr carries at most the
// one {"error","code","root"} object (#649).
func noteSkippedPaths(w io.Writer, idx *docspkg.Index) {
	counts := map[string]int{}
	var order []string
	for _, sp := range idx.Skipped() {
		if counts[sp.Root] == 0 {
			order = append(order, sp.Root)
		}
		counts[sp.Root]++
	}
	for _, root := range order {
		_, _ = fmt.Fprintf(w, "skipped %d unreadable path(s) under %s (see docs check)\n",
			counts[root], safeLabel(root))
	}
}
