package docs

// Test plan for index.go (Backlinks, IndexOptions/NewIndexWithOptions)
//
// Backlinks (Classification: reverse index over ResolveLink; no I/O of its own)
//   [x] Happy: a doc with real linkers lists them, sorted by RelPath
//   [x] Happy: a doc nothing links to returns empty, never panics
//   [x] Happy: Rebuild() on a modified copy of the fixture picks up a new
//       linker; the ORIGINAL index (built before the copy changed) does not
//   [x] Unhappy: a single-file root's link to a doc in a directory root does
//       not appear in that doc's Backlinks (cross-root isolation)
//   [x] Happy: a single-file root's fragment-only self-link still resolves
//       through ResolveLink (it must not surface itself as its own backlink)
//
// IndexOptions / NewIndexWithOptions (Classification: construction-time
// override of detectRootKind's filesystem inference)
//   [x] Happy: overriding a docs-kind fixture root to RootVault flips its
//       link semantics — a bare basename now resolves via byName

import (
	"os"
	"path/filepath"
	"testing"
)

// copyLinksFixture copies testdata/links (a fixed, checked-in test fixture —
// never untrusted input) into a fresh t.TempDir() so a Rebuild test can
// mutate it without disturbing the fixture every other test in this package
// reads.
func copyLinksFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "links")
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, readErr := os.ReadFile(path) //nolint:gosec // G304/G703: path comes from Walk over this test's own fixed testdata/links, not untrusted input
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, 0o600) //nolint:gosec // G304/G703: target is under this test's own t.TempDir(), not untrusted input
	})
	if err != nil {
		t.Fatalf("copyLinksFixture: %v", err)
	}
	return dst
}

func TestBacklinks_ListsRealLinkers(t *testing.T) {
	idx := newLinksTestIndex(t)
	alpha := mustFindDoc(t, idx, "vault", "notes/Alpha.md")

	got := idx.Backlinks(alpha)
	if len(got) != 1 || got[0].RelPath != "notes/linker.md" {
		t.Errorf("Backlinks(notes/Alpha.md) = %+v, want exactly [notes/linker.md]", got)
	}
}

func TestBacklinks_UnlinkedDocReturnsEmpty(t *testing.T) {
	idx := newLinksTestIndex(t)
	// Nothing in the fixture links to repo/sub/nested.md: repo/index.md
	// links only to guide.md and an outside-root ../../etc/passwd.
	nested := mustFindDoc(t, idx, "repo", "sub/nested.md")

	got := idx.Backlinks(nested)
	if len(got) != 0 {
		t.Errorf("Backlinks(repo/sub/nested.md) = %+v, want empty", got)
	}
}

func TestBacklinks_NilDocReturnsEmptyWithoutPanic(t *testing.T) {
	idx := newLinksTestIndex(t)
	if got := idx.Backlinks(nil); len(got) != 0 {
		t.Errorf("Backlinks(nil) = %+v, want empty", got)
	}
}

func TestBacklinks_UnknownDocReturnsEmptyWithoutPanic(t *testing.T) {
	idx := newLinksTestIndex(t)
	foreign := &Doc{RootLabel: "nonexistent-root", AbsPath: "/nowhere"}
	if got := idx.Backlinks(foreign); len(got) != 0 {
		t.Errorf("Backlinks(unknown doc) = %+v, want empty", got)
	}
}

func TestBacklinks_RebuildPicksUpNewLinker(t *testing.T) {
	dir := copyLinksFixture(t)
	idx, err := NewIndex([]string{filepath.Join(dir, "vault"), filepath.Join(dir, "repo")})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	orphan := mustFindDoc(t, idx, "vault", "notes/orphan.md")
	if got := idx.Backlinks(orphan); len(got) != 1 || got[0].RelPath != "index.md" {
		t.Fatalf("pre-rebuild Backlinks(notes/orphan.md) = %+v, want exactly [index.md]", got)
	}

	newLinkerPath := filepath.Join(dir, "vault", "notes", "second-linker.md")
	if err := os.WriteFile(newLinkerPath, []byte("# Second Linker\n\n[[orphan]]\n"), 0o600); err != nil {
		t.Fatalf("write second-linker.md: %v", err)
	}

	rebuilt, err := idx.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// The ORIGINAL index must not have changed underneath the caller.
	if got := idx.Backlinks(orphan); len(got) != 1 {
		t.Errorf("original Index.Backlinks(notes/orphan.md) changed after Rebuild — Index must never be mutated in place; got %+v", got)
	}

	rebuiltOrphan := mustFindDoc(t, rebuilt, "vault", "notes/orphan.md")
	got := rebuilt.Backlinks(rebuiltOrphan)
	if len(got) != 2 {
		t.Fatalf("rebuilt Backlinks(notes/orphan.md) = %+v, want 2 linkers (index.md, second-linker.md)", got)
	}
	if got[0].RelPath != "index.md" || got[1].RelPath != "notes/second-linker.md" {
		t.Errorf("rebuilt Backlinks(notes/orphan.md) = %+v, want [index.md, notes/second-linker.md] sorted by RelPath", got)
	}
}

