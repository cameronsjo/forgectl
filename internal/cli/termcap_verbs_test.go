package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/review"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// capProbe is one field of one verb's text output, filled with a run of a
// rune nothing else in that output uses, far over its cap.
type capProbe struct {
	field string
	r     rune
	cap   int
}

// long is a run of p's rune four times its cap.
func (p capProbe) long() string { return strings.Repeat(string(p.r), p.cap*4) }

// check asserts text shows p's field capped: no run longer than the cap, the
// head of the value still there, and a cut marker present.
func (p capProbe) check(t *testing.T, verb, text, marker string) {
	t.Helper()
	if strings.Contains(text, strings.Repeat(string(p.r), p.cap+1)) {
		t.Errorf("%s printed more than %d runes of %s; the cap did not engage", verb, p.cap, p.field)
	}
	if !strings.Contains(text, strings.Repeat(string(p.r), p.cap/4)) {
		t.Errorf("%s lost the head of %s", verb, p.field)
	}
	if !strings.Contains(text, marker) {
		t.Errorf("%s did not mark %s as cut (want %q)", verb, p.field, marker)
	}
}

// TestTextVerbsCapUntrustedFields proves, per verb #913 swept, that the cap
// engages on the field the issue named: each is fed a value four times its
// cap and must print no more than the cap, keep the head, and say it cut.
//
// Mutations that turn it red, one per row: print the field through
// termsafe.SafeLine instead of its helper (report.Refusal in writeDrainHuman,
// it.Error in writeRepairHuman, p.Title in renderPRTable, it.Title in
// renderReviewTable, s.Cwd in printSessions, s.SessionID in printOutdated,
// d.RelPath in printDocsList), or raise the helper's cap (textMaxRunes to
// 100000 turns the pr drain row red).
func TestTextVerbsCapUntrustedFields(t *testing.T) {
	store := func(t *testing.T) *pr.ReviewedStore {
		return pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json"))
	}
	// Literal caps, not the constants: a test derived from textMaxRunes would
	// raise its own bound along with it and pass no matter what it is set to.
	text := func(p capProbe) capProbe { p.cap = 1280; return p }
	title := func(p capProbe) capProbe { p.cap = 256; return p }
	label := func(p capProbe) capProbe { p.cap = 64; return p }
	path := func(p capProbe) capProbe { p.cap = 512; return p }

	t.Run("pr drain", func(t *testing.T) {
		p := text(capProbe{field: "the refusal", r: 'α'})
		var out bytes.Buffer
		writeDrainHuman(&out, pr.DrainReport{Pass: 1, Refusal: p.long()}, false, 0)
		p.check(t, "pr drain", out.String(), termsafe.TruncatedMarker)
	})

	t.Run("pr repair", func(t *testing.T) {
		p := text(capProbe{field: "an item error", r: 'β'})
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		report := pr.RepairReport{Items: []pr.RepairItem{{Ref: "o/r#1", Outcome: "inspect", Error: p.long()}}}
		if err := writeRepairHuman(cmd, report, false); err != nil {
			t.Fatal(err)
		}
		p.check(t, "pr repair", out.String(), termsafe.TruncatedMarker)
	})

	t.Run("pr prs", func(t *testing.T) {
		p := title(capProbe{field: "a PR title", r: 'γ'})
		var out, errOut bytes.Buffer
		prs := []pr.PR{{Ref: pr.Ref{Owner: "o", Repo: "r", Number: 1}, Title: p.long(), State: "OPEN"}}
		if err := renderPRTable(&out, &errOut, prs, store(t), theme.Theme{}.Styles().Muted, 0); err != nil {
			t.Fatal(err)
		}
		p.check(t, "pr prs", out.String(), termsafe.TruncatedMarker)
	})

	t.Run("review list", func(t *testing.T) {
		p := title(capProbe{field: "an item title", r: 'δ'})
		var out, errOut bytes.Buffer
		items := []review.Item{{Kind: review.KindPR, Host: "github.com", Owner: "o", Repo: "r", Number: 1, Title: p.long(), State: "OPEN"}}
		if err := renderReviewTable(&out, &errOut, items, store(t), theme.Theme{}.Styles().Muted); err != nil {
			t.Fatal(err)
		}
		p.check(t, "review list", out.String(), termsafe.TruncatedMarker)
	})

	t.Run("resume ls", func(t *testing.T) {
		p := path(capProbe{field: "the cwd", r: 'ε'})
		var out, errOut bytes.Buffer
		s := resume.Session{ID: "s1", Repo: "r", Branch: "b", Cwd: "/" + p.long() + "/leaf", LastActive: time.Now()}
		if err := printSessions(&out, &errOut, []resume.Session{s}, false); err != nil {
			t.Fatal(err)
		}
		p.check(t, "resume ls", out.String(), "…/leaf")
	})

	t.Run("resume outdated", func(t *testing.T) {
		p := label(capProbe{field: "the session id", r: 'ζ'})
		var out bytes.Buffer
		list := []resume.OutdatedSession{{SessionID: p.long(), Cwd: "/w", Status: "idle", Version: "2.1.9", InstalledVersion: "2.1.10"}}
		if err := printOutdated(&out, list, false, false); err != nil {
			t.Fatal(err)
		}
		p.check(t, "resume outdated", out.String(), termsafe.TruncatedMarker)
	})

	t.Run("docs list", func(t *testing.T) {
		p := path(capProbe{field: "the relative path", r: 'η'})
		docs := []docspkg.Doc{{RootLabel: "docs", RelPath: p.long() + "/leaf.md", AbsPath: "/r/x.md", Title: "T"}}
		out, _ := renderCmd(t, func(cmd *cobra.Command) error { return printDocsList(cmd, docs, false) })
		p.check(t, "docs list", out, "…/leaf.md")
	})
}

// TestPrintDocsList_OrdinaryRowKeepsItsColumn pins the #913 layout call: the
// relative path is capped without quotes, so an ordinary row prints exactly
// as it did before the cap, its title starting at the same column.
//
// Mutation that turns it red: render d.RelPath through safePath (quoted) in
// printDocsList.
func TestPrintDocsList_OrdinaryRowKeepsItsColumn(t *testing.T) {
	docs := []docspkg.Doc{{RootLabel: "docs", RelPath: "guide/a.md", AbsPath: "/r/guide/a.md", Title: "A"}}
	out, _ := renderCmd(t, func(cmd *cobra.Command) error { return printDocsList(cmd, docs, false) })
	want := "docs             guide/a.md                                       A\n"
	if out != want {
		t.Errorf("docs list row = %q, want %q", out, want)
	}
}
