package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface tag <name>` stamps display tokens on a worker's pane, for a
// scheduler that wants its task or issue id in herdr's sidebar
// (forgectl#1239).

// tagTimeout bounds the ownership check and the report.
const tagTimeout = 30 * time.Second

// tagResult is what `surface tag --json` prints. Additive changes only
// (ADR-0008 rule 2).
type tagResult struct {
	Name    string            `json:"name"`
	PaneID  string            `json:"pane_id"`
	Set     map[string]string `json:"set"`
	Cleared []string          `json:"cleared"`
}

type tagOptions struct {
	Repo string
	// Surface is the worker surface: herdr (the default) or hc.
	Surface string
	Name    string
	Tokens  []string
	Clear   []string
	JSON    bool
}

func newSurfaceTagCmd(deps module.Deps) *cobra.Command {
	opts := tagOptions{}
	cmd := &cobra.Command{
		Use:   "tag <name>",
		Short: "Set or clear display tokens on a worker's herdr pane",
		Long: `tag sets or clears display tokens on a launched worker's root pane in herdr
(herdr pane report-metadata, source forgectl-surface). A herdr sidebar row
shows a token named k as $k, for example $task or $issue. Keys share herdr's
token map with every other reporter, and herdr's own keys (session, cwd,
worktree, branch) are taken, so pick one nothing else sets. Tokens are display
only: they change no agent state, wait or notification.

--token k=v sets a token and --clear-token k clears one; each repeats, up to
16 in all. A key is 1-32 ASCII letters, digits, '_' or '-', not starting
with '-'. A value is 1-80
characters with no control, bidi or invisible character and no leading or
trailing space, so herdr stores exactly what was sent. A key may appear once.

The pane is written only after the checks every write to a worker makes: its
workspace carries forgectl's ownership marker and herdr places the pane in
it.

--json prints {"name","pane_id","set","cleared"}.

Exit 0: tagged. Exit 1: the worker's workspace is gone, herdr could not be
read, or herdr refused the report. Exit 2: a usage error (no such worker, a
worker not at stage launched, a bad token).

  forgectl surface tag fix-login --token task=PAP-12 --token issue=forgectl#1239
  forgectl surface tag fix-login --clear-token task --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceTag(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	addWorkerSurfaceFlag(cmd, &opts.Surface)
	cmd.Flags().StringArrayVar(&opts.Tokens, "token", nil, "set a display token, k=v (repeatable)")
	cmd.Flags().StringArrayVar(&opts.Clear, "clear-token", nil, "clear a display token by key (repeatable)")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","pane_id","set","cleared"} as JSON`)
	return cmd
}

// parseTagTokens turns --token k=v flags into a map, refusing a malformed or
// repeated key before anything reaches herdr.
func parseTagTokens(tokens []string) (map[string]string, error) {
	set := make(map[string]string, len(tokens))
	for _, t := range tokens {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("--token %q: want k=v", termsafe.SafeLineMax(t, 120))
		}
		if err := herdradapter.CheckTokenKey(k); err != nil {
			return nil, err
		}
		if err := herdradapter.CheckTokenValue(k, v); err != nil {
			return nil, err
		}
		if _, dup := set[k]; dup {
			return nil, fmt.Errorf("--token %s is given twice", k)
		}
		set[k] = v
	}
	return set, nil
}

func runSurfaceTag(cmd *cobra.Command, deps module.Deps, opts tagOptions) error {
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	if len(opts.Tokens) == 0 && len(opts.Clear) == 0 {
		return WithExitCode(errors.New("give at least one --token k=v or --clear-token k"), exitUsage)
	}
	set, err := parseTagTokens(opts.Tokens)
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	// The cleared keys, a key both set and cleared, and the per-report cap
	// are checked here too, before the ledger or herdr is touched.
	if _, _, err := herdradapter.CheckTag(set, opts.Clear); err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	if err := checkWorkerSurface(opts.Surface); err != nil {
		return err
	}
	// Tags are herdr sidebar metadata tokens, which herdr-core has no
	// equivalent of yet.
	if opts.Surface == workerSurfaceHc {
		return WithExitCode(errors.New("surface tag supports herdr workers only; hc has no sidebar metadata"), exitUsage)
	}
	w, err := openWorkerLedger(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo)
	if err != nil {
		return err
	}
	_, ref, err := launchedWorker(w.led, opts.Name)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), tagTimeout)
	defer cancel()
	pane, err := w.herdr.TagPane(ctx, ref, set, opts.Clear)
	switch {
	case errors.Is(err, herdradapter.ErrBadToken):
		return WithExitCode(termsafe.Error(err), exitUsage)
	case err != nil:
		return WithExitCode(termsafe.Error(fmt.Errorf("worker %s: %w", opts.Name, err)), exitFailed)
	}
	res := tagResult{Name: opts.Name, PaneID: pane, Set: set, Cleared: opts.Clear}
	if res.Cleared == nil {
		res.Cleared = []string{}
	}
	if opts.JSON {
		return writeJSON(cmd.OutOrStdout(), res)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: tagged pane %s (%s set, %s cleared)\n",
		opts.Name, termsafe.SafeLineMax(pane, 64), plural(len(set), "token", "tokens"), plural(len(res.Cleared), "token", "tokens"))
	return err
}