func TestBacklinks_CrossRootIsolation_SingleFileRootLinkNeverCounts(t *testing.T) {
	idx, err := NewIndex([]string{
		filepath.Join("testdata", "links", "single.md"),
		filepath.Join("testdata", "links", "repo"),
	})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	nested := mustFindDoc(t, idx, "repo", "sub/nested.md")

	// single.md's own Links include a relative markdown link to
	// "repo/sub/nested.md" — the SAME string a real repo-root doc would use
	// — but single.md's root has no repo/ subtree of its own (Root.OnlyFile
	// makes it the sole doc in its root's tables), so the link must miss
	// entirely rather than leaking across into the repo root's doc.
	if got := idx.Backlinks(nested); len(got) != 0 {
		t.Errorf("Backlinks(repo/sub/nested.md) = %+v, want empty — a single-file root's link text must not cross into another root's namespace", got)
	}
}

func TestResolveLink_SingleFileRootFragmentOnlyResolvesToItself(t *testing.T) {
	idx, err := NewIndex([]string{filepath.Join("testdata", "links", "single.md")})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	single := mustFindDoc(t, idx, "single", "single.md")

	doc, miss := idx.ResolveLink(single, "#a-heading")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc != single {
		t.Errorf("doc = %+v, want the same *Doc as single (%+v)", doc, single)
	}

	// A self-link must never appear as its own backlink.
	if got := idx.Backlinks(single); len(got) != 0 {
		t.Errorf("Backlinks(single.md) = %+v, want empty — a doc is never its own backlink", got)
	}
}

func TestNewIndexWithOptions_RootKindOverrideFlipsVaultSemantics(t *testing.T) {
	repoPath := filepath.Join("testdata", "links", "repo")
	idx, err := NewIndexWithOptions(
		[]string{repoPath},
		IndexOptions{RootKinds: map[string]RootKind{repoPath: RootVault}},
	)
	if err != nil {
		t.Fatalf("NewIndexWithOptions: %v", err)
	}
	roots := idx.Roots()
	if len(roots) != 1 || roots[0].Kind != RootVault {
		t.Fatalf("Roots() = %+v, want a single RootVault root", roots)
	}
	if roots[0].VaultPath != roots[0].Path {
		t.Errorf("VaultPath = %q, want it to fall back to the root's own path %q (repo/ has no real .obsidian ancestor)", roots[0].VaultPath, roots[0].Path)
	}

	from := mustFindDoc(t, idx, "repo", "index.md")
	// "guide" (bare basename, no extension) only resolves under vault
	// semantics — a RootDocs repo root only ever consults byRel, which needs
	// a relative path, not a bare basename.
	doc, miss := idx.ResolveLink(from, "guide")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "guide.md" {
		t.Errorf("doc = %+v, want guide.md", doc)
	}
}

func TestNewIndexWithOptions_OverrideToDocsClearsVaultPath(t *testing.T) {
	vaultPath := filepath.Join("testdata", "links", "vault")
	idx, err := NewIndexWithOptions(
		[]string{vaultPath},
		IndexOptions{RootKinds: map[string]RootKind{vaultPath: RootDocs}},
	)
	if err != nil {
		t.Fatalf("NewIndexWithOptions: %v", err)
	}
	roots := idx.Roots()
	if len(roots) != 1 || roots[0].Kind != RootDocs {
		t.Fatalf("Roots() = %+v, want a single RootDocs root", roots)
	}
	if roots[0].VaultPath != "" {
		t.Errorf("VaultPath = %q, want empty — an override to RootDocs clears it even though a real .obsidian directory exists here", roots[0].VaultPath)
	}
}
