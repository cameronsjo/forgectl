package docs

// Tests for #645: buildBacklinks needs each link's target document only, so
// it must do no fragment work at all.

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func setHostileLinks(idx *Index, rel, target, frag string, n int) {
	for i := range idx.docs {
		if idx.docs[i].RelPath == rel {
			idx.docs[i].Links = nil
			for j := 0; j < n; j++ {
				idx.docs[i].Links = append(idx.docs[i].Links, LinkRef{Raw: target + "#" + frag, Path: target, Fragment: frag, Form: FormHeading})
			}
		}
	}
}

func BenchmarkBuildBacklinks_RenderedFragments(b *testing.B) {
	vault := b.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		b.Fatal(err)
	}
	for name, body := range map[string]string{"Note.md": "# Note\n\n## x\n", "Linker.md": "# Linker\n"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	idx, err := NewIndex([]string{vault})
	if err != nil {
		b.Fatal(err)
	}
	setHostileLinks(idx, "Linker.md", "Note", budgetFragment(), 900)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.buildBacklinks()
	}
}

// TestBuildBacklinks_DoesNoFragmentWork: links whose fragments would each
// need a rendered-text parse cost the same allocations as the same links
// with no fragment. Mutation: passing link.Fragment to resolveParts in
// buildBacklinks turns it red.
func TestBuildBacklinks_DoesNoFragmentWork(t *testing.T) {
	idx, _ := newMatchVault(t, "# Note\n\n## x\n")
	mallocs := func() uint64 {
		var a, b runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&a)
		idx.buildBacklinks()
		runtime.ReadMemStats(&b)
		return b.Mallocs - a.Mallocs
	}
	setHostileLinks(idx, "Linker.md", "Note", "", 300)
	base := mallocs()
	setHostileLinks(idx, "Linker.md", "Note", budgetFragment(), 300)
	got := mallocs()
	if float64(got) > 1.1*float64(base)+50 {
		t.Errorf("fragment links allocated %d, fragment-less %d: the build parsed fragments", got, base)
	}
}

// TestBuildBacklinks_ParityWithFragmentResolution: dropping the fragment
// changes no backlink. Every link, whether its fragment hits, misses, or
// the target is absent, yields the same target doc through the full
// resolver as the doc-only one buildBacklinks uses.
func TestBuildBacklinks_ParityWithFragmentResolution(t *testing.T) {
	idx, from := newMatchVault(t, "# Note\n\n## x\n\nPara ^blk\n")
	cases := []LinkRef{
		{Path: "Note", Fragment: "x"},
		{Path: "Note", Fragment: "missing"},
		{Path: "Note", Fragment: "^blk"},
		{Path: "Note", Fragment: "^nope"},
		{Path: "Note", Fragment: budgetFragment()},
		{Path: "Note", Fragment: "a#b#x"},
		{Path: "Ghost", Fragment: "x"},
		{Path: "", Fragment: "x"},
		{Path: "Note"},
	}
	for _, c := range cases {
		full, _ := idx.resolveParts(&from, c.Path, c.Fragment, nil)
		bare, _ := idx.resolveParts(&from, c.Path, "", nil)
		if !reflect.DeepEqual(full, bare) {
			t.Errorf("%s: fragment %.20q: full resolve %v, doc-only %v", c.Path, c.Fragment, full, bare)
		}
	}
	// End to end: one linker with all cases yields one backlink on Note.
	setHostileLinks(idx, "Linker.md", "Note", "missing", 3)
	note, _ := idx.Find(idx.Roots()[0].Label, "Note.md")
	if got := idx.buildBacklinks()[docKey{rootLabel: note.RootLabel, absPath: note.AbsPath}]; len(got) != 1 {
		t.Errorf("backlinks on Note = %v, want exactly Linker", fmt.Sprint(got))
	}
}
