package attention

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Every fixture below is synthetic. None is copied from a real transcript.

func TestParseSpecForm(t *testing.T) {
	text := "Some reply prose.\n\n**What's next:**\n\n" +
		"- **Question 1**: Ship the widget today? Recommended: yes\n" +
		"- **Action 2 (Cameron)**: run `bash /tmp/widget.sh` — Recommended: now\n" +
		"- **Action 3 (Claude)**: I can file the follow-up issue. Recommended: defer\n" +
		"- **Action 4 (Cameron):** merge the widget PR. Recommended: leave\n"
	p := Parse(text)
	if !p.Found {
		t.Fatal("header not found")
	}
	want := []Item{
		{Kind: KindQuestion, N: 1, Text: "Ship the widget today?", Rec: RecNow},
		{Kind: KindAction, N: 2, Actor: ActorCameron, Text: "run `bash /tmp/widget.sh`", Rec: RecNow},
		{Kind: KindAction, N: 3, Actor: ActorClaude, Text: "I can file the follow-up issue.", Rec: RecLater},
		{Kind: KindAction, N: 4, Actor: ActorCameron, Text: "merge the widget PR.", Rec: RecForget},
	}
	assertItems(t, p, want)
	if p.Seen != 4 || p.Read != 4 {
		t.Errorf("seen/read = %d/%d, want 4/4", p.Seen, p.Read)
	}
}

func TestParseHeaderForms(t *testing.T) {
	for name, header := range map[string]string{
		"bold":            "**What's next:**",
		"bold, colon out": "**What's next**:",
		"plain":           "What's next:",
		"heading":         "## What's next",
		"curly":           "**What’s next:**",
	} {
		t.Run(name, func(t *testing.T) {
			p := Parse(header + "\n- **Question 1**: Pick a name?\n")
			if !p.Found || len(p.Items) != 1 {
				t.Fatalf("Parse = %+v", p)
			}
		})
	}
}

func TestParseUsesTheLastBlock(t *testing.T) {
	text := "**What's next:**\n- **Question 1**: old?\n\nMore work happened.\n\n**What's next:**\n- **Question 1**: new?\n"
	p := Parse(text)
	if len(p.Items) != 1 || p.Items[0].Text != "new?" {
		t.Fatalf("items = %+v, want the last block", p.Items)
	}
}

func TestParseNoHeader(t *testing.T) {
	p := Parse("- **Question 1**: no header above me?\n")
	if p.Found || len(p.Items) != 0 || p.Seen != 0 {
		t.Fatalf("Parse = %+v, want nothing", p)
	}
}

// The six failure shapes measured in the corpus, each in synthetic form.

func TestParseNumberedBareActor(t *testing.T) {
	p := Parse("**What's next:**\n1. **Question 1**: Keep the old flag?\n3. **Claude** — I can delete the stale branch.\n")
	assertItems(t, p, []Item{
		{Kind: KindQuestion, N: 1, Text: "Keep the old flag?"},
		{Kind: KindAction, N: 3, Actor: ActorClaude, Text: "I can delete the stale branch."},
	})
}

func TestParseBareActorWithoutItemNumber(t *testing.T) {
	p := Parse("**What's next:**\n1. **Cameron** — if you want the gadget, run the installer.\n- **Claude**: I tidy the docs after.\n")
	assertItems(t, p, []Item{
		{Kind: KindAction, N: 1, Actor: ActorCameron, Text: "if you want the gadget, run the installer."},
		{Kind: KindAction, N: 2, Actor: ActorClaude, Text: "I tidy the docs after."},
	})
}

func TestParseFreeFormBoldLead(t *testing.T) {
	p := Parse("**What's next:**\n2. **Nothing owed by me.** The gizmo branch is merged.\n")
	if p.Seen != 1 || p.Read != 1 {
		t.Fatalf("seen/read = %d/%d, want 1/1", p.Seen, p.Read)
	}
	if len(p.Items) != 1 || p.Items[0].Kind != KindNote || !strings.Contains(p.Items[0].Text, "Nothing owed by me.") {
		t.Fatalf("items = %+v, want one note", p.Items)
	}
}

func TestParseLetteredOption(t *testing.T) {
	p := Parse("**What's next:**\n- **Question 1**: Which colour?\n- **A.** Red\n- **B)** Blue\n- **C.** Leave it\n")
	if p.Seen != 4 || p.Read != 4 {
		t.Fatalf("seen/read = %d/%d, want 4/4", p.Seen, p.Read)
	}
	if got := p.Items[3]; got.Kind != KindOption || got.Tag != "C" || got.Text != "Leave it" {
		t.Fatalf("option = %+v", got)
	}
}

