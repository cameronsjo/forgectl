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

// docsSearchLookPath resolves the backend binary (rg or qmd). Tests replace
// the seam to simulate a host without it.
var docsSearchLookPath = osexec.LookPath

// Human-output caps for the untrusted fields of one result line.
const (
	docsSearchRootRunes    = 64
	docsSearchPathRunes    = 256
	docsSearchSnippetRunes = 320
)

// newDocsSearchCmd builds `forgectl docs search <query>` — full-text search
// over the default docs roots with the ripgrep backend, or with qmd when
// --backend or [docs] search_backend asks for it.
func newDocsSearchCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	var timeout time.Duration
	var limit int
	var backend string

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search the indexed docs (ripgrep, or opt-in qmd)",
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
reason goes to stderr, and the exit code is 1. A missing rg, a root or
config error, or an expired --timeout exits 2, and under --json writes one
{"error","code","root"} object to stderr with stdout empty.

--backend qmd (or search_backend = "qmd" in the [docs] config section)
sends the query to qmd's BM25 search ("qmd search") instead of rg. qmd
searches its own default collections, not the roots, so every hit is
checked against the docs index the same way and anything outside it is
dropped. qmd is used only when asked for, never because it is installed.
A missing qmd, a failed qmd run, or qmd output that is not one JSON array
exits 2.`,
		Args: docsArgs("docs search", cobra.ExactArgs(1)),
		// Same reason as docs list: under --json a failure has already put its
		// ONE JSON object on stderr, and cobra's own error line would be a
		// second write to that stream.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			// The flag wins; without it [docs] search_backend decides, and
			// docsIndexOptions below rejects a bad config value by name.
			if !cmd.Flags().Changed("backend") {
				backend = deps.Cfg.Docs.SearchBackend
			} else if !docspkg.ValidSearchBackend(backend) {
				return docsFail(cmd, "docs search", "", fmt.Errorf("--backend must be %q or %q, not %q", docspkg.SearchBackendRipgrep, docspkg.SearchBackendQMD, backend), 2, asJSON)
			}
			if limit < 1 {
				return docsFail(cmd, "docs search", "", fmt.Errorf("--limit must be at least 1, not %d", limit), 2, asJSON)
			}
			if err := docspkg.ValidateQuery(query); err != nil {
				return docsFail(cmd, "docs search", "", err, 2, asJSON)
			}
			streamer, ok := deps.Runner.(forgexec.StreamingRunner)
			if !ok {
				return docsFail(cmd, "docs search", "", errors.New("docs search: streaming runner is unavailable"), 2, asJSON)
			}

			roots, err := resolveDocsRoots(nil, deps.Cfg.Docs)
			if err != nil {
				return docsFail(cmd, "docs search", "", err, 2, asJSON)
			}
			opts, err := docsIndexOptions(deps.Cfg.Docs)
			if err != nil {
				return docsFail(cmd, "docs search", "", err, 2, asJSON)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			idx, err := docspkg.NewIndexContext(ctx, roots, opts)
			if err != nil {
				return docsFail(cmd, "docs search", deadlineRoot(err, ""), err, 2, asJSON)
			}
			// Under --json stderr is reserved for the one error object (#649),
			// so the note stays out of it; the skipped paths travel in the
			// stdout payload where it has room (docs search's skipped_paths).
			if !asJSON {
				noteSkippedPaths(cmd.ErrOrStderr(), idx)
			}

			searcher := docspkg.Searcher{Runner: streamer, LookPath: docsSearchLookPath, Backend: backend}
			resp, err := searcher.Search(ctx, idx, query, limit)
			if err != nil {
				return docsFail(cmd, "docs search", "", err, 2, asJSON)
			}
			return printDocsSearch(cmd, resp, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"backend","query","results","truncated","skipped","errors","skipped_paths"} to stdout; a result is {"root","path","title","line","snippet"}`)
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "deadline for indexing plus search, e.g. 10s or 1m")
	cmd.Flags().IntVar(&limit, "limit", 50, "return at most N results")
	cmd.Flags().StringVar(&backend, "backend", "", `search backend, "ripgrep" or "qmd" (default: [docs] search_backend, else ripgrep)`)
	cmd.SetFlagErrorFunc(docsFlagError("docs search"))
	return cmd
}

// docsSearchPartialJSON is the --json wire shape of a partial `docs search`:
// some root could not be fully searched, so stdout still carries the full
// response and this object, on stderr, says why the exit is 1. It has the
// docsErrorJSON keys; root is empty because the failed roots are listed in the
// response's errors array.
type docsSearchPartialJSON = docsErrorJSON

// printDocsSearch writes the results, then turns any per-root failure into
// exit 1 with its reason on stderr: under --json one {"error","code","root"} object
// (stdout still carries the full response, errors array included), otherwise
// one line per failed root.
func printDocsSearch(cmd *cobra.Command, resp docspkg.SearchResponse, asJSON bool) error {
	if err := printDocsSearchResults(cmd, resp, asJSON); err != nil {
		// stdout failed mid-response, so no verdict stands: under --json
		// stderr gets the docs object, keeping the exit code 1 this has
		// always had, not the generic contract's string-code shape.
		return docsFail(cmd, "docs search", "", err, 1, asJSON)
	}
	if len(resp.Errors) == 0 {
		return nil
	}
	if asJSON {
		msg := fmt.Sprintf("docs search: %d root(s) could not be fully searched; see errors in the response", len(resp.Errors))
		obj := docsSearchPartialJSON{Error: msg, Code: 1}
		if encErr := termsafe.JSONEncoder(cmd.ErrOrStderr()).Encode(obj); encErr != nil {
			// The response is already on stdout and stderr just refused a
			// write, so there is nothing more to say: exit 1 silently rather
			// than let the generic contract try stderr again in its own
			// string-code shape.
			return jsonVerdict(WithExitCode(fmt.Errorf("docs search: encode error: %w", encErr), 1), true)
		}
		return newSilentCodedError(1)
	}
	for _, e := range resp.Errors {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "docs search: root %s: %s\n",
			termsafe.SafeLineMax(e.Root, docsSearchRootRunes),
			safeText(e.Message))
	}
	return newSilentCodedError(1)
}

func printDocsSearchResults(cmd *cobra.Command, resp docspkg.SearchResponse, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		return termsafe.JSONEncoder(out).Encode(resp)
	}
	if len(resp.Results) == 0 && len(resp.Errors) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no matches")
	}
	for _, r := range resp.Results {
		_, _ = fmt.Fprintf(out, "%s  %s:%d  %s\n",
			termsafe.SafeLineMax(r.Root, docsSearchRootRunes),
			termsafe.SafeLineMax(r.Path, docsSearchPathRunes),
			r.Line,
			termsafe.SafeLineMax(r.Snippet, docsSearchSnippetRunes))
	}
	if resp.Truncated {
		// Worded for both backends: under qmd, truncated can be set with
		// fewer than limit results shown (qmd's window came back full).
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "more matches may exist; narrow the query or raise --limit")
	}
	return nil
}
