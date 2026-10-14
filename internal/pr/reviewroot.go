package pr

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// reviewWorkspaceRoot is the directory review workspaces, and so their
// private temp and settings dirs, are made in: the OS temp dir, unless that
// is, or is inside, the operator's claude-<uid> dir, in which case the parent
// of that claude-<uid> dir (forgectl#907).
//
// That is the usual case when forgectl runs inside a Claude Code session:
// Claude Code gives its Bash TMPDIR=<root>/claude-<uid>, the very dir every
// other session of the operator's shares. A workspace made there would put
// the review's own dirs inside the shared one, and the sandbox's write deny
// on the shared dir (sharedClaudeTmpDirs) would then have to be dropped. Made
// in the parent, the deny always applies.
//
// A parent that is not an existing directory refuses the review rather than
// falling back to the shared dir.
func reviewWorkspaceRoot() (string, error) {
	tmp := filepath.Clean(osTempDir())
	uid := reviewUID()
	if uid < 0 {
		return tmp, nil
	}
	name := "claude-" + strconv.Itoa(uid)
	root, found := outsideNamedDir(tmp, name)
	if !found {
		resolved := resolvedOrSelf(tmp)
		root, found = outsideNamedDir(resolved, name)
	}
	if !found {
		return tmp, nil
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("refusing to make the review workspace: TMPDIR %q is inside Claude Code's shared %s dir, "+
			"and its parent %q is not a usable directory; set TMPDIR to a directory outside it", tmp, name, root)
	}
	return root, nil
}

// outsideNamedDir returns the parent of the outermost component of path named
// name, and whether there is one.
func outsideNamedDir(path, name string) (string, bool) {
	root, found := "", false
	for dir := path; ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == name {
			root, found = filepath.Dir(dir), true
		}
		if filepath.Dir(dir) == dir {
			return root, found
		}
	}
}
