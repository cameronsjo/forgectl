package cli

// Test plan for pr_repair.go (forgectl#299 Task 2)
//
// newPrRepairCmd (Classification: cobra command, ADR-0008 surface)
//   [x] Inspect with nothing to settle prints "no records need repair", exit 0
//   [x] --json emits the report object, and [] rather than null when empty
//   [x] --apply with no breadcrumb refuses, naming the inspect command
//   [x] --apply with two modes refuses, naming the set and the two given
//   [x] --rollback off a TTY without --yes refuses, and mutates nothing
//   [x] --history --json returns the rows

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

func repairCmdClient(t *testing.T, sessionsDir string) *pr.Client {
	t.Helper()
	return pr.New(prListRunner(nil), pr.WithSessionsDir(sessionsDir),
		pr.WithTmuxSession("forgectl"), pr.WithTTYCheck(func() bool { return false }))
}

func runPrRepair(t *testing.T, client *pr.Client, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newPrRepairCmd(client)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// seedRepairRecord writes one v2 record at the given phase straight into the
// sessions dir, the way a crashed launcher would have left it.
func seedRepairRecord(t *testing.T, sessionsDir, ref, phase, workspace string) string {
	t.Helper()
	body := map[string]any{
		"ref":       ref,
		"agent":     "claude",
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
		"version":   2,
		"phase":     phase,
		"revision":  1,
	}
	if workspace != "" {
		body["workspace"] = workspace
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.ReplaceAll(strings.ReplaceAll(ref, "/", "-"), "#", "-") + "-1.json"
	path := filepath.Join(sessionsDir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrRepair_EmptyInspectSaysSoAndExitsZero(t *testing.T) {
	out, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()))
	if err != nil {
		t.Fatalf("pr repair: %v", err)
	}
	if !strings.Contains(out, "no records need repair") {
		t.Errorf("stdout = %q, want the empty-state line", out)
	}
}

func TestPrRepairJSON_EmptyIsArrayNeverNull(t *testing.T) {
	out, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), "--json")
	if err != nil {
		t.Fatalf("pr repair --json: %v", err)
	}
	var report struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout did not parse as the report object: %v\n%s", err, out)
	}
	if report.Items == nil {
		t.Errorf("items encoded as null, want []: %s", out)
	}
}

func TestPrRepairJSON_ItemShape(t *testing.T) {
	dir := t.TempDir()
	seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	out, _, err := runPrRepair(t, repairCmdClient(t, dir), "--json")
	if err != nil {
		t.Fatalf("pr repair --json: %v", err)
	}
	var report struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want 1: %s", len(report.Items), out)
	}
	for _, k := range []string{"ref", "record_path", "from_phase", "window_live", "workspace_exists", "outcome"} {
		if _, ok := report.Items[0][k]; !ok {
			t.Errorf("item is missing key %q: %s", k, out)
		}
	}
}

func TestPrRepair_ApplyWithoutARecordRefusesNamingInspect(t *testing.T) {
	_, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), "--apply", "--rollback", "--yes")
	if err == nil {
		t.Fatal("expected a refusal: --apply needs a breadcrumb")
	}
	if !strings.Contains(err.Error(), "forgectl pr repair") {
		t.Errorf("refusal %q does not name the inspect command", err)
	}
}

func TestPrRepair_ApplyWithTwoModesRefusesNamingTheSet(t *testing.T) {
	dir := t.TempDir()
	path := seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	_, _, err := runPrRepair(t, repairCmdClient(t, dir), path, "--apply", "--rollback", "--forget-if-absent", "--yes")
	if err == nil {
		t.Fatal("expected a refusal: two modes were given")
	}
	msg := err.Error()
	for _, want := range []string{"--adopt-window", "--rollback", "--forget-if-absent", "got"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}
}

func TestPrRepair_ApplyWithNoModeRefusesNamingTheSet(t *testing.T) {
	dir := t.TempDir()
	path := seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	_, _, err := runPrRepair(t, repairCmdClient(t, dir), path, "--apply")
	if err == nil {
		t.Fatal("expected a refusal: --apply needs exactly one mode")
	}
	if !strings.Contains(err.Error(), "--adopt-window") {
		t.Errorf("refusal %q does not name the mode set", err)
	}
}

func TestPrRepairRollback_OffATTYWithoutYesRefusesAndMutatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	_, _, err := runPrRepair(t, repairCmdClient(t, dir), path, "--apply", "--rollback")
	if err == nil {
		t.Fatal("expected a refusal: a destructive verb off a TTY requires --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal %q does not name --yes", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refusal removed the record: %v", serr)
	}
}

func TestPrRepairHistory_ReturnsTheRows(t *testing.T) {
	dir := t.TempDir()
	path := seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	client := repairCmdClient(t, dir)
	if _, _, err := runPrRepair(t, client, path, "--apply", "--forget-if-absent"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	out, _, err := runPrRepair(t, client, "--history", "--json")
	if err != nil {
		t.Fatalf("pr repair --history --json: %v", err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("history did not parse as an array: %v\n%s", err, out)
	}
	if len(rows) < 2 {
		t.Errorf("history rows = %d, want the intent and its completion: %s", len(rows), out)
	}
}

var _ = exec.FakeRunner{}
