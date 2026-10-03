package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/history"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// hubHeaderBudget bounds the whole header read, and the shell-history read
// beside it. Every source is local (.git files, tmux, the pr session store,
// $HISTFILE), so this is a stall guard — a wedged tmux server, a huge history
// file — not a network timeout.
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
//
// Nothing here writes: the project and branch come from reading .git files,
// not from a subprocess, and the review counts from pr's lock-free,
// read-only PeekCounts. A read that fails is logged at debug and its field
// omitted.
func liveHubHeaderSources(client *tmux.Client, reviews *pr.Client) hubHeaderSources {
	return hubHeaderSources{
		git: func(context.Context) (string, string, bool) {
			cwd, err := os.Getwd()
			if err != nil {
				slog.Debug("Hub header has no project: the working directory is unknown.", "error", err)
				return "", "", false
			}
			return gitProjectBranch(cwd)
		},
		tmux: func(ctx context.Context) (int, bool) {
			if client == nil {
				return 0, false
			}
			sessions, err := client.ListSessions(ctx)
			if err != nil {
				slog.Debug("Hub header has no tmux count.", "error", err)
				return 0, false
			}
			return len(sessions), true
		},
		reviews: func(context.Context) (int, int, bool) {
			if reviews == nil {
				return 0, 0, false
			}
			running, queued, err := reviews.PeekCounts()
			if err != nil {
				slog.Debug("Hub header has no review counts.", "error", err)
				return 0, 0, false
			}
			return running, queued, true
		},
	}
}

// gitMetaMaxBytes bounds a .git pointer file or HEAD read. Either is one short
// line; anything larger is not one.
const gitMetaMaxBytes = 4096

