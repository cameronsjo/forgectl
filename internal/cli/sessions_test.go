package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/sessions"
)

// ptrTime is a test helper for the mart's nullable timestamps.
func ptrTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// renderCmd runs f with a cobra command wired to captured stdout/stderr —
// mirrors the SetOut/SetErr buffer pattern the other cli tests use.
func renderCmd(t *testing.T, f func(cmd *cobra.Command) error) (stdout, stderr string) {
	t.Helper()
	cmd := &cobra.Command{}
	var out, err bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&err)
	if e := f(cmd); e != nil {
		t.Fatalf("render: %v", e)
	}
	return out.String(), err.String()
}

// The why/last subcommands must register so `forgectl sessions why|last` runs
// them rather than falling through to a "did you mean" suggestion.
func TestSessionsSubcommandsRegister(t *testing.T) {
	parent := newSessionsCmd(module.Deps{Runner: &exec.FakeRunner{}})
	for _, name := range []string{"why", "last", "search", "sync"} {
		if c := findChild(parent, name); c == nil {
			t.Errorf("subcommand %q did not register on `sessions`", name)
		}
	}
}

func TestPrintWhyHits_JSON(t *testing.T) {
	hits := []sessions.WhyHit{{
		SessionID: "11111111-1111-1111-1111-111111111111",
		Project:   "hearth", Model: "claude-fable-5",
		LastTs: ptrTime("2026-07-09T11:00:00Z"), Committed: true,
		Title: "Colima split brain", Type: "field-report",
		Path: "hearth/colima-split-brain.md", Snippet: "phantom <<default>> VM",
	}}
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printWhyHits(cmd, hits, true)
	})
	var got []whyDTO
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, stdout)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 hit, got %d", len(got))
	}
	g := got[0]
	if g.SessionID != hits[0].SessionID || g.Repo != "hearth" ||
		g.Date != "2026-07-09T11:00:00Z" || g.Intent != "Colima split brain" ||
		g.Link != "hearth/colima-split-brain.md" || g.RunbookType != "field-report" ||
		!g.Committed || g.KeyDecisions != "phantom <<default>> VM" {
		t.Errorf("DTO fields off: %+v", g)
	}
}

// A hit with no timestamp must omit the date key rather than emit a blank or
// bogus value — the omitempty contract for a nullable mart column.
func TestPrintWhyHits_JSON_OmitsMissingDate(t *testing.T) {
	hits := []sessions.WhyHit{{SessionID: "s1", Project: "p", Path: "p/x.md"}}
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printWhyHits(cmd, hits, true)
	})
	if strings.Contains(stdout, `"date"`) {
		t.Errorf("missing timestamp should omit the date key:\n%s", stdout)
	}
}

// The --json path for no hits must emit an empty JSON array, not null — a
// pipe consumer can iterate it without a null guard.
func TestPrintWhyHits_JSONEmpty(t *testing.T) {
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printWhyHits(cmd, nil, true)
	})
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty --json result must be [], got %q", stdout)
	}
}

func TestPrintWhyHits_HumanEmpty(t *testing.T) {
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printWhyHits(cmd, nil, false)
	})
	if !strings.Contains(stdout, "no sessions matched") {
		t.Errorf("empty result should say so, got %q", stdout)
	}
}

// Untrusted mart content must render inert: a control byte in a title/snippet
// is stripped before it reaches the terminal.
func TestPrintWhyHits_HumanSanitizesControlBytes(t *testing.T) {
	hits := []sessions.WhyHit{{
		SessionID: "s1\x1bpwned", Project: "p", LastTs: ptrTime("2026-07-09T11:00:00Z"),
		Title: "safe\x1b]0;pwned\x07title", Path: "p/x.md", Snippet: "ok",
	}}
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printWhyHits(cmd, hits, false)
	})
	if strings.ContainsRune(stdout, '\x1b') || strings.ContainsRune(stdout, '\x07') {
		t.Errorf("control bytes leaked into human output: %q", stdout)
	}
	if !strings.Contains(stdout, "safe") || !strings.Contains(stdout, "title") {
		t.Errorf("sanitize dropped legible text: %q", stdout)
	}
}

