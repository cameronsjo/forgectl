package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/sessions"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// ptrTime is a test helper for the concordance's nullable timestamps.
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
// bogus value — the omitempty contract for a nullable concordance column.
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

// Untrusted concordance content must render inert: a control byte in a title/snippet
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
// through --json unless the DTO builder strips them. Plant both in concordance-sourced
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

// TestWriteSearchHitsJSON pins `sessions search --json` (#482): valid JSON,
// the documented field set, and the stored value carried unaltered (no
// termsafe.SafeLine quoting on the machine path).
func TestWriteSearchHitsJSON(t *testing.T) {
	hits := []sessions.SearchHit{{
		Path: "hearth/colima.md", Title: "Colima split brain", Project: "hearth",
		Type: "field-report", Machine: "m1", Rank: 0.5, Snippet: "phantom <<default>> VM",
	}}
	var out bytes.Buffer
	if err := writeSearchHitsJSON(&out, hits); err != nil {
		t.Fatalf("write: %v", err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(raw) != 1 {
		t.Fatalf("want 1 hit, got %d", len(raw))
	}
	for _, k := range []string{"path", "title", "type", "project", "machine", "rank", "snippet"} {
		if raw[0][k] == nil {
			t.Errorf("hit is missing field %q: %s", k, out.String())
		}
	}
	if len(raw[0]) != 7 {
		t.Errorf("hit has %d fields, want 7: %s", len(raw[0]), out.String())
	}
	var got []searchHitJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g := got[0]
	if g.Path != "hearth/colima.md" || g.Title != "Colima split brain" || g.Project != "hearth" ||
		g.Type != "field-report" || g.Machine != "m1" || g.Rank != 0.5 || g.Snippet != "phantom <<default>> VM" {
		t.Errorf("fields off: %+v", g)
	}
}

func TestWriteSearchHitsJSON_EmptyIsArray(t *testing.T) {
	var out bytes.Buffer
	if err := writeSearchHitsJSON(&out, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("empty --json result must be [], got %q", out.String())
	}
}

func TestSessionsSearch_HasJSONFlag(t *testing.T) {
	parent := newSessionsCmd(module.Deps{Runner: &exec.FakeRunner{}})
	search := findChild(parent, "search")
	if search == nil {
		t.Fatal("search did not register")
	}
	if search.Flags().Lookup("json") == nil {
		t.Error("sessions search has no --json flag (ADR-0008 rule 2)")
	}
}

func TestSyncReceiptJSON_MissingIsArrayAndFailsExit(t *testing.T) {
	r := &sessions.Receipt{SessionsFound: 3, SessionsUpserted: 2, Missing: []string{"abc"}}
	var buf bytes.Buffer
	if err := writeJSON(&buf, newReceiptJSON(r)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if got["sessions_found"] != 3.0 || got["complete"] != false {
		t.Errorf("receipt = %v", got)
	}
	if m, ok := got["missing"].([]any); !ok || len(m) != 1 || m[0] != "abc" {
		t.Errorf("missing = %v", got["missing"])
	}
	if receiptError(r) == nil {
		t.Error("a missing session must still fail the run")
	}
}

func TestSyncReceiptJSON_CleanRunHasEmptyMissingArray(t *testing.T) {
	r := &sessions.Receipt{SessionsFound: 1, SessionsUpserted: 1}
	var buf bytes.Buffer
	if err := writeJSON(&buf, newReceiptJSON(r)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"missing": []`) || !strings.Contains(buf.String(), `"complete": true`) {
		t.Errorf("clean receipt = %s", buf.String())
	}
	if err := receiptError(r); err != nil {
		t.Errorf("clean run errored: %v", err)
	}
}

func TestFinishSync_MissingFailsRunInBothRenderings(t *testing.T) {
	for _, asJSON := range []bool{true, false} {
		r := &sessions.Receipt{SessionsFound: 2, SessionsUpserted: 1, Missing: []string{"abc"}}
		var buf bytes.Buffer
		err := finishSync(&buf, r, asJSON)
		if ExitCode(err) != 1 {
			t.Errorf("asJSON=%v: exit code = %d, want 1", asJSON, ExitCode(err))
		}
		if asJSON {
			// The receipt is the verdict: a silent exit (forgectl#862).
			if _, ok := err.(*silentCodedError); !ok {
				t.Errorf("asJSON=true: error = %T %v, want a silentCodedError", err, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "reconcile failed") {
			t.Errorf("asJSON=false: error = %v, want the reconcile failure", err)
		}
		if asJSON {
			var got map[string]any
			if jerr := json.Unmarshal(buf.Bytes(), &got); jerr != nil || got["complete"] != false {
				t.Errorf("JSON not emitted before the error: %v\n%s", jerr, buf.String())
			}
		} else if !strings.Contains(buf.String(), "MISSING abc") {
			t.Errorf("human output = %q, want a MISSING line", buf.String())
		}
	}
}

func TestFinishSync_CompleteAndDryRunSucceed(t *testing.T) {
	for _, r := range []*sessions.Receipt{{SessionsFound: 1, SessionsUpserted: 1}, {DryRun: true, Missing: []string{"x"}}} {
		for _, asJSON := range []bool{true, false} {
			if err := finishSync(&bytes.Buffer{}, r, asJSON); err != nil {
				t.Errorf("asJSON=%v receipt=%+v: unexpected error %v", asJSON, r, err)
			}
		}
	}
}

// TestSessionsText_CapsRunbookTitles pins forgectl#891 item 5: the indexer
// stores a runbook title uncut, so every `sessions` text line that prints one
// caps it at titleMaxRunes, while --json carries it whole.
//
// Mutations that turn it red, one per row: print h.Title through termsafe.SafeLine in
// printSearchHits (search), h.Title in printWhyHits (why), or a.Title in
// printLastSession (last).
func TestSessionsText_CapsRunbookTitles(t *testing.T) {
	long := strings.Repeat("t", titleMaxRunes*4)
	ts := ptrTime("2026-07-09T11:00:00Z")
	for _, tt := range []struct {
		sink       string
		text, json func(cmd *cobra.Command) error
	}{
		{
			sink: "search",
			text: func(cmd *cobra.Command) error {
				return printSearchHits(cmd.OutOrStdout(), []sessions.SearchHit{{Path: "p/x.md", Title: long}})
			},
			json: func(cmd *cobra.Command) error {
				return writeSearchHitsJSON(cmd.OutOrStdout(), []sessions.SearchHit{{Path: "p/x.md", Title: long}})
			},
		},
		{
			sink: "why",
			text: func(cmd *cobra.Command) error {
				return printWhyHits(cmd, []sessions.WhyHit{{SessionID: "s1", LastTs: ts, Title: long, Path: "p/x.md"}}, false)
			},
			json: func(cmd *cobra.Command) error {
				return printWhyHits(cmd, []sessions.WhyHit{{SessionID: "s1", LastTs: ts, Title: long, Path: "p/x.md"}}, true)
			},
		},
		{
			sink: "last",
			text: func(cmd *cobra.Command) error {
				return printLastSession(cmd, "p", &sessions.SessionSummary{SessionID: "s1", LastTs: ts,
					Artifacts: []sessions.Artifact{{Type: "handoff", Title: long, Path: "p/x.md"}}}, false)
			},
			json: func(cmd *cobra.Command) error {
				return printLastSession(cmd, "p", &sessions.SessionSummary{SessionID: "s1", LastTs: ts,
					Artifacts: []sessions.Artifact{{Type: "handoff", Title: long, Path: "p/x.md"}}}, true)
			},
		},
	} {
		text, _ := renderCmd(t, tt.text)
		if strings.Contains(text, strings.Repeat("t", titleMaxRunes+1)) {
			t.Errorf("%s text printed more than %d runes of the title", tt.sink, titleMaxRunes)
		}
		if !strings.Contains(text, strings.Repeat("t", titleMaxRunes/2)) {
			t.Errorf("%s text lost the title's head: %q", tt.sink, text)
		}
		asJSON, _ := renderCmd(t, tt.json)
		if !strings.Contains(asJSON, long) {
			t.Errorf("%s --json did not carry the title whole", tt.sink)
		}
	}
}

// TestSessionsText_CapsSnippets pins the #891 review finding: ts_headline's
// MaxWords bounds words, not characters, so a match snippet can be tens of
// thousands of characters. search and why cap it at snippetMaxRunes
// in text output, while --json carries it whole.
//
// Mutations that turn it red, one per row: print h.Snippet through termsafe.SafeLine
// in printSearchHits (search) or in printWhyHits (why).
func TestSessionsText_CapsSnippets(t *testing.T) {
	long := strings.Repeat("s", snippetMaxRunes*4)
	ts := ptrTime("2026-07-09T11:00:00Z")
	search := []sessions.SearchHit{{Path: "p/x.md", Title: "T", Snippet: long}}
	why := []sessions.WhyHit{{SessionID: "s1", LastTs: ts, Title: "T", Path: "p/x.md", Snippet: long}}
	for _, tt := range []struct {
		sink       string
		text, json func(cmd *cobra.Command) error
	}{
		{
			sink: "search",
			text: func(cmd *cobra.Command) error { return printSearchHits(cmd.OutOrStdout(), search) },
			json: func(cmd *cobra.Command) error { return writeSearchHitsJSON(cmd.OutOrStdout(), search) },
		},
		{
			sink: "why",
			text: func(cmd *cobra.Command) error { return printWhyHits(cmd, why, false) },
			json: func(cmd *cobra.Command) error { return printWhyHits(cmd, why, true) },
		},
	} {
		text, _ := renderCmd(t, tt.text)
		if strings.Contains(text, strings.Repeat("s", snippetMaxRunes+1)) {
			t.Errorf("%s text printed more than %d runes of the snippet", tt.sink, snippetMaxRunes)
		}
		if !strings.Contains(text, strings.Repeat("s", snippetMaxRunes/2)) {
			t.Errorf("%s text lost the snippet's head: %q", tt.sink, text)
		}
		asJSON, _ := renderCmd(t, tt.json)
		if !strings.Contains(asJSON, long) {
			t.Errorf("%s --json did not carry the snippet whole", tt.sink)
		}
	}
}

// sessionsTextFieldMaxRunes is how many runes of one field's value a
// `sessions` text sink may print, by field name. The numbers are literal on
// purpose: deriving them from labelMaxRunes and its siblings would let
// a raised cap raise its own bound. Labels are 64, titles 256, snippets 320;
// Path is 512 input runes (QuotePathMax, which a plain fill escapes to
// itself); "repo" is the argument printLastSession echoes on a miss.
var sessionsTextFieldMaxRunes = map[string]int{
	"SessionID": 64, "Project": 64, "Model": 64, "Machine": 64,
	"GitBranch": 64, "Type": 64, "Missing": 64, "repo": 64,
	"Title": 256, "Snippet": 320, "Path": 512,
}

// filledField is one value fillEveryString planted, under its field name.
type filledField struct{ name, value string }

// fillEveryString sets every exported string field of the struct v points at
// to a fresh value from next, recursing into slices of structs (one element)
// and giving string slices one element. It fails the test on any other field
// kind that is not on its text-free allowlist (numbers, bools, time.Time and
// *time.Time), so a *string, map or nested struct added later cannot stay at
// its zero value and leave the pin green. It returns what it planted.
func fillEveryString(t testing.TB, v reflect.Value, next func() string) []filledField {
	t.Helper()
	var filled []filledField
	timeType := reflect.TypeFor[time.Time]()
	for i := range v.NumField() {
		f, sf := v.Field(i), v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		switch {
		case f.Kind() == reflect.String:
			val := next()
			f.SetString(val)
			filled = append(filled, filledField{sf.Name, val})
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			val := next()
			f.Set(reflect.ValueOf([]string{val}))
			filled = append(filled, filledField{sf.Name, val})
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.Struct:
			elem := reflect.New(f.Type().Elem()).Elem()
			filled = append(filled, fillEveryString(t, elem, next)...)
			f.Set(reflect.Append(reflect.MakeSlice(f.Type(), 0, 1), elem))
		case f.Type() == timeType, f.Kind() == reflect.Pointer && f.Type().Elem() == timeType:
		default:
			switch f.Kind() {
			case reflect.Bool,
				reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
				reflect.Float32, reflect.Float64:
			default:
				t.Fatalf("fillEveryString: field %s.%s is a %s, which it cannot fill; teach it the kind, or allowlist it if it carries no text",
					v.Type().Name(), sf.Name, f.Type())
			}
		}
	}
	return filled
}

// fatalRecorder is a testing.TB whose Fatalf records the message and unwinds
// by panic, so a test can watch a helper call Fatalf without failing itself.
type fatalRecorder struct {
	testing.TB
	msg string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	panic(r)
}

// TestFillEveryString_RejectsKindsItCannotFill pins the default arm: a field
// kind fillEveryString does not handle must fail the fixture, not stay zero.
//
// Mutation that turns it red: delete fillEveryString's inner `default` arm.
func TestFillEveryString_RejectsKindsItCannotFill(t *testing.T) {
	for name, v := range map[string]any{
		"*string": &struct{ P *string }{},
		"map":     &struct{ M map[string]string }{},
		"struct":  &struct{ S struct{ X string } }{},
	} {
		rec := &fatalRecorder{TB: t}
		func() {
			defer func() {
				if p := recover(); p != nil && p != any(rec) {
					panic(p)
				}
			}()
			fillEveryString(rec, reflect.ValueOf(v).Elem(), func() string { return "x" })
		}()
		if !strings.Contains(rec.msg, "cannot fill") {
			t.Errorf("%s field: fillEveryString did not fail the fixture (msg %q)", name, rec.msg)
		}
	}
}

// TestSessionsText_EveryFieldCapped pins the structure the #891 review asked
// for: every string field of every struct a `sessions` text printer renders
// goes through a capped helper. Each struct gets a distinct 5000-rune value in
// EVERY string field by reflection, one Greek letter per field, so a field
// added later without a cap turns this red without anyone remembering to add a
// row. The text output must print each field at least once and at most its
// literal budget in sessionsTextFieldMaxRunes; --json carries every value
// whole. Path has no allowlist since forgectl#894.
//
// Mutations that turn it red: print h.Type through termsafe.SafeLine in
// printSearchHits, h.Project through termsafe.SafeLine in printSearchHits, or
// s.GitBranch through termsafe.SafeLine in printLastSession; raise
// labelMaxRunes to 200; print h.Type through safeTitle in
// printSearchHits; make safePath return termsafe.SafeLine(s).
func TestSessionsText_EveryFieldCapped(t *testing.T) {
	letter := 'α' // Greek alpha: no fixed text in these printers uses Greek
	next := func() string {
		if letter > 'ω' {
			t.Fatal("ran out of distinct Greek letters for the fields")
		}
		v := strings.Repeat(string(letter), 5000)
		letter++
		return v
	}
	var hit sessions.SearchHit
	var why sessions.WhyHit
	var last sessions.SessionSummary
	var receipt sessions.Receipt
	filled := map[string][]filledField{}
	for name, v := range map[string]any{"SearchHit": &hit, "WhyHit": &why, "SessionSummary": &last, "Receipt": &receipt} {
		if filled[name] = fillEveryString(t, reflect.ValueOf(v).Elem(), next); len(filled[name]) == 0 {
			t.Fatalf("%s: no string field filled; the fixture tests nothing", name)
		}
	}
	repo := filledField{"repo", next()}
	for _, tt := range []struct {
		sink       string
		text, json func(cmd *cobra.Command) error
		fields     []filledField
	}{
		{
			sink: "search",
			text: func(cmd *cobra.Command) error {
				return printSearchHits(cmd.OutOrStdout(), []sessions.SearchHit{hit})
			},
			json: func(cmd *cobra.Command) error {
				return writeSearchHitsJSON(cmd.OutOrStdout(), []sessions.SearchHit{hit})
			},
			fields: filled["SearchHit"],
		},
		{
			sink:   "why",
			text:   func(cmd *cobra.Command) error { return printWhyHits(cmd, []sessions.WhyHit{why}, false) },
			json:   func(cmd *cobra.Command) error { return printWhyHits(cmd, []sessions.WhyHit{why}, true) },
			fields: filled["WhyHit"],
		},
		{
			sink:   "last",
			text:   func(cmd *cobra.Command) error { return printLastSession(cmd, repo.value, &last, false) },
			json:   func(cmd *cobra.Command) error { return printLastSession(cmd, repo.value, &last, true) },
			fields: filled["SessionSummary"],
		},
		{
			sink:   "last (no session)",
			text:   func(cmd *cobra.Command) error { return printLastSession(cmd, repo.value, nil, false) },
			fields: []filledField{repo},
		},
		{
			sink: "sync receipt",
			text: func(cmd *cobra.Command) error {
				_ = printReceipt(cmd.OutOrStdout(), &receipt) // MISSING rows fail the receipt by design
				return nil
			},
			fields: filled["Receipt"],
		},
	} {
		text, _ := renderCmd(t, tt.text)
		if !strings.Contains(text, strings.TrimSpace(termsafe.TruncatedMarker)) {
			t.Errorf("%s text shows no truncation marker, so no cap engaged: %q", tt.sink, text)
		}
		for _, f := range tt.fields {
			budget, ok := sessionsTextFieldMaxRunes[f.name]
			if !ok {
				t.Fatalf("%s: field %s has no budget in sessionsTextFieldMaxRunes; add one", tt.sink, f.name)
			}
			r, _ := utf8.DecodeRuneInString(f.value)
			switch n := strings.Count(text, string(r)); {
			case n == 0:
				t.Errorf("%s text never printed field %s", tt.sink, f.name)
			case n > budget:
				t.Errorf("%s text printed %d runes of field %s, over its budget of %d", tt.sink, n, f.name, budget)
			}
		}
		if tt.json == nil {
			continue
		}
		asJSON, _ := renderCmd(t, tt.json)
		for _, f := range tt.fields {
			if !strings.Contains(asJSON, f.value) {
				t.Errorf("%s --json did not carry field %s whole", tt.sink, f.name)
			}
		}
	}
}

// TestPrintLastSession_MissEscapesRepoOnce pins the #894 nit: the miss line
// quotes the repo once, so an ESC in it reads \x1b, not \\x1b.
//
// Mutation that turns it red: format safeLabel(repo) with %q again.
func TestPrintLastSession_MissEscapesRepoOnce(t *testing.T) {
	stdout, _ := renderCmd(t, func(cmd *cobra.Command) error {
		return printLastSession(cmd, "a\x1bb", nil, false)
	})
	if want := `no sessions recorded for "a\x1bb"` + "\n"; stdout != want {
		t.Errorf("miss line = %q, want %q", stdout, want)
	}
}
