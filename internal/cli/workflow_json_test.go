package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bless"
)

// --- workflow list --json ---

// TestWorkflowListJSON_KeySet pins the exact per-row key set.
func TestWorkflowListJSON_KeySet(t *testing.T) {
	cliRedirectConfigDir(t)

	cmd := newWorkflowListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("workflow list --json: %v", err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("stdout did not parse as a JSON array: %v\n%s", err, out.String())
	}
	if len(rows) == 0 {
		t.Fatal("expected at least the shipped built-in workflows")
	}
	for _, row := range rows {
		if len(row) != 1 {
			t.Fatalf("row keys = %v, want exactly [name]", keysOf(row))
		}
		if _, ok := row["name"]; !ok {
			t.Errorf("missing key %q; got %v", "name", keysOf(row))
		}
	}
}

// TestWorkflowListJSON_RowsMatchHumanTable: the same names, same order, as
// the human `workflow list` output — one per non-blank line.
func TestWorkflowListJSON_RowsMatchHumanTable(t *testing.T) {
	cliRedirectConfigDir(t)

	humanCmd := newWorkflowListCmd()
	var humanOut bytes.Buffer
	humanCmd.SetOut(&humanOut)
	humanCmd.SetArgs(nil)
	if err := humanCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("workflow list: %v", err)
	}
	var wantNames []string
	for _, line := range strings.Split(strings.TrimRight(humanOut.String(), "\n"), "\n") {
		if line != "" {
			wantNames = append(wantNames, line)
		}
	}

	jsonCmd := newWorkflowListCmd()
	var jsonOut bytes.Buffer
	jsonCmd.SetOut(&jsonOut)
	jsonCmd.SetArgs([]string{"--json"})
	if err := jsonCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("workflow list --json: %v", err)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != len(wantNames) {
		t.Fatalf("json rows = %d, human lines = %d", len(rows), len(wantNames))
	}
	for i, want := range wantNames {
		if rows[i].Name != want {
			t.Errorf("row %d = %q, want %q", i, rows[i].Name, want)
		}
	}
}

// TestBuildWorkflowListJSON_EmptyIsArrayNeverNull unit-tests the builder
// directly: ListBuiltins always returns the shipped built-ins, so the
// empty-input case is exercised at the JSON-shape level rather than through
// a real command invocation.
func TestBuildWorkflowListJSON_EmptyIsArrayNeverNull(t *testing.T) {
	rows := buildWorkflowListJSON(nil)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "[]" {
		t.Errorf("empty names = %s, want []", raw)
	}
}

// --- workflow status --json ---

// TestWorkflowStatusJSON_KeySet pins the exact top-level key set for a
// workflow that has run at least once.
func TestWorkflowStatusJSON_KeySet(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}

	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"multi", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status --json: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	want := []string{"name", "has_state", "run_id", "started_at", "updated_at", "steps", "note"}
	if len(decoded) != len(want) {
		t.Fatalf("key set = %v, want exactly %v", keysOf(decoded), want)
	}
	for _, k := range want {
		if _, ok := decoded[k]; !ok {
			t.Errorf("missing key %q; got %v", k, keysOf(decoded))
		}
	}
}

