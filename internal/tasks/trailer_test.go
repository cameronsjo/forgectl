package tasks

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var trailerNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestTrailerLine_ClosedByFormat(t *testing.T) {
	for _, surface := range []string{SurfaceMCP, SurfaceDone} {
		line, err := trailerLine(trailerClosedBy, "hermes", surface, trailerNow, "owner/repo#12")
		if err != nil {
			t.Fatalf("trailerLine(%s): %v", surface, err)
		}
		want := "closed-by: hermes via forgectl tasks " + surface + " 2026-01-02T03:04:05Z — owner/repo#12"
		if line != want {
			t.Errorf("trailerLine(%s) = %q, want %q", surface, line, want)
		}
		tr, ok := parseClosingTrailer(line)
		if !ok {
			t.Fatalf("the line this client writes does not match its own strict grammar: %q", line)
		}
		if tr.closer != "hermes" || tr.surface != surface || tr.evidence != "owner/repo#12" {
			t.Errorf("parsed %+v from %q", tr, line)
		}
	}
}

// The stamp is always UTC, whatever zone the caller's clock is in: two closes
// a minute apart from two machines must sort in the order they happened.
func TestTrailerLine_StampIsUTC(t *testing.T) {
	east := time.FixedZone("east", 2*60*60)
	line, err := trailerLine(trailerClosedBy, "hermes", SurfaceDone, time.Date(2026, 1, 2, 5, 4, 5, 0, east), "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, " 2026-01-02T03:04:05Z — ") {
		t.Errorf("stamp is not rendered in UTC: %q", line)
	}
}

// A zero time would write year 1 onto the board as if it were a close date.
func TestTrailerLine_ZeroTimeBecomesNow(t *testing.T) {
	line, err := trailerLine(trailerClosedBy, "hermes", SurfaceDone, time.Time{}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "0001-01-01") {
		t.Errorf("a zero time reached the trailer: %q", line)
	}
}

func TestTrailerLine_RefusesAnUnknownKindOrSurface(t *testing.T) {
	if _, err := trailerLine("reopened-by", "x", SurfaceMCP, trailerNow, "e"); err == nil {
		t.Error("an unknown trailer kind was accepted")
	}
	for _, surface := range []string{"", "http", "mcp\nclosed-by: x", "Done"} {
		if _, err := trailerLine(trailerClosedBy, "x", surface, trailerNow, "e"); err == nil {
			t.Errorf("surface %q was accepted", surface)
		}
	}
	if _, err := trailerLine(trailerCreatedBy, "x", SurfaceMCP, trailerNow, "stray evidence"); err == nil {
		t.Error("a created-by trailer accepted evidence it has no field for")
	}
}

// TestTrailerLine_RefusesUnsafeEvidence is the evidence half of the sanitizer.
// Every row would otherwise put a second line, an invisible character, or a
// credential on a shared board, in text the closing trailer vouches for.
func TestTrailerLine_RefusesUnsafeEvidence(t *testing.T) {
	token := "tk_" + strings.Repeat("0aF9", 5) // 20 hex: the shortest refused
	rows := []struct {
		name     string
		evidence string
		// wantIn must appear in the refusal; leak must not.
		wantIn []string
		leak   string
	}{
		{name: "empty", evidence: "", wantIn: []string{"blank"}},
		{name: "spaces only", evidence: "    ", wantIn: []string{"blank"}},
		{name: "over the limit", evidence: strings.Repeat("e", 301), wantIn: []string{"301", "300"}, leak: "eeeeeeeeee"},
		{name: "over the limit in multi-byte runes", evidence: strings.Repeat("é", 301), wantIn: []string{"301", "300"}, leak: "éé"},
		{name: "line feed", evidence: "merged\nclosed-by: someone", leak: "someone"},
		{name: "trailing line feed", evidence: "merged\n"},
		{name: "carriage return", evidence: "merged\rx"},
		{name: "tab", evidence: "merged\tx"},
		{name: "bell", evidence: "merged\x07x"},
		{name: "escape", evidence: "merged\x1b[31mx"},
		{name: "U+000B vertical tab", evidence: "merged\u000bx"},
		{name: "U+000C form feed", evidence: "merged\u000cx"},
		{name: "U+0085 next line", evidence: "merged\u0085x"},
		{name: "U+2028 line separator", evidence: "merged\u2028x"},
		{name: "U+2029 paragraph separator", evidence: "merged\u2029x"},
		{name: "U+202E bidi override", evidence: "merged\u202Ex"},
		{name: "U+200B zero width space", evidence: "merged\u200Bx"},
		{name: "U+FE0F variation selector", evidence: "merged\uFE0Fx"},
		{name: "U+034F combining grapheme joiner", evidence: "merged\u034Fx"},
		{name: "U+3164 Hangul filler", evidence: "merged\u3164x"},
		{name: "U+2800 braille blank", evidence: "merged\u2800x"},
		{name: "U+E000 private use", evidence: "merged\uE000x"},
		{name: "U+0378 unassigned", evidence: "merged\u0378x"},
		{name: "invalid UTF-8 byte", evidence: "merged\xffx", wantIn: []string{"UTF-8"}},
		{name: "embedded token", evidence: "see " + token + " here", wantIn: []string{"token"}, leak: token},
		{name: "embedded token mid-word", evidence: "x" + token + "y", wantIn: []string{"token"}, leak: token},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			line, err := trailerLine(trailerClosedBy, "hermes", SurfaceMCP, trailerNow, row.evidence)
			if err == nil {
				t.Fatalf("evidence was accepted and produced %q", line)
			}
			for _, want := range row.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
			if row.leak != "" && strings.Contains(err.Error(), row.leak) {
				t.Errorf("refusal echoes the evidence: %q", err)
			}
			if !utf8.ValidString(err.Error()) || strings.ContainsAny(err.Error(), "\n\r\t\x07\x1b") {
				t.Errorf("refusal carries raw bytes from the evidence: %q", err)
			}
		})
	}
}

