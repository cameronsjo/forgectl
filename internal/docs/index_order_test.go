package docs

// Test plan for sortByRecency (forgectl#564, part 1)
//
// sortByRecency (Classification: pure ordering over []Doc)
//   [x] Happy: equal mtimes order by (RootLabel, RelPath), whatever order
//       the docs arrived in (forward and reversed input agree)
//   [x] Happy: two NewIndex builds over an equal-mtime tree are DeepEqual
//       and list tied docs in RelPath order

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestSortByRecency_EqualMtimes_OrderIndependentOfInput(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	// Two roots, two mtimes, interleaved: more than 12 docs so sort.Slice
	// leaves insertion sort for pdqsort, which reorders equal elements.
	var in []Doc
	for i := range 24 {
		mt := older
		if i%3 == 0 {
			mt = newer
		}
		in = append(in, Doc{
			RootLabel: []string{"alpha", "beta"}[i%2],
			RelPath:   fmt.Sprintf("d%02d.md", i),
			ModTime:   mt,
		})
	}

	forward := slices.Clone(in)
	reversed := slices.Clone(in)
	slices.Reverse(reversed)
	sortByRecency(forward)
	sortByRecency(reversed)

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("order depends on input order:\nforward:  %v\nreversed: %v", keysOf(forward), keysOf(reversed))
	}

	want := slices.Clone(in)
	slices.SortFunc(want, func(a, b Doc) int {
		if c := b.ModTime.Compare(a.ModTime); c != 0 {
			return c
		}
		if a.RootLabel != b.RootLabel {
			if a.RootLabel < b.RootLabel {
				return -1
			}
			return 1
		}
		if a.RelPath < b.RelPath {
			return -1
		}
		if a.RelPath > b.RelPath {
			return 1
		}
		return 0
	})
	if !reflect.DeepEqual(forward, want) {
		t.Errorf("order = %v\nwant    %v", keysOf(forward), keysOf(want))
	}
}

func TestNewIndex_EqualMtimes_TwoBuildsDeepEqualInPathOrder(t *testing.T) {
	dir := t.TempDir()
	tied := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := tied.Add(time.Hour)
	for i := range 30 {
		p := filepath.Join(dir, fmt.Sprintf("n%02d.md", i))
		writeFile(t, p, fmt.Sprintf("# N%02d", i))
		mt := tied
		if i%4 == 0 {
			mt = fresh
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	build := func() []Doc {
		t.Helper()
		idx, err := NewIndex([]string{dir})
		if err != nil {
			t.Fatalf("NewIndex: %v", err)
		}
		return idx.List()
	}
	first, second := build(), build()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two builds differ:\n%v\n%v", keysOf(first), keysOf(second))
	}

	for i := 1; i < len(first); i++ {
		a, b := first[i-1], first[i]
		if a.ModTime.Equal(b.ModTime) && a.RelPath > b.RelPath {
			t.Fatalf("tied docs out of path order at %d: %v", i, keysOf(first))
		}
	}
}

func keysOf(docs []Doc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.RootLabel + "/" + d.RelPath
	}
	return out
}
