package docs

// Frontmatter size cap contract (#910):
//   [x] Happy: a block of exactly maxFrontmatterBytes is frontmatter, YAML and TOML
//   [x] Sad: one byte more is not frontmatter, YAML and TOML
//   [x] Sad: an over-cap block renders as text, with no properties block,
//       and the index reads no aliases or trust fields from it
//   [x] Unhappy: a doc past the cap costs about what an unterminated block
//       of the same bytes costs, which is never decoded

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/perftest"
)

// fmBlock returns n bytes of frontmatter lines, n at least 5: plain
// `kNNNNNNNNN: v` keys, then one `z: xxx` line that takes up the rest.
func fmBlock(n int) string {
	var b strings.Builder
	for i := 0; n-b.Len() >= 14+5; i++ {
		fmt.Fprintf(&b, "k%09d: v\n", i)
	}
	b.WriteString("z: " + strings.Repeat("x", n-b.Len()-4) + "\n")
	return b.String()
}

// The docs promise 16 KiB (docs/commands/docs.md). Mutation: any other
// maxFrontmatterBytes turns this red.
func TestMaxFrontmatterBytes_Is16KiB(t *testing.T) {
	if maxFrontmatterBytes != 16<<10 {
		t.Fatalf("maxFrontmatterBytes = %d, want %d (16 KiB, as docs/commands/docs.md says)", maxFrontmatterBytes, 16<<10)
	}
}

// A block of exactly maxFrontmatterBytes is frontmatter and one byte more
// is not, whichever fence opens it. Mutation: dropping the cap check in
// splitFrontmatter turns the over-cap rows red; `>=` for `>` turns the
// at-cap rows red.
func TestSplitFrontmatter_Cap(t *testing.T) {
	for _, fence := range []string{"---", "+++"} {
		for _, tc := range []struct {
			n    int
			want bool
		}{
			{maxFrontmatterBytes, true},
			{maxFrontmatterBytes + 1, false},
		} {
			block := fmBlock(tc.n)
			if len(block) != tc.n {
				t.Fatalf("fmBlock(%d) is %d bytes", tc.n, len(block))
			}
			fm, ok := splitFrontmatter([]byte(fence + "\n" + block + fence + "\n\nbody\n"))
			if ok != tc.want {
				t.Errorf("%s block of %d bytes: frontmatter = %v, want %v", fence, tc.n, ok, tc.want)
				continue
			}
			if ok && (len(fm.block) != tc.n || string(fm.body) != "\nbody\n") {
				t.Errorf("%s block of %d bytes split as a %d-byte block and body %q", fence, tc.n, len(fm.block), fm.body)
			}
		}
	}
}

// overCapDoc is a doc whose YAML frontmatter names an alias and the trust
// fields but runs one byte past the cap.
func overCapDoc() []byte {
	head := "aliases: [capped-alias]\nstatus: deprecated\nstale_after: 2020-01-01T00:00:00Z\norphan_ok: true\n"
	block := head + fmBlock(maxFrontmatterBytes+1-len(head))
	return []byte("---\n" + block + "---\n\n# Capped\n\nbody-marker-910\n")
}

// An over-cap block is the document's text, not its metadata: the page
// keeps the body and shows the block's text with no properties block, and
// the index reads no alias or trust field from it. Mutation: dropping the
// cap check in splitFrontmatter turns every assertion here red.
func TestFrontmatterOverCap_ReadAsText(t *testing.T) {
	src := overCapDoc()
	got, err := Render(src)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "data-forgectl-props") {
		t.Errorf("an over-cap block rendered as a properties block")
	}
	for _, want := range []string{"body-marker-910", "capped-alias", "k000000001: v"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered page is missing %q, so the source is not readable", want)
		}
	}
	if words := countWords(src); words < 1000 {
		t.Errorf("countWords = %d, want the block's words counted as body", words)
	}
	meta, err := scanDocFrom(RootDocs, bytes.NewReader(src), "capped.md")
	if err != nil {
		t.Fatalf("scanDocFrom: %v", err)
	}
	if len(meta.Aliases) != 0 || meta.Status != "" || meta.StaleAfter != "" || meta.OrphanOK {
		t.Errorf("the index read metadata from an over-cap block: aliases %q, status %q, stale_after %q, orphan_ok %v",
			meta.Aliases, meta.Status, meta.StaleAfter, meta.OrphanOK)
	}
}

// splitSink keeps the timed splits from being optimized away.
var splitSink int

// A doc whose frontmatter runs far past the cap costs splitFrontmatter
// about what the same bytes cost with no closing fence, which no version
// ever decodes: the cap stops the scan before any YAML decode. The check
// is a ratio in process CPU time (perftest.Within). Measured: about 1
// with the cap; about 780 without it, where the decode of 128 KiB of
// keys, quadratic in their number, dominates.
//
// Mutation: dropping the cap check in splitFrontmatter turns this red on
// the ratio.
func TestSplitFrontmatter_OverCapSkipsTheDecode(t *testing.T) {
	const n, reps = 128 << 10, 10
	block := fmBlock(n)
	unterminated := []byte("---\n" + block + "\nbody\n")
	overCap := []byte("---\n" + block + "---\n\nbody\n")
	// Whether the block is taken as frontmatter is TestSplitFrontmatter_Cap's
	// question; this one only times the split.
	split := func(src []byte) func() {
		return func() {
			for range reps {
				fm, _ := splitFrontmatter(src)
				splitSink += len(fm.block)
			}
		}
	}
	perftest.Within(t, "splitFrontmatter past the cap", 4, split(unterminated), split(overCap))
}