// encoding/json escapes only 0x00-0x1F, so DEL (U+007F) and the C1 range
// (U+0080-U+009F, incl. U+009B = single-byte CSI) would reach a terminal raw
// through --json unless the DTO builder strips them. Plant both in mart-sourced
// fields (via rune constants, to keep the source clean ASCII) and assert the
// emitted JSON carries neither raw byte.
func TestPrintWhyHits_JSONStripsC1AndDEL(t *testing.T) {
	c1, del := string(rune(0x9b)), string(rune(0x7f))
	hits := []sessions.WhyHit{{
		SessionID: "s1", Project: "p", LastTs: ptrTime("2026-07-09T11:00:00Z"),
		Title: "ti" + c1 + "tle", Path: "p/x.md", Snippet: "sni" + del + "ppet",
	}}
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printWhyHits(cmd, hits, true)
	})
	if strings.ContainsRune(stdout, 0x9b) || strings.ContainsRune(stdout, 0x7f) {
		t.Errorf("C1/DEL leaked into why --json output: %q", stdout)
	}
}

func TestPrintLastSession_JSONStripsC1AndDEL(t *testing.T) {
	c1, del := string(rune(0x9b)), string(rune(0x7f))
	s := &sessions.SessionSummary{
		SessionID: "s1", Project: "p", LastTs: ptrTime("2026-07-10T08:15:00Z"),
		Artifacts: []sessions.Artifact{{Type: "handoff", Title: "ti" + c1 + "tle", Path: "p/x" + del + ".md"}},
	}
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printLastSession(cmd, "p", s, true)
	})
	if strings.ContainsRune(stdout, 0x9b) || strings.ContainsRune(stdout, 0x7f) {
		t.Errorf("C1/DEL leaked into last --json output: %q", stdout)
	}
}

func TestPrintLastSession_JSON(t *testing.T) {
	s := &sessions.SessionSummary{
		SessionID: "33333333-3333-3333-3333-333333333333",
		Project:   "forgectl", GitBranch: "main", Model: "claude-sonnet-5",
		Machine: "it-machine", FirstTs: ptrTime("2026-07-10T08:00:00Z"),
		LastTs: ptrTime("2026-07-10T08:15:00Z"), Committed: false,
		Artifacts: []sessions.Artifact{{Type: "handoff", Title: "H", Path: "forgectl/h.md"}},
	}
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printLastSession(cmd, "forgectl", s, true)
	})
	var got lastDTO
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json output invalid: %v\n%s", err, stdout)
	}
	if got.SessionID != s.SessionID || got.Repo != "forgectl" || got.Branch != "main" ||
		got.LastTs != "2026-07-10T08:15:00Z" || len(got.Artifacts) != 1 ||
		got.Artifacts[0].Type != "handoff" {
		t.Errorf("last DTO off: %+v", got)
	}
}

// A repo with no session is a clean miss, not an error: null in --json.
func TestPrintLastSession_JSONNull(t *testing.T) {
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printLastSession(cmd, "nope", nil, true)
	})
	if strings.TrimSpace(stdout) != "null" {
		t.Errorf("missing session should emit JSON null, got %q", stdout)
	}
}

func TestPrintLastSession_Human(t *testing.T) {
	cases := []struct {
		name    string
		summary *sessions.SessionSummary
		want    string
		absent  string
	}{
		{
			name:    "no session",
			summary: nil,
			want:    `no sessions recorded for "ghost"`,
		},
		{
			name: "session without artifacts",
			summary: &sessions.SessionSummary{
				SessionID: "s1", Project: "ghost", LastTs: ptrTime("2026-07-10T08:15:00Z"),
			},
			want: "no field report or handoff recorded",
		},
		{
			name: "session with artifacts",
			summary: &sessions.SessionSummary{
				SessionID: "s1", Project: "ghost", Committed: true,
				LastTs:    ptrTime("2026-07-10T08:15:00Z"),
				Artifacts: []sessions.Artifact{{Type: "field-report", Title: "FR", Path: "ghost/fr.md"}},
			},
			want:   "field-report",
			absent: "no field report or handoff recorded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
				return printLastSession(cmd, "ghost", tc.summary, false)
			})
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("want %q in output, got %q", tc.want, stdout)
			}
			if tc.absent != "" && strings.Contains(stdout, tc.absent) {
				t.Errorf("did not want %q in output, got %q", tc.absent, stdout)
			}
		})
	}
}

func TestFmtTs(t *testing.T) {
	if got := fmtTs(nil); got != "" {
		t.Errorf("nil timestamp should format empty for omitempty, got %q", got)
	}
	if got := humanTs(nil); got != "unknown" {
		t.Errorf("nil timestamp should read 'unknown' for humans, got %q", got)
	}
	if got := fmtTs(ptrTime("2026-07-10T08:15:00Z")); got != "2026-07-10T08:15:00Z" {
		t.Errorf("RFC3339 round-trip off: %q", got)
	}
}