// gitProjectBranch finds the checkout containing dir by walking up to its
// .git marker, and reads its branch from HEAD — file reads only, no git
// subprocess, so it works in a repo with no commits (where `git rev-parse
// --abbrev-ref HEAD` fails) and costs nothing outside one. The project is the
// checkout's directory name; ok is false only when dir is inside no checkout.
//
// The branch is best-effort and fails toward omission. It is left out when:
//   - the .git marker or the git dir it names is not owned by this user —
//     git's own safe.directory default, since HEAD is then someone else's text;
//   - a file on the way is not a regular file (a FIFO would block the read, a
//     device would stream): each is Lstat'd first and opened O_NOFOLLOW|
//     O_NONBLOCK, then re-checked on the handle;
//   - .git is a symlink — even one to a git dir inside the repo. Following it
//     would let the header read HEAD wherever the link points; dropping the
//     branch there is the cheaper, rarer cost;
//   - HEAD is over gitMetaMaxBytes, or names something that is not a
//     plausible branch (validRefName).
//
// A detached HEAD shows as "(detached)".
func gitProjectBranch(dir string) (project, branch string, ok bool) {
	dir = filepath.Clean(dir)
	for {
		marker := filepath.Join(dir, ".git")
		info, err := os.Lstat(marker)
		if err == nil {
			return filepath.Base(dir), branchForMarker(marker, info, dir), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// branchForMarker resolves the git dir the .git marker names and reads its
// branch, or "" under any of gitProjectBranch's omission rules.
func branchForMarker(marker string, info os.FileInfo, checkout string) string {
	if !ownedByMe(info) {
		return ""
	}
	gitDir := marker
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		gitDir = gitDirFromPointer(marker, checkout)
		if gitDir == "" {
			return ""
		}
		dirInfo, err := os.Lstat(gitDir)
		if err != nil || !dirInfo.IsDir() || !ownedByMe(dirInfo) {
			return ""
		}
	default:
		return "" // a symlink, FIFO, or device named .git
	}
	return branchFromHead(gitDir)
}

// gitDirFromPointer resolves a .git FILE (a linked worktree or submodule) to
// the git dir it names, or "" when it names none.
func gitDirFromPointer(marker, checkout string) string {
	data, err := readSmallFile(marker)
	if err != nil {
		return ""
	}
	target, found := strings.CutPrefix(strings.TrimSpace(data), "gitdir:")
	if !found {
		return ""
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(checkout, target)
	}
	return filepath.Clean(target)
}

// branchFromHead reads gitDir/HEAD: a symbolic ref to refs/heads/<name> is
// that branch, a bare object id is a detached HEAD, anything else is "".
func branchFromHead(gitDir string) string {
	data, err := readSmallFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(data)
	if ref, found := strings.CutPrefix(head, "ref:"); found {
		name, isBranch := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/")
		if !isBranch || !validRefName(name) {
			return ""
		}
		return name
	}
	if (len(head) == 40 || len(head) == 64) && strings.Trim(head, "0123456789abcdef") == "" {
		return "(detached)"
	}
	return ""
}

// validRefName is a conservative subset of git's check-ref-format rules for
// a branch name: printable ASCII only, no space or the characters git
// forbids, no "..", "@{", "//", and no leading "-" or "/", nor a trailing
// "/", "." or ".lock". A name it refuses is omitted, never shown.
func validRefName(name string) bool {
	if name == "" || len(name) > 255 || name == "@" || name == ".invalid" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r > '~' || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	switch {
	case strings.Contains(name, ".."), strings.Contains(name, "@{"), strings.Contains(name, "//"),
		strings.HasPrefix(name, "-"), strings.HasPrefix(name, "/"), strings.HasPrefix(name, "."),
		strings.HasSuffix(name, "/"), strings.HasSuffix(name, "."), strings.HasSuffix(name, ".lock"),
		strings.Contains(name, "/."):
		return false
	}
	return true
}

// readSmallFile reads a regular file of at most gitMetaMaxBytes without
// following a symlink or blocking: Lstat first and require a regular file,
// open O_NOFOLLOW|O_NONBLOCK (openGitMeta), then re-check the open handle is
// the same regular file, so a FIFO or device swapped in between is refused.
func readSmallFile(path string) (string, error) {
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	f, err := openGitMeta(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return "", errors.New("the file changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(f, gitMetaMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > gitMetaMaxBytes {
		return "", errors.New("too large")
	}
	return string(data), nil
}

// hubArgSources feeds the hub's argument picker. Only local sources belong
// here: the projects walk is filesystem-only. The PR picker deliberately has
// none — `pr prs` is three `gh search prs` calls over the network, and the hub
// makes no network calls — so it takes free text (forgectl#730).
func hubArgSources() map[string]tui.ArgSource {
	projectNames := func(context.Context) []string {
		root, err := projects.ResolveRoot()
		if err != nil {
			// Like the header's own fields, an unavailable picker source is
			// omitted, not reported: the hub opens with free text (forgectl#730).
			return nil
		}
		return projects.LocalNames(root)
	}
	return map[string]tui.ArgSource{
		"projects pick":     projectNames,
		"projects list":     projectNames,
		"projects clone":    projectNames,
		"projects worktree": projectNames,
	}
}

// hubState reads the hub's local state: its header, and the shell history that
// ranks the recent section. The history read runs beside the header sources
// under the same budget; if it has not finished by then, entries is nil and
// the hub has no recent section. The TUI hub and `menu` both read through it.
func hubState(ctx context.Context, deps module.Deps, client *tmux.Client) (tui.HubHeader, []history.Entry) {
	deadline := time.Now().Add(hubHeaderBudget)
	historyDone := make(chan []history.Entry, 1)
	go func() { historyDone <- readShellHistory() }()

	header := gatherHubHeader(ctx, liveHubHeaderSources(client, pr.New(deps.Runner)), hubHeaderBudget)

	var entries []history.Entry
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case entries = <-historyDone:
	case <-timer.C:
		slog.Debug("Hub opened without a recent section; shell history took longer than the budget.")
	}
	return header, entries
}

// hubRunOptions assembles everything the hub screen shows: its rows, its
// header, and its picker sources.
func hubRunOptions(ctx context.Context, deps module.Deps, root *cobra.Command, client *tmux.Client) tui.RunOptions {
	header, entries := hubState(ctx, deps, client)
	return tui.RunOptions{
		Hub:        buildHub(root, configFilePresent(), recentCommands(root, entries, hubRecentLimit)),
		Header:     header,
		ArgSources: hubArgSources(),
		BuildArgv:  hubPickerArgv(root),
		Theme:      deps.Theme,
	}
}
