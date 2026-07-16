package docs

// Test plan for render.go
//
// Render (Classification: ops layer — markdown render + sanitize)
//   [x] Happy: a heading renders with an id (goldmark auto-heading-id survives sanitization)
//   [x] Happy: a fenced code block renders class-based chroma spans, not inline style=
//   [x] Happy: a GFM table renders <table>/<th>/<td>
//   [x] Unhappy (security): a raw <script> tag is stripped, not passed through
//   [x] Unhappy (security): an inline onerror= handler is stripped
//   [x] Unhappy (security): a javascript: href is neutralized
//
// ChromaCSS (Classification: helper)
//   [x] Happy: returns non-empty CSS containing the chroma class prefix

import (
	"strings"
	"testing"
)

func TestRender_HeadingGetsID(t *testing.T) {
	out, err := Render([]byte("# Hello World\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `id="hello-world"`) {
		t.Errorf("Render output missing heading id: %s", out)
	}
}

func TestRender_FencedCodeBlock_UsesClassesNotInlineStyle(t *testing.T) {
	out, err := Render([]byte("```go\nfunc main() {}\n```\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "style=") {
		t.Errorf("Render output contains inline style= (want class-based highlighting only): %s", out)
	}
	if !strings.Contains(out, `class="chroma"`) {
		t.Errorf("Render output missing chroma class wrapper: %s", out)
	}
}

func TestRender_GFMTable_Renders(t *testing.T) {
	src := "| a | b |\n| --- | --- |\n| 1 | 2 |\n"
	out, err := Render([]byte(src))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"<table>", "<th>", "<td>"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render GFM table output missing %q: %s", want, out)
		}
	}
}

func TestRender_ScriptTag_Stripped(t *testing.T) {
	out, err := Render([]byte("hello\n\n<script>alert(1)</script>\n\nworld\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "<script") {
		t.Errorf("Render output contains a <script> tag, want it stripped: %s", out)
	}
	if strings.Contains(out, "alert(1)") {
		t.Errorf("Render output still contains script payload text: %s", out)
	}
}

func TestRender_OnErrorHandler_Stripped(t *testing.T) {
	out, err := Render([]byte(`<img src="x" onerror="alert(1)">` + "\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "onerror") {
		t.Errorf("Render output contains onerror= handler, want it stripped: %s", out)
	}
}

func TestRender_JavascriptHref_Neutralized(t *testing.T) {
	out, err := Render([]byte(`[click me](javascript:alert(1))` + "\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "javascript:") {
		t.Errorf("Render output contains a javascript: URL, want it neutralized: %s", out)
	}
}

func TestChromaCSS_NonEmptyWithClassPrefix(t *testing.T) {
	css := ChromaCSS()
	if len(css) == 0 {
		t.Fatal("ChromaCSS() is empty")
	}
	if !strings.Contains(string(css), ".chroma") {
		t.Errorf("ChromaCSS() missing .chroma class rules: %s", css)
	}
}
