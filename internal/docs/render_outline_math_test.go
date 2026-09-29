package docs

import (
	"testing"
)

func TestExtractOutline_MathHeadings(t *testing.T) {
	doc, err := RenderDoc([]byte("## Energy $E=mc^2$\n\n### Bound $a < b$ and $$\\sum x$$\n\n## Cost `$5` here\n\n## Price \\$5 and $x$ and $y$\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Energy E=mc^2", "Bound a < b and \\sum x", "Cost $5 here", "Price $5 and x and y"}
	if len(doc.Outline) != len(want) {
		t.Fatalf("outline = %+v", doc.Outline)
	}
	for i, w := range want {
		if doc.Outline[i].Text != w {
			t.Errorf("outline[%d] = %q, want %q", i, doc.Outline[i].Text, w)
		}
	}
}
