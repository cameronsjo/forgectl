// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/sweep"
)

func runSweep(cmd *cobra.Command, deps module.Deps, o sweepOpts) error {
	p, err := resolveSweepParams(deps.Cfg, o)
	if err != nil {
		return err
	}
	base, err := attentionStateBase(deps)
	if err != nil {
		return err
	}
	if deps.Runner == nil {
		return errors.New("sweep: no command runner available")
	}
	host, err := githubauth.ResolveHost(deps.Cfg.Github.Host)
	if err != nil {
		return sweepUsage("sweep: invalid [github] host")
	}
	ctx := cmd.Context()
	now := attentionNow().UTC()

	briefs, skipped, err := sweep.ReadBriefs(p.briefs, deps.Cfg.Attention.Prefix(), p.since)
	if err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	if skipped > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "sweep: skipped %d worker directories (symlinks or over the cap)\n", skipped) //nolint:errcheck // best-effort note on stderr
	}

	// GitHub is the one source the ledger cannot do without: a ledger with
	// no PRs would erase the queue's marker-clean rows, so a failed read
	// stops here and the last good file stays.
	gh := sweep.GitHub{Run: githubauth.Runner(deps.Runner, host)}
	open, openCut, err := gh.OpenPRs(ctx, p.owner)
	if err != nil {
		return err
	}
	merged, mergedCut, err := gh.MergedPRs(ctx, p.owner, p.since)
	if err != nil {
		return err
	}

	h := attentionHerdr(ctx, deps)
	items, deskState := attentionDesk()
	deskPanes := map[string]bool{}
	for _, it := range items {
		if attention.ValidPane(it.SignalPane) {
			deskPanes[it.SignalPane] = true
		}
	}
	agents := make([]sweep.Agent, 0, len(h.Agents))
	for _, a := range h.Agents {
		agents = append(agents, sweep.Agent{Pane: a.Pane, Status: a.Status, Names: a.Names})
	}

	sources := sweep.Sources{GitHub: sourceOK, Herdr: sourceOK, Desk: deskState, Briefs: sourceOK}
	if openCut || mergedCut {
		sources.GitHub = "truncated"
	}
	if !h.Known {
		sources.Herdr = sourceUnavailable
	}
	ledger := sweep.Build(sweep.Input{
		Now: now, Since: p.since, Reviewers: p.reviewers, Briefs: briefs, Agents: agents,
		DeskPanes: deskPanes, Open: open, Merged: merged,
		Filed: sweep.ReadFiled(parentDir(p.briefs)), Sources: sources,
	})
	data, err := sweep.Marshal(ledger)
	if err != nil {
		return err
	}
	if err := sweep.Write(base, data); err != nil {
		return err
	}
	if o.asJSON {
		return writeJSON(cmd.OutOrStdout(), ledger)
	}
	return printSweepSummary(cmd, ledger, sweep.Path(base))
}

func printSweepSummary(cmd *cobra.Command, l sweep.Ledger, path string) error {
	var clean, blocked, act int
	for _, p := range l.PRs {
		switch p.State {
		case sweep.StateMarkerClean:
			clean++
		case sweep.StateBlocked:
			blocked++
		}
	}
	for _, o := range l.Opinions {
		if o.Tone == sweep.ToneAct {
			act++
		}
	}
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("%d workers, %d open PRs (%d marker-clean, %d blocked), %d merged, %d need action\n",
		len(l.Workers), l.OpenPRs, clean, blocked, l.Merged, act)
	w.printf("wrote %s\n", safeText(path))
	if l.Sources.GitHub != sourceOK || l.Sources.Herdr != sourceOK {
		w.printf("sources: github %s, herdr %s, desk %s\n", l.Sources.GitHub, l.Sources.Herdr, l.Sources.Desk)
	}
	return w.err
}

// parentDir is the directory holding the briefs directory, where the
// chief-of-staff keeps filed.txt.
func parentDir(briefs string) string { return filepath.Dir(briefs) }
