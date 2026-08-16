package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// TestPrintPlan_RendersUnblessedDefinitionInertly closes the sharpest sink the
// #281 review found. `workflow run --dry-run` skips the blessing gate on
// purpose — it is the review a user performs BEFORE trusting the file — so the
// unblessed definition reaches this renderer, and until now reached it raw. A
// definition that can rewrite the lines describing it can forge its own review.
//
// TOML forbids a raw control byte in the file, so the fixture builds one the
// way an attacker would: a \u escape, which decodes to the real rune.
func TestPrintPlan_RendersUnblessedDefinitionInertly(t *testing.T) {
	for _, tt := range append([]unsafeRune{
		{name: "ESC", r: 0x1b},
		{name: "CSI-C1", r: 0x9b},
		{name: "RIGHT-TO-LEFT-OVERRIDE", r: 0x202e},
		{name: "LINE-FEED", r: 0x0a},
	}, invisibleFormatting...) {
		t.Run(tt.name, func(t *testing.T) {
			hostile := "before" + string(tt.r) + "after"
			plan := workflow.Plan{
				Name:    hostile,
				Version: hostile,
				Steps: []workflow.PlanStep{{
					Uses:    hostile,
					Repo:    hostile,
					Ref:     hostile,
					Globs:   []string{hostile},
					Skill:   hostile,
					Posture: hostile,
					Mode:    hostile,
					From:    hostile,
					To:      hostile,
					Cmd:     hostile,
					Args:    []string{hostile},
				}},
			}
			var out bytes.Buffer
			printPlan(&out, plan)
			assertValueQuoted(t, "printPlan", out.String(), tt.r)
			if !strings.Contains(out.String(), "before") {
				t.Fatalf("printPlan dropped the field text: %q", out.String())
			}
		})
	}
}

// TestPickRepoLabel_IsQuoted covers the interactive twin of
// projectCandidateLine. A local project's name is a directory basename, so
// anyone who can create a directory under the projects dir picks those bytes,
// and the picker is the path an operator actually uses.
//
// This asserts the RENDERER only. huh's option list is not inspectable
// headlessly, so the wiring — that pickRepo still passes its label through this
// renderer — is asserted separately and structurally by
// TestPickerLabelsAreBuiltByAnEscapingRenderer. Neither test is sufficient
// alone: this one passes if the picker stops calling the renderer, and that one
// passes if the renderer stops escaping.
func TestPickRepoLabel_IsQuoted(t *testing.T) {
	for _, tt := range append([]unsafeRune{
		{name: "ESC", r: 0x1b},
		{name: "CSI-C1", r: 0x9b},
		{name: "RIGHT-TO-LEFT-OVERRIDE", r: 0x202e},
	}, invisibleFormatting...) {
		t.Run(tt.name, func(t *testing.T) {
			repo := projects.Repo{Host: "github", Owner: "c", Name: "before" + string(tt.r) + "after"}
			// Assert the fixture is hostile BEFORE the boundary runs. Without
			// this the subtest passes identically on a repo name the rune never
			// reached, which is a green that proves nothing.
			if !strings.ContainsRune(repo.DisplayLine(), tt.r) {
				t.Fatalf("fixture is already inert — the test cannot fail for the right reason")
			}
			label := repoPickerLabel(repo)
			if strings.ContainsRune(label, tt.r) {
				t.Fatalf("picker label passed %U through to the terminal: %q", tt.r, label)
			}
		})
	}
}

// TestWrapDocsTokenDescriptorError_QuotesExactlyOnce pins the review nit that
// QuotePath, unlike the sanitizer it replaced, is not idempotent: handing it an
// already-rendered path produced a doubly-quoted one.
//
// It drives readDocsTokenFile rather than calling the wrapper directly. The
// defect was at the CALL SITE — the wrapper was always correct on a raw path —
// so a test that called the wrapper itself passed while the bug stood. That is
// exactly what a mutation probe caught here, and why this test enters where the
// production code does.
func TestWrapDocsTokenDescriptorError_QuotesExactlyOnce(t *testing.T) {
	file := &injectedDocsTokenFile{
		reader:   &partialErrorReader{data: nil, err: errString("injected read failure")},
		closeErr: errString("injected close failure"),
	}
	_, err := readDocsTokenFile("/safe/token", file)
	if err == nil {
		t.Fatal("a failing read and close must produce an error")
	}
	// Bind the real count into the message. Formatting the WANT into the "got"
	// slot reports the same number on every failure, so the two cases a reader
	// most needs told apart — 0 and 3 — read identically.
	text := err.Error()
	if got := strings.Count(text, `"/safe/token"`); got != 2 {
		t.Fatalf("path quoted %d time(s), want exactly once per joined failure (read + close): %q", got, text)
	}
	for _, doubled := range []string{`\"`, `""`, `"\"`} {
		if strings.Contains(text, doubled) {
			t.Fatalf("path was quoted more than once (%q): %q", doubled, text)
		}
	}
}
