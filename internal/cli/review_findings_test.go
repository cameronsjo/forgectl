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
// and the picker is the path an operator actually uses. The label is asserted
// through the exported DisplayLine the picker feeds to huh, since huh's option
// list is not constructible headlessly.
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
func TestWrapDocsTokenDescriptorError_QuotesExactlyOnce(t *testing.T) {
	got := wrapDocsTokenDescriptorError("read", "/safe/token", errString("boom")).Error()
	if want := `read token file "/safe/token": boom`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if strings.Contains(got, `\"`) || strings.Contains(got, `""`) {
		t.Fatalf("path was quoted more than once: %q", got)
	}
}
