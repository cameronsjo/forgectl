package docs

// Frontmatter rendering contract:
//   [x] Happy: YAML frontmatter becomes a collapsed disclosure block with a .kv grid
//   [x] Happy: document order of keys is preserved
//   [x] Sad: no frontmatter -> no disclosure block, body untouched
//   [x] Sad: frontmatter values are HTML-escaped (no injection through metadata)
//   [x] Sad: a mid-document thematic break still renders as <hr>
//   [x] Sad: a LEADING thematic break (unterminated --- fence) keeps the body
//   [x] Sad: a leading --- whose "block" is prose (not a YAML mapping) keeps the body

import (
	"strings"
	"testing"
)

const fmDoc = `---
status: "in-review"
updated: "2026-08-31"
branch: "plan/docs-fix-up"
---

# Title

Body paragraph.
`

func TestRender_FrontmatterBecomesDisclosure(t *testing.T) {
	got, err := Render([]byte(fmDoc))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		`<details class="frontmatter">`,
		`<dl class="kv">`,
		`<dt>status</dt>`,
		`<dd>in-review</dd>`,
		`<dt>branch</dt>`,
		`<dd>plan/docs-fix-up</dd>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output missing %q:\n%s", want, got)
		}
	}
	// The pre-fix failure mode: goldmark reads the opening --- as a thematic
	// break and folds the YAML into a mangled heading.
	if strings.Contains(got, "status: &#34;") || strings.Contains(got, "<h2 id=\"branch") {
		t.Errorf("frontmatter leaked into the rendered body:\n%s", got)
	}
	if !strings.Contains(got, "<h1 id=\"title\">Title</h1>") {
		t.Errorf("body heading missing:\n%s", got)
	}
}

func TestRender_FrontmatterKeyOrderPreserved(t *testing.T) {
	got, err := Render([]byte(fmDoc))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	iStatus := strings.Index(got, "<dt>status</dt>")
	iUpdated := strings.Index(got, "<dt>updated</dt>")
	iBranch := strings.Index(got, "<dt>branch</dt>")
	if iStatus < 0 || iUpdated < 0 || iBranch < 0 {
		t.Fatalf("expected all three keys present:\n%s", got)
	}
	if iStatus >= iUpdated || iUpdated >= iBranch {
		t.Errorf("keys out of document order (status=%d updated=%d branch=%d)", iStatus, iUpdated, iBranch)
	}
}

func TestRender_NoFrontmatterNoDisclosure(t *testing.T) {
	got, err := Render([]byte("# Plain\n\nNo metadata here.\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, `class="frontmatter"`) {
		t.Errorf("disclosure block on a frontmatter-less doc:\n%s", got)
	}
}

func TestRender_FrontmatterValuesEscaped(t *testing.T) {
	doc := "---\ntitle: \"<script>alert(1)</script>\"\n---\n\n# Body\n"
	got, err := Render([]byte(doc))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Fatalf("unescaped frontmatter value reached the output:\n%s", got)
	}
	if !strings.Contains(got, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("escaped frontmatter value missing:\n%s", got)
	}
}

// TestRender_LeadingThematicBreakKeepsBody pins the gate in Render: the
// frontmatter extension's opener is greedy (any leading --- fence starts a
// block, and an unterminated one consumes to end of file), so without
// hasWellFormedFrontmatter a doc opening with a thematic break rendered as a
// completely empty page.
func TestRender_LeadingThematicBreakKeepsBody(t *testing.T) {
	cases := map[string]string{
		"unterminated":        "---\n\nText after break\n",
		"bare_hr_only":        "---\n",
		"prose_between_rules": "---\n\npara one\n\n---\n\npara two\n",
		"yaml_list_block":     "---\n- a\n- b\n---\n\n# T\n",
	}
	for name, src := range cases {
		got, err := Render([]byte(src))
		if err != nil {
			t.Fatalf("%s: Render: %v", name, err)
		}
		if strings.TrimSpace(got) == "" && strings.TrimSpace(src) != "---" {
			t.Errorf("%s: body swallowed, rendered empty for %q", name, src)
		}
	}
	// The prose case must keep BOTH paragraphs — para one sat inside what the
	// ungated extension read as an (invalid) frontmatter block.
	got, err := Render([]byte("---\n\npara one\n\n---\n\npara two\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"para one", "para two"} {
		if !strings.Contains(got, want) {
			t.Errorf("leading-rule doc lost %q:\n%s", want, got)
		}
	}
}

func TestRender_MidDocumentThematicBreakSurvives(t *testing.T) {
	got, err := Render([]byte("# A\n\nabove\n\n---\n\nbelow\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, "<hr") {
		t.Errorf("mid-document thematic break lost:\n%s", got)
	}
}

// TestChromaCSS_UsesArtificerTokens pins the syntax palette to the design
// system: the served stylesheet must derive every color from Artificer vars,
// never a hardcoded style palette (monokai's #272822 slab ignored the theme).
func TestChromaCSS_UsesArtificerTokens(t *testing.T) {
	css := string(ChromaCSS())
	for _, want := range []string{"var(--fg-muted)", "var(--brand-purple-bright)", "var(--bg-inactive)"} {
		if !strings.Contains(css, want) {
			t.Errorf("chroma stylesheet missing %s", want)
		}
	}
	if strings.Contains(css, "#272822") {
		t.Error("chroma stylesheet still carries the monokai background")
	}
}
