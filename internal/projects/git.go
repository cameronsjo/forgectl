package projects

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// gitStatus runs git in dir and returns a populated GitStatus. Returns a
// zero-value GitStatus for non-git directories — callers treat it as empty.
func gitStatus(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, dir string) GitStatus {
	if _, err := os.Stat(dir + "/.git"); err != nil {
		slog.Debug("git status: directory is not a git repo.", "dir", dir)
		return GitStatus{State: TreeNotRepo}
	}

	porcelain, err := run.Run(ctx, "git", "-C", dir, "status", "--porcelain")
	if err != nil {
		slog.Warn("git status: status command failed.", "dir", dir, "error", err)
		return GitStatus{State: TreeUnknown}
	}

	gs := GitStatus{State: TreeOK}
	if porcelain == "" {
		// Clean working tree — check for unpushed commits.
		ahead, _ := run.Run(ctx, "git", "-C", dir, "rev-list", "--count", "@{upstream}..HEAD")
		gs.Ahead = atoi(strings.TrimSpace(ahead))
		slog.Debug("git status: clean working tree.", "dir", dir, "ahead", gs.Ahead)
	} else {
		for _, line := range strings.Split(porcelain, "\n") {
			if len(line) < 2 {
				continue
			}
			if strings.HasPrefix(line, "??") {
				gs.Untracked++
			} else {
				gs.Modified++
			}
		}
		slog.Debug("git status: dirty working tree.", "dir", dir, "modified", gs.Modified, "untracked", gs.Untracked)
	}
	return gs
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