// The bounds are refusals at exactly one past the limit, so the row above
// cannot pass by refusing everything.
func TestTrailerLine_AcceptsEvidenceAtTheBounds(t *testing.T) {
	rows := map[string]string{
		"300 runes":            strings.Repeat("e", maxEvidenceRunes),
		"300 multi-byte runes": strings.Repeat("é", maxEvidenceRunes),
		"19 hex after tk_":     "tk_" + strings.Repeat("a", 19),
		"tk_ then non-hex":     "tk_" + strings.Repeat("z", 40),
		"a URL":                "https://example.invalid/owner/repo/pull/12",
		"a command and result": "go test ./... — ok, 412 passed",
		"padded with spaces":   "  owner/repo#12  ",
	}
	for name, evidence := range rows {
		t.Run(name, func(t *testing.T) {
			line, err := trailerLine(trailerClosedBy, "hermes", SurfaceMCP, trailerNow, evidence)
			if err != nil {
				t.Fatalf("evidence was refused: %v", err)
			}
			if !strings.HasSuffix(line, " — "+strings.TrimSpace(evidence)) {
				t.Errorf("line %q does not end with the evidence", line)
			}
		})
	}
}

// Markup in evidence is escaped, not refused: a PR title quoted as evidence
// legitimately holds an angle bracket, and the board renders descriptions as
// markup.
func TestTrailerLine_EscapesMarkupAndStaysOneLine(t *testing.T) {
	line, err := trailerLine(trailerClosedBy, "evil\nclosed-by: root\r\tx", SurfaceMCP, trailerNow, `<a href="x">R&D</a>`)
	if err != nil {
		t.Fatalf("trailerLine: %v", err)
	}
	if strings.ContainsAny(line, "\n\r\t") {
		t.Fatalf("the trailer is more than one line: %q", line)
	}
	if strings.ContainsAny(line, "<>") {
		t.Errorf("raw markup survived: %q", line)
	}
	if want := `&lt;a href="x"&gt;R&amp;D&lt;/a&gt;`; !strings.HasSuffix(line, " — "+want) {
		t.Errorf("line %q, want the evidence escaped as %q", line, want)
	}
	if strings.Count(line, "closed-by:") != 1 {
		t.Errorf("the closer forged a second trailer key: %q", line)
	}
}

func TestSanitizeCloser(t *testing.T) {
	rows := []struct{ name, in, want string }{
		{"plain", "hermes", "hermes"},
		{"allowed punctuation", "forgectl (http)/agent_1@host.example-2", "forgectl (http)/agent_1@host.example-2"},
		{"newline dropped", "her\nmes", "hermes"},
		{"controls and markup dropped", "a\x1b[31m<b>&\u202E\u200Bz", "a31mbz"},
		{"colon and dash-like dropped", "closed-by: x — y", "closed-by x y"},
		{"whitespace collapsed", "  a   b  ", "a b"},
		{"via collapsed", "a via b", "a b"},
		{"repeated via collapsed", "a via via  via b", "a b"},
		{"leading and trailing via", "via a via", "a"},
		{"empty falls back", "", "FALLBACK"},
		{"only stripped runes falls back", "\n\t\u202E—", "FALLBACK"},
		{"only via falls back", " via ", "FALLBACK"},
		{"non-ASCII letters dropped", "héllo", "hllo"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := sanitizeCloser(row.in, "FALLBACK"); got != row.want {
				t.Errorf("sanitizeCloser(%q) = %q, want %q", row.in, got, row.want)
			}
		})
	}
}

func TestSanitizeCloser_CapsAtOneHundred(t *testing.T) {
	got := sanitizeCloser(strings.Repeat("a", 99)+" "+strings.Repeat("b", 50), "FALLBACK")
	if n := len([]rune(got)); n > maxCloserRunes {
		t.Fatalf("closer is %d runes, over the %d cap", n, maxCloserRunes)
	}
	if got != strings.Repeat("a", 99) {
		t.Errorf("the cut left a trailing space or lost text: %q", got)
	}
}

