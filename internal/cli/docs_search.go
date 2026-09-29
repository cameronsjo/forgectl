package cli

import (
	"context"
	"errors"
	"fmt"
	osexec "os/exec"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// docsSearchLookPath resolves the rg binary. Tests replace the seam to
// simulate a host without ripgrep.
var docsSearchLookPath = osexec.LookPath

// Human-output caps for the untrusted fields of one result line.
const (
	docsSearchRootRunes    = 64
	docsSearchPathRunes    = 256
	docsSearchSnippetRunes = 320
)

// newDocsSearchCmd builds `forgectl docs search <query>` — full-text search
// over the default docs roots with the ripgrep backend.
func newDocsSearchCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	var timeout time.Duration
	var limit int

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search the indexed docs (ripgrep backend)",
		Long: `search runs a case-insensitive, fixed-string full-text query over the
same roots docs list indexes with no arguments, using ripgrep (rg), and
prints one line per hit: root, path:line, snippet.

Only indexed docs are returned: every hit rg reports is checked against the
docs index, so nothing outside a root or excluded from the reader appears.
rg's own config file (RIPGREP_CONFIG_PATH) is ignored.

A query that starts with "-" must follow "--":

  forgectl docs search -- --flag-name

Results come in root order, then in path order within a root, so the same
query over the same tree prints the same lines and --limit keeps a stable
prefix. A doc reachable through two overlapping roots is printed once.

No match exits 0 with no results. If rg could not fully search a root (an
unreadable file, say), the results from every root are still printed, the
reason goes to stderr, and the exit code is 1. A missing rg or an expired
--timeout exits 2.`,
		Args: cobra.ExactArgs(1),
		// Same reason as docs list: under --json a failure has already put its
		// ONE JSON object on stderr, and cobra's own error line would be a
		// second write to that stream.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			if limit < 1 {
				return reportDocsSearchError(cmd, fmt.Errorf("--limit must be at least 1, not %d", limit), 2, asJSON)
			}
			if err := docspkg.ValidateQuery(query); err != nil {
				return reportDocsSearchError(cmd, err, 2, asJSON)
			}
			streamer, ok := deps.Runner.(forgexec.StreamingRunner)
			if !ok {
				return reportDocsSearchError(cmd, errors.New("docs search: streaming runner is unavailable"), 1, asJSON)
			}

			roots, err := resolveDocsRoots(nil, deps.Cfg.Docs)
			if err != nil {
				return reportDocsSearchError(cmd, err, 1, asJSON)
			}
			opts, err := docsIndexOptions(deps.Cfg.Docs)
			if err != nil {
				return reportDocsSearchError(cmd, err, 1, asJSON)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			idx, err := docspkg.NewIndexContext(ctx, roots, opts)
			if err != nil {
				return reportDocsSearchError(cmd, err, docsSearchExitCode(err), asJSON)
			}

			searcher := docspkg.Searcher{Runner: streamer, LookPath: docsSearchLookPath}
			resp, err := searcher.Search(ctx, idx, query, limit)
			if err != nil {
				return reportDocsSearchError(cmd, err, docsSearchExitCode(err), asJSON)
			}
			return printDocsSearch(cmd, resp, limit, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "deadline for indexing plus search, e.g. 10s or 1m")
	cmd.Flags().IntVar(&limit, "limit", 50, "return at most N results")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return WithExitCode(err, 2)
	})
	return cmd
}

// docsSearchExitCode maps a failure to its exit code: 2 for a missing search
// backend, an invalid query, or an expired deadline; 1 for anything else.
func docsSearchExitCode(err error) int {
	switch {
	case errors.Is(err, docspkg.ErrNoSearchBackend),
		errors.Is(err, docspkg.ErrInvalidQuery),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return 2
	}
	return 1
}

// docsSearchErrorJSON is the --json wire shape for a `docs search` failure,
// and the only thing written to stderr. On a fatal failure stdout stays
// empty; on a partial one (some root in the response's errors) stdout
// carries the full response.
type docsSearchErrorJSON struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// reportDocsSearchError renders a failure the way reportDocsListDeadline
// does: under --json exactly one JSON object on stderr and a silent coded
// error, otherwise the normal error path with the exit code attached.
func reportDocsSearchError(cmd *cobra.Command, err error, code int, asJSON bool) error {
	if !asJSON {
		return WithExitCode(err, code)
	}
	obj := docsSearchErrorJSON{Error: err.Error(), Code: code}
	if encErr := termsafe.JSONEncoder(cmd.ErrOrStderr()).Encode(obj); encErr != nil {
		return WithExitCode(fmt.Errorf("docs search: encode error: %w", encErr), code)
	}
	return newSilentCodedError(code)
}

// printDocsSearch writes the results, then turns any per-root failure into
// exit 1 with its reason on stderr: under --json one {"error","code"} object
// (stdout still carries the full response, errors array included), otherwise
// one line per failed root.
func printDocsSearch(cmd *cobra.Command, resp docspkg.SearchResponse, limit int, asJSON bool) error {
	if err := printDocsSearchResults(cmd, resp, limit, asJSON); err != nil {
		return err
	}
	if len(resp.Errors) == 0 {
		return nil
	}
	if asJSON {
		msg := fmt.Sprintf("docs search: %d root(s) could not be fully searched; see errors in the response", len(resp.Errors))
		return reportDocsSearchError(cmd, errors.New(msg), 1, true)
	}
	for _, e := range resp.Errors {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "docs search: root %s: %s\n",
			termsafe.SafeLineMax(e.Root, docsSearchRootRunes),
			termsafe.SafeLine(e.Message))
	}
	return newSilentCodedError(1)
}

func printDocsSearchResults(cmd *cobra.Command, resp docspkg.SearchResponse, limit int, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		return termsafe.JSONEncoder(out).Encode(resp)
	}
	if len(resp.Results) == 0 {
		if len(resp.Errors) == 0 {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no matches")
		}
		return nil
	}
	for _, r := range resp.Results {
		_, _ = fmt.Fprintf(out, "%s  %s:%d  %s\n",
			termsafe.SafeLineMax(r.Root, docsSearchRootRunes),
			termsafe.SafeLineMax(r.Path, docsSearchPathRunes),
			r.Line,
			termsafe.SafeLineMax(r.Snippet, docsSearchSnippetRunes))
	}
	if resp.Truncated {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "showing the first %d results; raise --limit for more\n", limit)
	}
	return nil
}
