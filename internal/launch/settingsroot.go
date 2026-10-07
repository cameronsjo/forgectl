package launch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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
// The walk runs on cwd's physical path. os.Getwd reports the logical $PWD, so
// a ~/link to /real/pkg would otherwise climb ~ instead of /real, while the
// launch profile, which follows symlinks, is resolved for /real/pkg. A moved
// launch gets the physical root; an unmoved one gets cwd back verbatim, so a
// caller detects a move by comparing the two strings.
//
// A root whose .claude is Claude Code's USER configuration directory (~/.claude,
// or $CLAUDE_CONFIG_DIR) is never a settings root: its settings.json is the
// user settings file, which applies everywhere already, and a git-tracked home
// directory would otherwise pull every launch outside a repository up to $HOME.
//
// Only the walk stops at .git. A settings directory between cwd and the
// repository root is not looked for, because Claude Code would not read it
// from cwd either. Any filesystem error other than "does not exist" stops the
// walk and keeps cwd: an unreadable directory is no evidence of a root.
func SettingsRoot(cwd string) string {
	return settingsRoot(cwd, userClaudeConfigDir())
}

func settingsRoot(cwd, userConfigDir string) string {
	start, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return cwd
	}
	if own, err := hasClaudeSettings(start); err != nil || own {
		return cwd
	}
	for dir := start; ; {
		isRoot, err := present(os.Lstat, filepath.Join(dir, ".git"))
		if err != nil {
			return cwd
		}
		if isRoot {
			if dir == start || isUserConfigDir(filepath.Join(dir, ".claude"), userConfigDir) {
				return cwd
			}
			if has, err := hasClaudeSettings(dir); err == nil && has {
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

// userClaudeConfigDir is Claude Code's user configuration directory:
// $CLAUDE_CONFIG_DIR when set, otherwise ~/.claude. "" when neither resolves,
// which disables the check rather than guessing at a home.
func userClaudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// isUserConfigDir reports whether candidate names the user configuration
// directory, comparing physical paths so a symlinked home still matches.
func isUserConfigDir(candidate, userConfigDir string) bool {
	if userConfigDir == "" {
		return false
	}
	return physical(candidate) == physical(userConfigDir)
}

func physical(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// hasClaudeSettings reports whether dir holds either settings file. os.Stat,
// because Claude Code follows a settings symlink: a dangling one is no file.
func hasClaudeSettings(dir string) (bool, error) {
	for _, name := range claudeSettingsFiles {
		ok, err := present(os.Stat, filepath.Join(dir, name))
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// present reports whether stat finds path. Only "does not exist" means absent
// (ENOTDIR included: a .claude that is a file holds no settings);
// any other error is returned so the caller stops rather than guesses. .git is
// probed with os.Lstat, because a .git that is a dangling symlink still marks
// where the repository's root was meant to be.
func present(stat func(string) (fs.FileInfo, error), path string) (bool, error) {
	_, err := stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	default:
		return false, err
	}
}