func TestParseStopsAtTrailingProse(t *testing.T) {
	text := "**What's next:**\n- **Question 1**: Rename the sprocket?\n  It is used in two places.\n\nA closing paragraph with no list.\n- not part of the block\n"
	p := Parse(text)
	if p.Seen != 1 || len(p.Items) != 1 {
		t.Fatalf("Parse = %+v, want one item", p)
	}
	if p.Items[0].Text != "Rename the sprocket? It is used in two places." {
		t.Errorf("continuation not joined: %q", p.Items[0].Text)
	}
}

func TestParseIgnoresAFencedBlock(t *testing.T) {
	text := "**What's next:**\n- **Question 1**: real?\n\nExample:\n\n```markdown\n**What's next:**\n- **Question 9**: fenced?\n```\n"
	p := Parse(text)
	if len(p.Items) != 1 || p.Items[0].N != 1 {
		t.Fatalf("items = %+v, want only the unfenced block", p.Items)
	}
	only := Parse("```\n**What's next:**\n- **Question 1**: fenced?\n```\n")
	if only.Found {
		t.Fatalf("a header inside a fence was found: %+v", only)
	}
}

func TestParseUnreadLineIsCounted(t *testing.T) {
	p := Parse("**What's next:**\n- **Question 1**: fine?\n- plain bullet with no lead\n")
	if p.Seen != 2 || p.Read != 1 {
		t.Fatalf("seen/read = %d/%d, want 2/1", p.Seen, p.Read)
	}
}

func TestParseRecommendedMap(t *testing.T) {
	for raw, want := range map[string]string{
		"now": RecNow, "go": RecNow, "yes": RecNow, "later": RecLater, "defer": RecLater,
		"park": RecPark, "forget": RecForget, "leave": RecForget, "maybe": "",
	} {
		p := Parse("**What's next:**\n- **Question 1**: x? Recommended: " + raw + "\n")
		if len(p.Items) != 1 || p.Items[0].Rec != want {
			t.Errorf("Recommended: %s -> %+v, want %q", raw, p.Items, want)
		}
	}
}

func TestParseCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString("**What's next:**\n")
	for i := 1; i <= 30; i++ {
		b.WriteString("- **Question 1**: " + strings.Repeat("x", 500) + "\n")
	}
	p := Parse(b.String())
	if len(p.Items) != MaxItems {
		t.Errorf("items = %d, want %d", len(p.Items), MaxItems)
	}
	if p.Seen != 30 || p.Read != 30 {
		t.Errorf("seen/read = %d/%d, want 30/30", p.Seen, p.Read)
	}
	if n := utf8.RuneCountInString(p.Items[0].Text); n > MaxText {
		t.Errorf("text is %d runes, want at most %d", n, MaxText)
	}
}

func TestParseStripsHiddenCharacters(t *testing.T) {
	p := Parse("**What's next:**\n- **Question 1**: a\u200bb\u202ec\x1b[31md\u2066e\ufeff?\n")
	if len(p.Items) != 1 {
		t.Fatalf("items = %+v", p.Items)
	}
	if got := p.Items[0].Text; got != "abc[31mde?" {
		t.Errorf("text = %q, want hidden characters stripped", got)
	}
}

func assertItems(t *testing.T, p Parsed, want []Item) {
	t.Helper()
	if len(p.Items) != len(want) {
		t.Fatalf("items = %+v, want %d items", p.Items, len(want))
	}
	for i := range want {
		if p.Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, p.Items[i], want[i])
		}
	}
}

func TestParseKeepsAnUnknownRecommendation(t *testing.T) {
	p := Parse("**What's next:**\n- **Question 1**: Which one? (Recommended: option B)\n")
	if len(p.Items) != 1 || p.Items[0].Rec != "" || !strings.Contains(p.Items[0].Text, "Recommended: option B") {
		t.Fatalf("items = %+v, want the unknown value left in the text", p.Items)
	}
}

func TestParseStopsAtAThematicBreak(t *testing.T) {
	p := Parse("**What's next:**\n- **Question 1**: run x?\n---\nSigned off.\n")
	if len(p.Items) != 1 || p.Items[0].Text != "run x?" {
		t.Fatalf("items = %+v, want the rule to end the block", p.Items)
	}
	p = Parse("**What's next:**\n- **Question 1**: a?\n* * *\n- **Question 2**: b?\n")
	if p.Seen != 1 {
		t.Errorf("seen = %d, want the starred rule to end the block", p.Seen)
	}
}

func TestParseInlineTripleBacktickIsNotAFence(t *testing.T) {
	text := "```x``` is an inline span.\n\n**What's next:**\n- **Question 1**: real?\n"
	if p := Parse(text); !p.Found || len(p.Items) != 1 {
		t.Fatalf("Parse = %+v, want the header found", p)
	}
	// A fence closes only on a bare marker; "```go" inside a fence does not.
	text = "```\n```go\n**What's next:**\n- **Question 1**: fenced?\n```\n"
	if p := Parse(text); p.Found {
		t.Fatalf("Parse = %+v, want the fenced header ignored", p)
	}
}
