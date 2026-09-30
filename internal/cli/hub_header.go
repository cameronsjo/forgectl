package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// hubHeaderBudget bounds the whole header read. Every source is local (git,
// tmux, the pr lifecycle store), so this is a stall guard — a wedged tmux
// server or a held lifecycle lock — not a network timeout.
const hubHeaderBudget = 750 * time.Millisecond

// hubHeaderSources are the header's local reads. Each reports ok=false when
// its value is unavailable, and the field is then left out of the line.
type hubHeaderSources struct {
	git     func(ctx context.Context) (project, branch string, ok bool)
	tmux    func(ctx context.Context) (sessions int, ok bool)
	reviews func(ctx context.Context) (running, queued int, ok bool)
}

// gatherHubHeader runs every source concurrently under one budget. A source
// still running when the budget expires is abandoned and its field omitted:
// the hub opens on time rather than waiting on it.
func gatherHubHeader(ctx context.Context, src hubHeaderSources, budget time.Duration) tui.HubHeader {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type result func(*tui.HubHeader)
	results := make(chan result, 3)
	pending := 0
	if src.git != nil {
		pending++
		go func() {
			project, branch, ok := src.git(ctx)
			results <- func(h *tui.HubHeader) {
				if ok {
					h.Project, h.Branch = project, branch
				}
			}
		}()
	}
	if src.tmux != nil {
		pending++
		go func() {
			n, ok := src.tmux(ctx)
			results <- func(h *tui.HubHeader) { h.TmuxSessions, h.HasTmux = n, ok }
		}()
	}
	if src.reviews != nil {
		pending++
		go func() {
			running, queued, ok := src.reviews(ctx)
			results <- func(h *tui.HubHeader) { h.ReviewsRunning, h.ReviewsQueued, h.HasReviews = running, queued, ok }
		}()
	}

	var h tui.HubHeader
	for ; pending > 0; pending-- {
		select {
		case apply := <-results:
			apply(&h)
		case <-ctx.Done():
			return h
		}
	}
	return h
}

// liveHubHeaderSources wires the header to this machine. The doctor field has
// no source: `forgectl doctor` records no result anywhere, so there is nothing
// local to read and the field is always omitted (forgectl#730).
func liveHubHeaderSources(run exec.Runner, client *tmux.Client) hubHeaderSources {
	return hubHeaderSources{
		git: func(ctx context.Context) (string, string, bool) {
			if run == nil {
				return "", "", false
			}
			return gitProjectBranch(ctx, run)
		},
		tmux: func(ctx context.Context) (int, bool) {
			if client == nil {
				return 0, false
			}
			sessions, err := client.ListSessions(ctx)
			if err != nil {
				return 0, false
			}
			return len(sessions), true
		},
		reviews: func(ctx context.Context) (int, int, bool) {
			return reviewCounts(ctx, pr.New(run))
		},
	}
}

// gitProjectBranch reads the cwd checkout's top-level directory name and
// branch in one local `git rev-parse`. A detached HEAD reports "HEAD", shown
// as "(detached)".
func gitProjectBranch(ctx context.Context, run exec.Runner) (project, branch string, ok bool) {
	out, err := run.Run(ctx, "git", "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" {
		return "", "", false
	}
	project = filepath.Base(strings.TrimSpace(lines[0]))
	branch = strings.TrimSpace(lines[1])
	if branch == "HEAD" {
		branch = "(detached)"
	}
	return project, branch, true
}

// reviewCounts counts the pr lifecycle store's records: queued ones, and
// running ones (every phase between a reserved slot and a live window, plus a
// legacy phaseless record, which pr treats as active). A needs-repair record
// or one whose workspace has gone missing is neither.
//
// It reads only a sessions directory that already exists — List would create
// it, and opening the hub is no reason to — and reports unavailable when any
// record was unreadable, because a short count would read as the truth.
func reviewCounts(ctx context.Context, client *pr.Client) (running, queued int, ok bool) {
	dir := client.SessionsDir()
	if dir == "" {
		return 0, 0, false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return 0, 0, false
	}
	summaries, unreadable, err := client.List(ctx)
	if err != nil || unreadable > 0 {
		return 0, 0, false
	}
	for _, s := range summaries {
		switch {
		case s.Phase() == pr.PhaseQueued:
			queued++
		case s.Phase() == pr.PhaseNeedsRepair, s.IsWorkspaceMissing():
		default:
			running++
		}
	}
	return running, queued, true
}

// hubArgSources feeds the hub's argument picker. Only local sources belong
// here: the projects walk is filesystem-only. The PR picker deliberately has
// none — `pr prs` is three `gh search prs` calls over the network, and the hub
// makes no network calls — so it takes free text (forgectl#730).
func hubArgSources() map[string]tui.ArgSource {
	projectNames := func(context.Context) []string {
		return projects.LocalNames(projects.ResolveRoot())
	}
	return map[string]tui.ArgSource{
		"projects pick":     projectNames,
		"projects list":     projectNames,
		"projects clone":    projectNames,
		"projects worktree": projectNames,
	}
}

// hubRunOptions assembles everything the hub screen shows: its rows, its
// header, and its picker sources.
func hubRunOptions(ctx context.Context, deps module.Deps, root *cobra.Command, client *tmux.Client) tui.RunOptions {
	return tui.RunOptions{
		Hub:        buildHub(root, configFilePresent(), recentCommands(root, readShellHistory(), hubRecentLimit)),
		Header:     gatherHubHeader(ctx, liveHubHeaderSources(deps.Runner, client), hubHeaderBudget),
		ArgSources: hubArgSources(),
		Theme:      deps.Theme,
	}
}
