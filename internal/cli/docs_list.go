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
const docsListProgressDelay = 2 * time.Second

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
		// (reportDocsListDeadline); cobra's own "Error: ..." line and usage
		// block would be a second, conflicting write to the same stream.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return fmt.Errorf("--limit must be 0 or a positive count, not %d", limit)
			}

			roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
			if err != nil {
				return err
			}
			opts, err := docsIndexOptions(deps.Cfg.Docs)
			if err != nil {
				return err
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
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "indexing %s …\n", termsafe.SafeLine(progressRoot))
			})
			defer timer.Stop()

			idx, err := docspkg.NewIndexContext(ctx, roots, opts)
			mu.Lock()
			printed = true
			mu.Unlock()
			timer.Stop()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return reportDocsListDeadline(cmd, deadlineRoot(err, progressRoot), err, asJSON)
				}
				return err
			}

			docs := idx.List()
			if limit > 0 && limit < len(docs) {
				docs = docs[:limit]
			}
			return printDocsList(cmd, docs, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "walk deadline, e.g. 15s or 2m")
	cmd.Flags().IntVar(&limit, "limit", 0, "print only the first N entries, after the full walk completes (0 or unset: no limit)")
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

// docsListDeadlineError is docs list's own silent-coded-error type: rendered
// through termsafeErrorHandler as nothing at all, because reportDocsListDeadline
// already wrote everything the operator gets — the human message on stderr, or
// (under --json) the one JSON error object. Wrapping it in WithExitCode is
// what still gets exit 2 to `main` (exitcode.go's ExitCode walks the chain for
// a *codedError, so this type composes with it rather than replacing it).
//
// Task 3 (a parallel PR, forgectl#481) is adding a general-purpose
// silentCodedError{code int} type to execute.go for the same "render nothing,
// carry a code" shape. This task does not wait on that merge — it defines its
// own minimal equivalent here, and Task 1's polish reconciles the two into
// one shared type.
type docsListDeadlineError struct {
	err error
}

func (e *docsListDeadlineError) Error() string { return e.err.Error() }
func (e *docsListDeadlineError) Unwrap() error { return e.err }

// docsListDeadlineJSON is the --json wire shape for a `docs list` deadline
// failure: stdout stays empty and this is the only thing written to stderr.
type docsListDeadlineJSON struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
	Root  string `json:"root"`
}

// reportDocsListDeadline handles a walk that stopped on ctx.Err(): under
// --json it writes exactly one JSON object to stderr and leaves stdout
// untouched (printDocsList is never called), then returns a silent error so
// termsafeErrorHandler renders nothing more; otherwise it lets the normal
// human-readable error path render walkErr, which already names the root
// (NewIndexContext). Either way the process exits 2.
func reportDocsListDeadline(cmd *cobra.Command, root string, walkErr error, asJSON bool) error {
	if !asJSON {
		return WithExitCode(walkErr, 2)
	}
	obj := docsListDeadlineJSON{Error: walkErr.Error(), Code: 2, Root: root}
	enc := termsafe.JSONEncoder(cmd.ErrOrStderr())
	if encErr := enc.Encode(obj); encErr != nil {
		return WithExitCode(fmt.Errorf("docs list: encode deadline error: %w", encErr), 2)
	}
	return WithExitCode(&docsListDeadlineError{err: walkErr}, 2)
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
	for _, d := range docs {
		fmt.Fprintf(out, "%-16s %-48s %s\n", d.RootLabel, d.RelPath, d.Title)
	}
	return nil
}