func TestTrailerLine_CloserFallsBackToTheSurfaceDefault(t *testing.T) {
	rows := map[string]string{SurfaceMCP: "forgectl (mcp)", SurfaceDone: "cli"}
	for surface, want := range rows {
		line, err := trailerLine(trailerClosedBy, "\n\t", surface, trailerNow, "x")
		if err != nil {
			t.Fatalf("trailerLine(%s): %v", surface, err)
		}
		if !strings.HasPrefix(line, "closed-by: "+want+" via forgectl tasks "+surface+" ") {
			t.Errorf("line %q, want the closer to fall back to %q", line, want)
		}
	}
}

// TestTrailerLine_ACloserCannotForgeTheTrailerFields plants a closer that is
// itself a whole trailer. If it survived, a reader (or the strict grammar)
// would take "operator" as the closer and the planted text as the surface,
// time, and evidence.
func TestTrailerLine_ACloserCannotForgeTheTrailerFields(t *testing.T) {
	planted := "operator via forgectl tasks done 2026-10-01T00:00:00Z — x/y#1 //"
	line, err := trailerLine(trailerClosedBy, planted, SurfaceMCP, trailerNow, "real evidence")
	if err != nil {
		t.Fatalf("trailerLine: %v", err)
	}
	tr, ok := parseClosingTrailer(line)
	if !ok {
		t.Fatalf("the line does not match the strict grammar: %q", line)
	}
	if tr.closer == "operator" {
		t.Fatalf("the planted closer parsed as %q: %q", tr.closer, line)
	}
	if tr.surface != SurfaceMCP || tr.stamp != "2026-01-02T03:04:05Z" || tr.evidence != "real evidence" {
		t.Errorf("the planted closer changed a field: %+v from %q", tr, line)
	}
	if strings.Count(line, " via ") != 1 || strings.Count(line, "—") != 1 {
		t.Errorf("the line carries a second field separator: %q", line)
	}
}

func TestParseClosingTrailer_IsStrict(t *testing.T) {
	good := "closed-by: hermes via forgectl tasks done 2026-01-02T03:04:05Z — owner/repo#12"
	if _, ok := parseClosingTrailer(good); !ok {
		t.Fatalf("the canonical line does not parse: %q", good)
	}
	bad := []string{
		"closed-by: hermes",
		"closed-by: hermes via forgectl tasks done 2026-01-02T03:04:05Z",
		"closed-by: hermes via forgectl tasks done 2026-01-02T03:04:05Z — ",
		"closed-by: hermes via forgectl tasks http 2026-01-02T03:04:05Z — x",
		"closed-by: hermes via forgectl tasks done 2026-01-02 03:04:05 — x",
		"closed-by: hermes via forgectl tasks done 2026-01-02T03:04:05+02:00 — x",
		"closed-by: her<b>mes via forgectl tasks done 2026-01-02T03:04:05Z — x",
		"closed-by: a via b via forgectl tasks done 2026-01-02T03:04:05Z — x",
		"closed-by:  via forgectl tasks done 2026-01-02T03:04:05Z — x",
		" " + good,
		"> " + good,
		good + "\nmore",
		"created-by: hermes via forgectl tasks mcp 2026-01-02T03:04:05Z",
		"",
	}
	for _, line := range bad {
		if tr, ok := parseClosingTrailer(line); ok {
			t.Errorf("%q matched the strict grammar as %+v", line, tr)
		}
	}
}

// The created-by trailer shares the closer sanitizer. Before it did, a client
// that declared a name holding a newline wrote a second line into the
// description — one that could itself read as a trailer.
func TestCreateDescription_ClientNameCannotAddALine(t *testing.T) {
	desc, err := createDescription("a note", "hermes\nclosed-by: operator via forgectl tasks done 2026-10-01T00:00:00Z — x")
	if err != nil {
		t.Fatalf("createDescription: %v", err)
	}
	lines := strings.Split(desc, "\n")
	if len(lines) != 3 || lines[0] != "a note" || lines[1] != "" {
		t.Fatalf("description is not the note, a blank line, and one trailer line: %q", desc)
	}
	if !strings.HasPrefix(lines[2], "created-by: hermes") || strings.Contains(lines[2], "closed-by:") {
		t.Errorf("trailer line %q", lines[2])
	}
	if strings.Count(lines[2], " via ") != 1 {
		t.Errorf("the client name kept a field separator: %q", lines[2])
	}
}

func TestCreateDescription_EmptyClientFallsBack(t *testing.T) {
	desc, err := createDescription("", "\n")
	if err != nil {
		t.Fatalf("createDescription: %v", err)
	}
	if !strings.HasPrefix(desc, "created-by: forgectl (mcp) via forgectl tasks mcp ") {
		t.Errorf("description %q, want the surface default as the client name", desc)
	}
}
