package projects

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// gitStatus runs git in dir and returns a populated GitStatus. Returns
// StatusNotRepo when dir has no .git, and StatusUnknown when the git command
// itself fails — neither is ever read as StatusOK, so a caller can't mistake
// "never checked" or "check failed" for a clean tree.
func gitStatus(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, dir string) GitStatus {
	if _, err := os.Stat(dir + "/.git"); err != nil {
		return GitStatus{State: StatusNotRepo}
	}

	porcelain, err := run.Run(ctx, "git", "-C", dir, "status", "--porcelain")
	if err != nil {
		return GitStatus{State: StatusUnknown}
	}

	gs := GitStatus{State: StatusOK}
	if porcelain == "" {
		// Clean working tree — check for unpushed commits.
		ahead, _ := run.Run(ctx, "git", "-C", dir, "rev-list", "--count", "@{upstream}..HEAD")
		gs.Ahead = atoi(strings.TrimSpace(ahead))
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
	}
	return gs
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
