package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// docsListProgressDelay is how long `docs list` waits with no output before
// telling the operator which root it's still walking — long enough that a
// normal, fast index never prints it, short enough that a hung or
// cloud-backed root doesn't read as the command having stalled silently.
// A var so tests can shrink it to force the progress path.
var docsListProgressDelay = 2 * time.Second

// newDocsListCmd builds `forgectl docs list [dir|file ...]` — lists the
// indexed doc set without binding a server.
func newDocsListCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	var timeout time.Duration
	var limit int

	cmd := &cobra.Command{
		Use:   "list [dir|file ...]",
		Short: "List the indexed docs, most-recently-modified first",
		Args:  cobra.ArbitraryArgs,
		// SilenceUsage/SilenceErrors mirror env.go's own setting: a deadline
		// error under --json has already put its ONE JSON object on stderr
		// (docsFail); cobra's own "Error: ..." line and usage
		// block would be a second, conflicting write to the same stream.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Exit contract, as docs check: 0 listed (an empty list included);
			// 2 the list could not be produced (bad flag, bad root, deadline).
			if limit < 0 {
				return docsFail(cmd, "docs list", "", fmt.Errorf("--limit must be 0 or a positive count, not %d", limit), 2, asJSON)
			}

			roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
			if err != nil {
				return docsFail(cmd, "docs list", "", err, 2, asJSON)
			}
			opts, err := docsIndexOptions(deps.Cfg.Docs)
			if err != nil {
				return docsFail(cmd, "docs list", "", err, 2, asJSON)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			progressRoot := ""
			if len(roots) > 0 {
				progressRoot = roots[0]
			}
			// mu and printed together make the progress line and any later
			// write to the same stderr sink mutually exclusive: the AfterFunc
			// callback and RunE's own goroutine never call cmd.ErrOrStderr()
			// concurrently (that race corrupted interleaved writes), and
			// setting printed under the lock before RunE writes anything else
			// also stops a progress line from appearing AFTER the result —
			// timer.Stop() alone only prevents a FUTURE fire; it does not
			// wait for one already in flight.
			var mu sync.Mutex
			printed := false
			timer := time.AfterFunc(docsListProgressDelay, func() {
				mu.Lock()
				defer mu.Unlock()
				if printed {
					return
				}
				printed = true
				// Under --json stderr is reserved for the one error object
				// (#649, #672): a slow walk that then hits its deadline would
				// otherwise put this text line ahead of it.
				if asJSON {
					return
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "indexing %s …\n", safeColumnPath(progressRoot))
			})
			defer timer.Stop()

			idx, err := docspkg.NewIndexContext(ctx, roots, opts)
			mu.Lock()
			printed = true
			mu.Unlock()
			timer.Stop()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return docsFail(cmd, "docs list", deadlineRoot(err, progressRoot), err, 2, asJSON)
				}
				return docsFail(cmd, "docs list", "", err, 2, asJSON)
			}
			// Under --json stderr is reserved for the one error object (#649),
			// so the note stays out of it. The bare-array output has no room for
			// the skipped paths either; docs check --json lists them.
			if !asJSON {
				noteSkippedPaths(cmd.ErrOrStderr(), idx)
			}

			docs := idx.List()
			if limit > 0 && limit < len(docs) {
				docs = docs[:limit]
			}
			if err := printDocsList(cmd, docs, asJSON); err != nil {
				// stdout refused the list: under --json stderr gets the docs
				// integer-code object, keeping the exit code 1 this has always
				// had, not the generic contract's string-code shape.
				return docsFail(cmd, "docs list", "", err, 1, asJSON)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"root","path","title","modTime"}] to stdout (an empty list is [])`)
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "walk deadline, e.g. 15s or 2m")
	cmd.Flags().IntVar(&limit, "limit", 0, "print only the first N entries, after the full walk completes (0 or unset: no limit)")
	cmd.SetFlagErrorFunc(docsFlagError("docs list"))
	return cmd
}

// deadlineRoot reports which root a deadline stopped on: the exact root a
// *docspkg.WalkDeadlineError carries, when the error is one, since
// NewIndexContext may be walking any of several roots and the one that
// actually stalled is not necessarily the caller's first. fallback is used
// only for an error that reached this path without that type (defensive; no
// known caller of NewIndexContext returns one otherwise).
func deadlineRoot(err error, fallback string) string {
	var deadline *docspkg.WalkDeadlineError
	if errors.As(err, &deadline) {
		return deadline.Root
	}
	return fallback
}

// docJSON is the --json wire shape for one entry of `forgectl docs list`.
type docJSON struct {
	Root    string    `json:"root"`
	Path    string    `json:"path"`
	Title   string    `json:"title"`
	ModTime time.Time `json:"modTime"`
}

func printDocsList(cmd *cobra.Command, docs []docspkg.Doc, asJSON bool) error {
	out := cmd.OutOrStdout()

	if asJSON {
		wire := make([]docJSON, len(docs))
		for i, d := range docs {
			wire[i] = docJSON{Root: d.RootLabel, Path: d.RelPath, Title: d.Title, ModTime: d.ModTime}
		}
		enc := termsafe.JSONEncoder(out)
		return enc.Encode(wire)
	}

	if len(docs) == 0 {
		fmt.Fprintln(out, "no docs found")
		return nil
	}
	// Every field is escaped: RelPath is a filename and Title is the doc's own
	// H1, so either can carry a terminal escape sequence (forgectl#598). The
	// title is also capped (forgectl#894), and so are the root label and the
	// path (#913): the path unquoted and cut in the middle, so an ordinary row
	// keeps the %-48s column. --json above carries every field whole.
	for _, d := range docs {
		_, _ = fmt.Fprintf(out, "%-16s %-48s %s\n",
			safeLabel(d.RootLabel), safeColumnPath(d.RelPath), safeTitle(d.Title))
	}
	return nil
}
