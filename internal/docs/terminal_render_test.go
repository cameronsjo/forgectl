package docs

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"
)

func TestRenderTerminal_TextLinksAndFallbacks(t *testing.T) {
	dir := t.TempDir()
	docPath := filepath.Join(dir, "README.md")
	source := []byte("# Hello\n\n[Next](next.md)\n\n![remote](https://example.com/x.png)\n")
	if err := os.WriteFile(docPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := Doc{RootLabel: "root", RelPath: "README.md", AbsPath: docPath, Title: "Hello"}
	page, err := RenderTerminal(source, doc, Root{Label: "root", Path: dir}, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Content, "Hello") || !strings.Contains(page.Content, "image: remote") {
		t.Fatalf("content = %q", page.Content)
	}
	if len(page.Links) != 1 || page.Links[0].Target != "next.md" {
		t.Fatalf("links = %+v", page.Links)
	}
	if len(page.ImageIDs) != 0 || strings.ContainsRune(page.Content, kitty.Placeholder) {
		t.Fatal("graphics-off page emitted graphics")
	}
}

func TestRenderTerminal_LocalImageAndMermaid(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(dir, "README.md")
	imagePath := filepath.Join(dir, "pixel.png")
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	f, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	source := []byte("![pixel](pixel.png)\n\n```mermaid\ngraph LR\nA --> B\n```\n")
	if err := os.WriteFile(docPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := Doc{RootLabel: "root", RelPath: "README.md", AbsPath: docPath}
	page, err := RenderTerminal(source, doc, Root{Label: "root", Path: dir}, 60, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.ImageIDs) != 2 {
		t.Fatalf("image IDs = %v, content = %q", page.ImageIDs, page.Content)
	}
	if strings.Count(page.Content, string(kitty.Placeholder)) == 0 || !strings.Contains(page.Graphics, "\x1b_G") {
		t.Fatal("rendered media has no placeholders")
	}
	if strings.Contains(page.Content, "\x1b_G") {
		t.Fatal("scrollable content contains Kitty transmission bytes")
	}
}

func TestRenderTerminal_RejectsEscapingAndRemoteImages(t *testing.T) {
	dir := t.TempDir()
	docPath := filepath.Join(dir, "README.md")
	doc := Doc{RootLabel: "root", RelPath: "README.md", AbsPath: docPath}
	root := Root{Label: "root", Path: dir}
	for _, target := range []string{"../outside.png", "https://example.com/x.png", "/tmp/x.png"} {
		_, err := resolveTerminalResource(target, doc, root)
		if err == nil {
			t.Fatalf("resolveTerminalResource(%q) succeeded", target)
		}
	}
}

func TestRenderTerminal_FallbackNeutralizesControls(t *testing.T) {
	segment := terminalSegment{kind: "mermaid", source: "graph LR\nA[\x1b[2J]-->B\n"}
	fallback := mediaFallback(segment)
	if strings.ContainsRune(fallback, '\x1b') || !strings.Contains(fallback, `\x1b`) {
		t.Fatalf("fallback did not visibly neutralize control: %q", fallback)
	}
}

func TestRenderTerminal_MarkdownNeutralizesControls(t *testing.T) {
	dir := t.TempDir()
	docPath := filepath.Join(dir, "README.md")
	doc := Doc{RootLabel: "root", RelPath: "README.md", AbsPath: docPath}
	page, err := RenderTerminal([]byte("# heading\n\ntext \x1b[2J end\n"), doc, Root{Label: "root", Path: dir}, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	// Glamour's own ANSI is expected. Removing CSI prefixes leaves any raw
	// source escape visible to this assertion.
	stripped := strings.ReplaceAll(page.Content, "\x1b[", "")
	if strings.ContainsRune(stripped, '\x1b') || !strings.Contains(page.Content, `\x1b`) {
		t.Fatalf("content did not visibly neutralize source control: %q", page.Content)
	}
}
