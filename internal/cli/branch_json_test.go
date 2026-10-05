package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	branchpkg "github.com/cameronsjo/forgectl/internal/branch"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func TestBranchReportJSON_GroupsAreArraysWithFields(t *testing.T) {
	r := branchpkg.Report{
		SafeToDelete: []branchpkg.Classification{{
			Info:   branchpkg.Info{Name: "feat/x", LocalExists: true, UpstreamGone: true},
			Reason: "PR #1 merged",
		}},
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, newBranchReportJSON(r)); err != nil {
		t.Fatal(err)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if got["blocked"] == nil || len(got["blocked"]) != 0 || got["needs_attention"] == nil {
		t.Errorf("empty groups must be [], got %s", buf.String())
	}
	row := got["safe_to_delete"][0]
	if row["name"] != "feat/x" || row["reason"] != "PR #1 merged" || row["local"] != true ||
		row["remote"] != false || row["upstream_gone"] != true {
		t.Errorf("row = %v", row)
	}
}

func TestBranchCmd_JSONRefusedWithApplyBeforeAnyGit(t *testing.T) {
	runner := &exec.FakeRunner{}
	cmd := newBranchCmdForClient(branchpkg.New(runner), theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--json", "--apply"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("error = %v, want a refusal naming --apply", err)
	}
	if runner.Last().Name != "" {
		t.Errorf("git ran despite the refusal: %#v", runner.Last())
	}
}
