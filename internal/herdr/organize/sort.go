package organize

import (
	"path/filepath"
	"strings"
)

// outsideRoot is the wing a tab files under when its cwd is not below the
// projects root. It sorts after every ordinary name.
const outsideRoot = "~"

const worktreesMarker = "/.claude/worktrees"

// sortKey orders tabs within a workspace: wing, repo, cwd, then terminal id.
type sortKey struct {
	wing     string
	repo     string
	cwd      string
	terminal string
}

// sortKeyFor builds the key for a tab whose sort pane has the given cwd. The
// cwd part drops a `/.claude/worktrees/...` suffix so a worktree files with
// its repo. Ties break on the tab's terminal id, compared as a string. The
// script broke them on the tab id, but herdr renumbers a tab that changes
// workspace, so equal-cwd tabs could swap on the next run instead of
// converging; a terminal id survives every move.
func sortKeyFor(root, cwd, terminalID string) sortKey {
	stripped := stripWorktree(cwd)
	wing, repo := wingAndRepo(root, stripped)
	return sortKey{wing: wing, repo: repo, cwd: stripped, terminal: terminalID}
}

func stripWorktree(cwd string) string {
	if i := strings.Index(cwd, worktreesMarker+"/"); i >= 0 {
		return cwd[:i]
	}
	return strings.TrimSuffix(cwd, worktreesMarker)
}

// wingAndRepo returns the first two path parts of cwd under root. A cwd that
// is empty, relative, the root itself, or outside the root returns the
// outside-root wing. Containment is checked with filepath.Rel, so
// "/Projects2/x" is outside "/Projects".
func wingAndRepo(root, cwd string) (wing, repo string) {
	if cwd == "" || root == "" {
		return outsideRoot, ""
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return outsideRoot, ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (k sortKey) less(o sortKey) bool {
	if k.wing != o.wing {
		return k.wing < o.wing
	}
	if k.repo != o.repo {
		return k.repo < o.repo
	}
	if k.cwd != o.cwd {
		return k.cwd < o.cwd
	}
	return k.terminal < o.terminal
}