// TestWorkflowStatusJSON_RowsMatchHumanTable: the completed-step rows match
// what the human render shows (one entry per checkpointed step, same uses).
func TestWorkflowStatusJSON_RowsMatchHumanTable(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}

	humanCmd := newWorkflowStatusCmd()
	var humanOut bytes.Buffer
	humanCmd.SetOut(&humanOut)
	humanCmd.SetArgs([]string{"multi"})
	if err := humanCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}

	jsonCmd := newWorkflowStatusCmd()
	var jsonOut bytes.Buffer
	jsonCmd.SetOut(&jsonOut)
	jsonCmd.SetArgs([]string{"multi", "--json"})
	if err := jsonCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status --json: %v", err)
	}

	var decoded struct {
		Name     string `json:"name"`
		HasState bool   `json:"has_state"`
		Steps    []struct {
			Index       int    `json:"index"`
			Uses        string `json:"uses"`
			CompletedAt string `json:"completed_at"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Name != "multi" || !decoded.HasState {
		t.Fatalf("decoded = %+v, want name=multi has_state=true", decoded)
	}
	if len(decoded.Steps) != 1 || decoded.Steps[0].Uses != "run" {
		t.Fatalf("steps = %+v, want one completed step using %q", decoded.Steps, "run")
	}
	if !strings.Contains(humanOut.String(), "1 step(s) complete") {
		t.Fatalf("test setup: human status missing the step count:\n%s", humanOut.String())
	}
}

// TestWorkflowStatusJSON_NoState: has_state is false and steps is [] for a
// never-run workflow, never null.
func TestWorkflowStatusJSON_NoState(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))

	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"multi", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var decoded struct {
		HasState bool                     `json:"has_state"`
		Steps    []workflowStatusStepJSON `json:"steps"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if decoded.HasState {
		t.Errorf("has_state = true, want false: %s", out.String())
	}
	if decoded.Steps == nil || len(decoded.Steps) != 0 {
		t.Errorf("steps = %#v, want a non-nil empty slice: %s", decoded.Steps, out.String())
	}
}

// --- workflow verify --json ---

// TestWorkflowVerifyJSON_KeySet pins the exact top-level key set.
func TestWorkflowVerifyJSON_KeySet(t *testing.T) {
	cliRedirectConfigDir(t)
	swapVerifier(t, &spyVerifier{})

	cmd := newWorkflowVerifyCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"clean-room-review", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("verify --json: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	want := []string{"name", "builtin", "valid"}
	if len(decoded) != len(want) {
		t.Fatalf("key set = %v, want exactly %v", keysOf(decoded), want)
	}
	for _, k := range want {
		if _, ok := decoded[k]; !ok {
			t.Errorf("missing key %q; got %v", k, keysOf(decoded))
		}
	}
}

// TestWorkflowVerifyJSON_RowsMatchHumanTable: builtin/valid track what the
// human sentence says for the same two cases (built-in exempt, blessed and
// valid).
func TestWorkflowVerifyJSON_RowsMatchHumanTable(t *testing.T) {
	cliRedirectConfigDir(t)
	swapVerifier(t, &spyVerifier{})

	cmd := newWorkflowVerifyCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"clean-room-review", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("verify --json: %v", err)
	}
	var decoded struct {
		Name    string `json:"name"`
		Builtin bool   `json:"builtin"`
		Valid   bool   `json:"valid"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Name != "clean-room-review" || !decoded.Builtin || !decoded.Valid {
		t.Errorf("decoded = %+v, want name=clean-room-review builtin=true valid=true", decoded)
	}

	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	swapVerifier(t, fakeVerifier{err: nil})

	cmd2 := newWorkflowVerifyCmd()
	var out2 bytes.Buffer
	cmd2.SetOut(&out2)
	cmd2.SetArgs([]string{"demo", "--json"})
	if err := cmd2.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("verify --json: %v", err)
	}
	var decoded2 struct {
		Builtin bool `json:"builtin"`
		Valid   bool `json:"valid"`
	}
	if err := json.Unmarshal(out2.Bytes(), &decoded2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded2.Builtin || !decoded2.Valid {
		t.Errorf("decoded2 = %+v, want builtin=false valid=true", decoded2)
	}
}

// TestWorkflowVerifyJSON_FailingRunLeavesStdoutEmpty: an unblessed file under
// --json writes nothing to stdout and returns a non-zero-exit error, exactly
// as the human path does — no partial JSON on failure.
func TestWorkflowVerifyJSON_FailingRunLeavesStdoutEmpty(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	swapVerifier(t, fakeVerifier{err: fmt.Errorf("%w", bless.ErrUnblessed)})

	cmd := newWorkflowVerifyCmd()
	// SilenceUsage: a standalone command with no parent root prints cobra's
	// usage block to OutOrStdout() on any RunE error regardless of the real
	// root's setting (a test-harness artifact, not production behavior — the
	// real root sets SilenceUsage). Set it here so this test measures the
	// command's own write behavior, not cobra's default harness noise.
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"demo", "--json"})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected a non-nil error for an unblessed workflow")
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a failing run", out.String())
	}
}
