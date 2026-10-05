package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// runDashJSON runs `pr dash --json` against a sessions dir and a canned gh
// search result, returning stdout and stderr.
func runDashJSON(t *testing.T, sessionsDir, searchJSON string) (string, string) {
	t.Helper()
	client := pr.New(dashRunner(searchJSON), pr.WithSessionsDir(sessionsDir))
	cmd := newPrDashCmdForClient(client, filepath.Join(t.TempDir(), "r.json"), theme.Theme{})
	cmd.SetArgs([]string{"--json"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("dash --json: %v", err)
	}
	return stdout.String(), stderr.String()
}

func TestDashJSON_EmptySectionsEncodeAsArrays(t *testing.T) {
	out, _ := runDashJSON(t, t.TempDir(), "[]")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	for _, key := range []string{"active_reviews", "awaiting_you", "your_open"} {
		if got := strings.TrimSpace(string(raw[key])); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
	if len(raw) != 3 {
		t.Errorf("got %d top-level keys, want exactly 3: %s", len(raw), out)
	}
}

func TestDashJSON_PRRowsMatchPrPrsShape(t *testing.T) {
	search := "[" + prSearchRow("cameronsjo/forgectl", 42) + "]"
	out, _ := runDashJSON(t, t.TempDir(), search)
	var got prDashJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(got.AwaitingYou) != 1 || got.AwaitingYou[0].Ref != "cameronsjo/forgectl#42" {
		t.Errorf("awaiting_you = %+v, want the one searched PR", got.AwaitingYou)
	}
	if got.AwaitingYou[0].Number != 42 || got.AwaitingYou[0].Repo != "cameronsjo/forgectl" {
		t.Errorf("PR row lost repo/number: %+v", got.AwaitingYou[0])
	}
}

func TestDashJSON_ActiveReviewCarriesWorkspacePhaseAndReason(t *testing.T) {
	dir := t.TempDir()
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 7}
	seedPhasedSummaries(t, dir, []phasedRecord{{
		ref: ref, phase: pr.PhaseNeedsRepair, repairReason: "window vanished", withWorkspace: false,
	}})
	out, _ := runDashJSON(t, dir, "[]")
	var got prDashJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(got.ActiveReviews) != 1 {
		t.Fatalf("active_reviews = %+v, want one row", got.ActiveReviews)
	}
	row := got.ActiveReviews[0]
	if row.Ref != ref.String() || row.Phase != string(pr.PhaseNeedsRepair) || row.RepairReason != "window vanished" {
		t.Errorf("row = %+v", row)
	}
	if row.Workspace == "live" || row.Workspace == "" {
		t.Errorf("a needs-repair record with no workspace reported workspace %q", row.Workspace)
	}
}

func TestDashJSON_StdoutIsOnlyJSON(t *testing.T) {
	out, _ := runDashJSON(t, t.TempDir(), "[]")
	dec := json.NewDecoder(strings.NewReader(out))
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.More() {
		t.Errorf("stdout carries more than one JSON value:\n%s", out)
	}
	if strings.Contains(out, "active reviews") {
		t.Errorf("human section header leaked into --json stdout:\n%s", out)
	}
}
