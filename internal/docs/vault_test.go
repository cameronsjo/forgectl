package docs

// Test plan for vault.go (detectRootKind)
//
// detectRootKind (Classification: pure classifier over the filesystem)
//   [x] Happy: a root that IS a vault (has .obsidian directly) -> RootVault
//   [x] Happy: a root with no .obsidian anywhere up to the real $HOME -> RootDocs
//   [x] Unhappy: a .obsidian directory sitting inside a SIMULATED $HOME is
//       never treated as a vault — the $HOME stop is inclusive and fires
//       before the .obsidian check at that same directory. This is the
//       literal real-machine bug the Global Constraint calls out
//       (~/.obsidian exists on the machine this was built on).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectRootKind_VaultRoot(t *testing.T) {
	canonical, err := CanonicalizeRoot(filepath.Join("testdata", "links", "vault"))
	if err != nil {
		t.Fatalf("CanonicalizeRoot: %v", err)
	}

	kind, vaultPath := detectRootKind(canonical)
	if kind != RootVault {
		t.Fatalf("detectRootKind(vault fixture) kind = %v, want RootVault", kind)
	}
	if vaultPath != canonical {
		t.Errorf("detectRootKind(vault fixture) vaultPath = %q, want %q", vaultPath, canonical)
	}
}

func TestDetectRootKind_DocsRoot(t *testing.T) {
	canonical, err := CanonicalizeRoot(filepath.Join("testdata", "links", "repo"))
	if err != nil {
		t.Fatalf("CanonicalizeRoot: %v", err)
	}

	kind, vaultPath := detectRootKind(canonical)
	if kind != RootDocs {
		t.Fatalf("detectRootKind(repo fixture) kind = %v, want RootDocs", kind)
	}
	if vaultPath != "" {
		t.Errorf("detectRootKind(repo fixture) vaultPath = %q, want empty", vaultPath)
	}
}

// TestDetectRootKind_HomeBoundaryStopsBeforeObsidianAtHome pins the
// inclusive-$HOME-stop rule: a ".obsidian" directory sitting directly
// inside $HOME must never classify $HOME's own subtree as a vault, even
// though the walk-up would otherwise find it. Without this stop, a root
// nested under a real home directory that happens to carry a top-level
// Obsidian vault (as this repo's build machine does) would be
// misclassified.
func TestDetectRootKind_HomeBoundaryStopsBeforeObsidianAtHome(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	childRoot := filepath.Join(home, "project", "childRoot")
	if err := os.MkdirAll(filepath.Join(home, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(childRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	// t.TempDir() is not itself symlink-resolved on macOS (/var vs. the
	// canonical /private/var), while CanonicalizeRoot below DOES resolve
	// symlinks — so the comparison inside detectRootKind needs $HOME
	// canonicalized the same way, or "cur == home" never matches.
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	restore := userHomeDir
	userHomeDir = func() string { return filepath.Clean(realHome) }
	t.Cleanup(func() { userHomeDir = restore })

	canonical, err := CanonicalizeRoot(childRoot)
	if err != nil {
		t.Fatalf("CanonicalizeRoot: %v", err)
	}

	kind, vaultPath := detectRootKind(canonical)
	if kind != RootDocs {
		t.Errorf("detectRootKind(childRoot under simulated $HOME with .obsidian AT $HOME) kind = %v, want RootDocs", kind)
	}
	if vaultPath != "" {
		t.Errorf("detectRootKind(childRoot under simulated $HOME with .obsidian AT $HOME) vaultPath = %q, want empty", vaultPath)
	}
}
