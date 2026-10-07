package launch

import (
	"os"
	"path/filepath"
)

// claudeSettingsFiles are the project settings files Claude Code reads from
// its launch directory. It reads them from that directory only: measured on
// Claude Code 2.1.292, a session started in a subfolder of a repository runs
// without the repository root's env, hooks, and permissions, and says nothing
// (cameronsjo/cadence-ecosystem#608).
var claudeSettingsFiles = []string{
	filepath.Join(".claude", "settings.json"),
	filepath.Join(".claude", "settings.local.json"),
}

// SettingsRoot is the directory a claude session started from cwd should run
// in so that it reads the repository's project settings.
//
// When cwd holds its own .claude/settings.json or .claude/settings.local.json,
// it is the answer: a subfolder that carries its own settings meant them.
// Otherwise SettingsRoot walks up to the nearest directory holding a .git
// entry, either a directory or the file a linked worktree has, and returns it
// when it holds either settings file. In every other case, no repository above
// cwd or a repository root with no settings, cwd comes back unchanged, so a
// launch outside a configured repository runs exactly where it was started.
//
// Only the walk stops at .git. A settings directory between cwd and the
// repository root is not looked for, because Claude Code would not read it
// from cwd either; the repository root is the one place a project's settings
// are expected to live.
func SettingsRoot(cwd string) string {
	start := filepath.Clean(cwd)
	if hasClaudeSettings(start) {
		return cwd
	}
	for dir := start; ; {
		if pathExists(filepath.Join(dir, ".git")) {
			if dir != start && hasClaudeSettings(dir) {
				return dir
			}
			return cwd
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
		dir = parent
	}
}

func hasClaudeSettings(dir string) bool {
	for _, name := range claudeSettingsFiles {
		if pathExists(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// pathExists reports whether path names anything at all. Lstat, because a .git
// that is a dangling symlink still marks where the repository's root was meant
// to be, and a settings file is followed by Claude Code itself.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
