package docs

// Test plan for search snippet re-reads (forgectl#743)
//   [x] Security: an rg hit's snippet is the doc's own text, re-read at
//       rg's absolute_offset, never the line text rg printed
//   [x] Security: a hit whose doc was swapped for a symlink to another
//       indexed doc is dropped and counted, not served under its name
//   [x] Security: real rg under a swap-race stress run never returns
//       outside content or another doc's content in a snippet
//   [x] Happy: readSnippet's bounded window cuts the same snippet
//       snippetAround cuts from the whole line, across rune widths and
//       match positions
//   [x] Unhappy: lineOffset finds a line's start and refuses one past EOF

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
)

// offsetRecord renders an rg --json match record carrying absolute_offset,
// as real rg prints, with lineText as the line rg claims it read.
func offsetRecord(t *testing.T, path, lineText string, lineNo int, offset int64, start int) string {
	t.Helper()
	rec := map[string]any{
		"type": "match",
		"data": map[string]any{
			"path":            map[string]any{"text": path},
			"lines":           map[string]any{"text": lineText},
			"line_number":     lineNo,
			"absolute_offset": offset,
			"submatches":      []map[string]any{{"match": map[string]any{"text": "needle"}, "start": start, "end": start + 6}},
		},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func TestSearchSnippetIsReReadFromTheDocNotRgOutput(t *testing.T) {
	idx := searchRootWith(t, map[string]string{"a.md": "# a\nhello needle world\n"})
	root := idx.Roots()[0].Path
	rec := offsetRecord(t, filepath.Join(root, "a.md"), "OUTSIDE secret needle\n", 2, 4, 6)
	resp, err := (Searcher{Runner: &fakeRg{write: writeAll(rec)}, LookPath: fakeLookPath}).Search(t.Context(), idx, "needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Snippet != "hello needle world" {
		t.Errorf("results = %+v, want one hit whose snippet is the doc's own line 2", resp.Results)
	}
}

func TestSearchDropsAHitSwappedToAnotherDoc(t *testing.T) {
	idx := searchRootWith(t, map[string]string{"a.md": "# A\nneedle A\n", "b.md": "# B\nneedle B\n"})
	root := idx.Roots()[0].Path
	a := filepath.Join(root, "a.md")
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b.md", a); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	rec := offsetRecord(t, a, "needle A\n", 2, 4, 0)
	resp, err := (Searcher{Runner: &fakeRg{write: writeAll(rec)}, LookPath: fakeLookPath}).Search(t.Context(), idx, "needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 0 || resp.Skipped != 1 {
		t.Errorf("results = %+v skipped = %d, want the swapped hit dropped and counted", resp.Results, resp.Skipped)
	}
}

func TestReadSnippetMatchesTheWholeLineCut(t *testing.T) {
	lines := map[string]string{
		"ascii":  strings.Repeat("a", 3000) + "needle" + strings.Repeat("b", 3000),
		"3-byte": strings.Repeat("€", 1500) + "needle" + strings.Repeat("€", 1500),
		"4-byte": strings.Repeat("\U0001F600", 1500) + "needle" + strings.Repeat("\U0001F600", 1500),
		"mixed":  strings.Repeat("a€\U0001F600", 700) + "needle" + strings.Repeat("\U0001F600€b", 700),
		"short":  "a needle here",
	}
	for name, line := range lines {
		match := strings.Index(line, "needle")
		// Match positions near the line's start, middle and end.
		for _, start := range []int{0, 1, match, len(line) - 1, len(line) - 700, 700, 959, 961, 1921} {
			if start < 0 || start >= len(line) {
				continue
			}
			prefix := "# t\nfirst line\n"
			file := bytes.NewReader([]byte(prefix + line + "\nnext line\n"))
			got := readSnippet(file, int64(len(prefix)), start)
			want := snippetAround(strings.ToValidUTF8(line, "�"), start, maxSnippetRunes)
			if got != want {
				t.Errorf("%s at %d: readSnippet =\n%q\nwhole-line cut =\n%q", name, start, got, want)
			}
		}
	}
}

func TestLineOffset(t *testing.T) {
	f := bytes.NewReader([]byte("one\ntwo\n\nfour"))
	for line, want := range map[int]int64{1: 0, 2: 4, 3: 8, 4: 9} {
		if got, ok := lineOffset(f, line); !ok || got != want {
			t.Errorf("lineOffset(%d) = %d, %v, want %d", line, got, ok, want)
		}
	}
	for _, line := range []int{0, -1, 5, 99} {
		if _, ok := lineOffset(f, line); ok {
			t.Errorf("lineOffset(%d) found a line past the file", line)
		}
	}
}

func TestSearchRealRgSwapRaceNeverServesOtherContent(t *testing.T) {
	requireRg(t)
	if testing.Short() {
		t.Skip("stress test")
	}
	base := mustCanonicalRoot(t, t.TempDir())
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	writeFile(t, filepath.Join(outside, "secret.md"), "# S\nneedle OUTSIDE\n")
	writeFile(t, filepath.Join(root, "a.md"), "# A\nneedle a-own\n")
	writeFile(t, filepath.Join(root, "b.md"), "# B\nneedle b-own\n")
	writeFile(t, filepath.Join(root, "c.md"), "# C\nneedle c-own\n")
	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "probe")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	swapper(t, stop, &wg, filepath.Join(root, "a.md"), "# A\nneedle a-own\n", filepath.Join(outside, "secret.md"), false)
	swapper(t, stop, &wg, filepath.Join(root, "c.md"), "# C\nneedle c-own\n", "b.md", false)
	defer func() {
		close(stop)
		wg.Wait()
	}()

	s := Searcher{Runner: forgexec.OSRunner{}}
	hits := 0
	for range 150 {
		resp, err := s.Search(t.Context(), idx, "needle", 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range resp.Results {
			hits++
			own := "needle " + strings.TrimSuffix(r.Path, ".md") + "-own"
			if r.Snippet != own {
				t.Fatalf("hit %s:%d snippet %q, want %q: content from another file", r.Path, r.Line, r.Snippet, own)
			}
		}
	}
	if hits == 0 {
		t.Fatal("stress run returned no hits; it exercised nothing")
	}
}
