package docs

import (
	"os"
	"path/filepath"
)

// userHomeDir returns the boundary detectRootKind's walk-up stops at
// (inclusive — the home directory itself is never a vault, even when
// ~/.obsidian exists). A package-level func var, rather than a direct
// os.UserHomeDir() call, so a test can simulate a synthetic $HOME without
// mutating the real environment.
var userHomeDir = defaultUserHomeDir

// defaultUserHomeDir returns $HOME in the same canonical form the walk
// compares against — symlink-resolved, since the root it is compared to
// already is. A $HOME that is itself a symlink (as macOS temp dirs and
// some network homes are) would otherwise never equal any walked path,
// and the boundary would silently not exist.
func defaultUserHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	return filepath.Clean(home)
}

// detectRootKind walks up from canonical — which must already be a
// canonical, symlink-resolved absolute directory (see CanonicalizeRoot) —
// looking for a ".obsidian" directory, per the Global Constraint's vault
// heuristic. The walk-up stops, in priority order, at:
//
//   - $HOME itself (inclusive): ~/.obsidian exists on the machine this was
//     built on, and an unbounded walk would call the home directory a
//     vault.
//   - a device change (crossing a mount boundary), where deviceOf (the
//     platform-specific half; see vault_dev_unix.go / vault_dev_other.go)
//     can determine one. Platforms with no device-of implementation skip
//     this stop condition and rely on the other two.
//   - "/" (no parent left to walk to).
//
// Returns (RootVault, vaultPath) when a ".obsidian" directory was found
// before any stop condition — vaultPath is the directory that contains it.
// Otherwise returns (RootDocs, "").
func detectRootKind(canonical string) (RootKind, string) {
	home := userHomeDir()
	cur := filepath.Clean(canonical)
	startDev, hasDev := deviceOf(cur)

	for {
		if home != "" && cur == home {
			return RootDocs, ""
		}
		if info, err := os.Stat(filepath.Join(cur, ".obsidian")); err == nil && info.IsDir() {
			return RootVault, cur
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			return RootDocs, ""
		}
		if hasDev {
			// A parent that cannot be stat'd stops the walk too: the
			// boundary can no longer be proven, and RootDocs is the
			// default a root keeps when nothing proves it is a vault.
			dev, ok := deviceOf(parent)
			if !ok || dev != startDev {
				return RootDocs, ""
			}
		}
		cur = parent
	}
}
