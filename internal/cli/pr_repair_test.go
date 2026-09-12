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
	"errors"
	"github.com/cameronsjo/forgectl/internal/pr"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	// An inspect that found something to settle exits 1 in BOTH output shapes —
	// that is the question a script asks pr repair, and answering it only in the
	// human text would make --json the one caller that cannot hear the answer.
	out, _, err := runPrRepair(t, repairCmdClient(t, dir), "--json")
	if err == nil {
		t.Fatal("pr repair --json with an unsettled record should exit nonzero")
	}
	// A standalone cobra command prints usage to stdout after a RunE error, so
	// decode the first JSON value rather than the whole buffer (a test-harness
	// artifact: the real root sets SilenceUsage).
	var report struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&report); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
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

// TestPrRepair_UnreadableRecordIsReportedAndExitsNonzero is the survey verb
// answering the opposite of the truth: an unreadable record refuses every
// launch, and `pr repair` used to print "no records need repair" and exit 0.
func TestPrRepair_UnreadableRecordIsReportedAndExitsNonzero(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "o-r-9-1.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runPrRepair(t, repairCmdClient(t, dir))
	if err == nil {
		t.Fatal("an unreadable record must not exit 0")
	}
	if strings.Contains(out, "no records need repair") {
		t.Errorf("stdout = %q, want the unreadable row rather than the empty-state line", out)
	}
	if !strings.Contains(out, "unreadable") || !strings.Contains(out, filepath.Base(bad)) {
		t.Errorf("stdout = %q, want it to name the file and why it could not be read", out)
	}
}

// TestPrRepair_ForgetSettlesAnUnreadableRecord closes the loop: the row the
// report now shows has a command that removes it, so the only escape is no
// longer a manual rm that no message mentions.
func TestPrRepair_ForgetSetsAnUnreadableRecordAside(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "o-r-9-1.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := repairCmdClient(t, dir)
	// The arm is gated: it is the only one that cannot prove what it acts on.
	if _, _, err := runPrRepair(t, client, bad, "--apply", "--forget-if-absent"); err == nil {
		t.Fatal("expected a refusal off a TTY without --yes")
	}
	if _, _, err := runPrRepair(t, client, bad, "--apply", "--forget-if-absent", "--yes"); err != nil {
		t.Fatalf("set aside an unreadable record: %v", err)
	}
	// SET ASIDE, not removed: the .json name is gone (so nothing is blocked)
	// but the bytes survive under a name no enumeration sees.
	if _, serr := os.Stat(bad); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the original name is still on disk: %v", serr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(bad)+".unreadable-") {
			preserved = true
		}
	}
	if !preserved {
		t.Errorf("the record was unlinked rather than set aside; dir = %v", entries)
	}
	out, _, err := runPrRepair(t, client)
	if err != nil {
		t.Fatalf("inspect after settling should exit 0: %v", err)
	}
	if !strings.Contains(out, "no records need repair") {
		t.Errorf("stdout = %q, want the empty state once nothing is left", out)
	}
}

// --- --prune ---------------------------------------------------------------

func TestPrPrune_RefusesAlongsideApplyNamingBoth(t *testing.T) {
	_, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), "--prune", "--apply", "--rollback")
	if err == nil {
		t.Fatal("expected a refusal: --prune and --apply do different things")
	}
	for _, want := range []string{"--prune", "--apply"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

func TestPrPrune_RefusesAlongsideHistoryNamingBoth(t *testing.T) {
	_, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), "--prune", "--history")
	if err == nil {
		t.Fatal("expected a refusal: --history reads the trail, --prune rewrites it")
	}
	for _, want := range []string{"--prune", "--history"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

// TestPrPrune_RetentionFlagsRefuseWithoutPrune: a flag that silently does
// nothing is the shape where an operator believes a window was applied and it
// was not.
func TestPrPrune_RetentionFlagsRefuseWithoutPrune(t *testing.T) {
	for _, flag := range []string{"--older-than=7d", "--log-retention=7d"} {
		t.Run(flag, func(t *testing.T) {
			_, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), flag)
			if err == nil {
				t.Fatalf("expected a refusal: %s only applies to --prune", flag)
			}
			if !strings.Contains(err.Error(), "--prune") {
				t.Errorf("refusal %q does not name --prune", err)
			}
		})
	}
}

func TestPrPrune_RefusesAMalformedRetentionWindow(t *testing.T) {
	for _, args := range [][]string{
		{"--prune", "--older-than=0"},
		{"--prune", "--log-retention=-1d"},
		{"--prune", "--older-than=whenever"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), args...)
			if err == nil {
				t.Fatal("expected a refusal: the retention window is not a positive duration")
			}
		})
	}
}

func TestPrPruneJSON_ShapeIsAnObjectWithItemsAndLog(t *testing.T) {
	out, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), "--prune", "--json", "--yes")
	if err != nil {
		t.Fatalf("pr repair --prune --json: %v — a no-op sweep is an action that succeeded", err)
	}
	var report struct {
		Items []map[string]json.RawMessage `json:"items"`
		Log   map[string]json.RawMessage   `json:"log"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&report); err != nil {
		t.Fatalf("stdout did not parse as the report object: %v\n%s", err, out)
	}
	if report.Items == nil {
		t.Errorf("items encoded as null, want []: %s", out)
	}
	for _, k := range []string{"path", "dropped", "kept", "outcome"} {
		if _, ok := report.Log[k]; !ok {
			t.Errorf("log is missing key %q: %s", k, out)
		}
	}
}

// TestPrPrune_HumanRowShape covers both halves of the sweep's output: a row per
// file, and the log line, which is the only place the compaction result shows
// without --json.
func TestPrPrune_HumanRowShape(t *testing.T) {
	dir := t.TempDir()
	// Young by its name, so it is listed and kept rather than removed.
	aside := filepath.Join(dir, "o-r-1-1.json.unreadable-"+strconv.FormatInt(time.Now().UTC().Unix(), 10))
	if err := os.WriteFile(aside, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runPrRepair(t, repairCmdClient(t, dir), "--prune", "--yes")
	if err != nil {
		t.Fatalf("pr repair --prune: %v", err)
	}
	if !strings.Contains(out, filepath.Base(aside)) {
		t.Errorf("stdout = %q, want the set-aside file named", out)
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("stdout = %q, want the outcome column", out)
	}
	if !strings.Contains(out, "log\t") {
		t.Errorf("stdout = %q, want the compaction line", out)
	}
	if _, serr := os.Stat(aside); serr != nil {
		t.Errorf("a file inside the retention window was removed: %v", serr)
	}
}

func TestPrPrune_EmptySweepSaysSoAndExitsZero(t *testing.T) {
	out, _, err := runPrRepair(t, repairCmdClient(t, t.TempDir()), "--prune", "--yes")
	if err != nil {
		t.Fatalf("pr repair --prune: %v", err)
	}
	if !strings.Contains(out, "no set-aside records to prune") {
		t.Errorf("stdout = %q, want the empty-state line", out)
	}
}

// TestPrPrune_RefusesAPositionalBreadcrumb closes the last hole in the prune
// grammar. The operand reads as "sweep this one record"; the sweep is
// directory-wide and unlinks, so ignoring it silently is the shape where an
// operator believes a scope was applied and it was not — and --yes skips the
// confirm prompt that would otherwise have shown the real file count.
func TestPrPrune_RefusesAPositionalBreadcrumb(t *testing.T) {
	dir := t.TempDir()
	path := seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	aside := filepath.Join(dir, "o-r-2-1.json.unreadable-1700000000")
	if err := os.WriteFile(aside, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runPrRepair(t, repairCmdClient(t, dir), path, "--prune", "--yes")
	if err == nil {
		t.Fatal("expected a refusal: --prune takes no breadcrumb")
	}
	for _, want := range []string{"--prune", "no breadcrumb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	// And the refusal is BEFORE dispatch: nothing was swept.
	if _, serr := os.Stat(aside); serr != nil {
		t.Errorf("the sweep ran anyway and removed a set-aside file: %v", serr)
	}
}

// TestPrPrune_RefusesEachApplyModeFlag: --rollback means "remove the one record
// I named". Running a directory-wide sweep under it would be the same silent
// re-scoping, one flag further in.
func TestPrPrune_RefusesEachApplyModeFlag(t *testing.T) {
	for _, mode := range []string{"--rollback", "--adopt-window", "--forget-if-absent"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			aside := filepath.Join(dir, "o-r-2-1.json.unreadable-1700000000")
			if err := os.WriteFile(aside, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := runPrRepair(t, repairCmdClient(t, dir), "--prune", mode, "--yes")
			if err == nil {
				t.Fatalf("expected a refusal: --prune and %s cannot be combined", mode)
			}
			for _, want := range []string{"--prune", mode} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			if _, serr := os.Stat(aside); serr != nil {
				t.Errorf("the sweep ran anyway and removed a set-aside file: %v", serr)
			}
		})
	}
}

// TestPrPrune_RefusesABreadcrumbEvenWithoutYes proves the operand check does not
// ride on the confirmation gate: it is grammar, settled before any I/O.
func TestPrPrune_RefusesABreadcrumbEvenWithoutYes(t *testing.T) {
	dir := t.TempDir()
	path := seedRepairRecord(t, dir, "o/r#1", "preparing", "")
	if _, _, err := runPrRepair(t, repairCmdClient(t, dir), path, "--prune", "--dry-run"); err == nil {
		t.Fatal("expected a refusal: --prune takes no breadcrumb, even under --dry-run")
	}
}

// TestPrRepairHistory_RendersTheVerbColumn: the trail now carries teardown and
// cleanup rows beside repair's, so the human view has to say which verb removed
// the thing. A row written before the field existed renders "-" rather than
// being read as a repair — the log is hand-editable, so an absent verb is
// unknown, never a claim.
func TestPrRepairHistory_RendersTheVerbColumn(t *testing.T) {
	dir := t.TempDir()
	rows := []pr.RepairRow{
		{TS: time.Now().UTC(), ID: "a", Verb: "teardown", Outcome: "applied", Ref: "o/r#1", RecordPath: "/tmp/one.json"},
		{TS: time.Now().UTC(), ID: "b", Mode: "--rollback", Outcome: "applied", Ref: "o/r#2", RecordPath: "/tmp/two.json"},
	}
	var log bytes.Buffer
	for _, r := range rows {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		log.Write(data)
		log.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "repair.jsonl"), log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, err := runPrRepair(t, repairCmdClient(t, dir), "--history")
	if err != nil {
		t.Fatalf("pr repair --history: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("history lines = %d, want one per row:\n%s", len(lines), out)
	}
	want := [][2]string{{"teardown", "-"}, {"-", "--rollback"}}
	for i, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			t.Fatalf("line %d has %d columns, want timestamp, verb, mode, outcome, ref, path: %q", i, len(fields), line)
		}
		if fields[1] != want[i][0] || fields[2] != want[i][1] {
			t.Errorf("line %d verb/mode = %q/%q, want %q/%q", i, fields[1], fields[2], want[i][0], want[i][1])
		}
	}
}

// TestPrRepairHistory_ClampsTheVerbModeAndOutcomeColumns pins the terminal sink
// on the three columns a hand-edited log can fill with anything. The log is
// declared untrusted input, and a raw ESC[2K + CR in the verb column would let a
// row repaint itself as any verb, outcome, ref, or path in the one view that
// exists to answer "which command removed the thing".
func TestPrRepairHistory_ClampsTheVerbModeAndOutcomeColumns(t *testing.T) {
	dir := t.TempDir()
	row := pr.RepairRow{
		TS: time.Now().UTC(), ID: "a",
		Verb: "teardown\x1b[2K\rrepair", Mode: "--rollback\x1b[31m", Outcome: "applied\x07",
		Ref: "o/r#1", RecordPath: "/tmp/one.json",
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repair.jsonl"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, err := runPrRepair(t, repairCmdClient(t, dir), "--history")
	if err != nil {
		t.Fatalf("pr repair --history: %v", err)
	}
	for _, raw := range []string{"\x1b", "\r", "\x07"} {
		if strings.Contains(out, raw) {
			t.Errorf("history output carries a raw %q byte from the log:\n%q", raw, out)
		}
	}
	if !strings.Contains(out, "teardown") {
		t.Errorf("the verb's graphic text was lost in clamping:\n%q", out)
	}
}

// TestPrRepairHistory_RendersThePruneVerbAndItsDetail: prune is the only verb
// that unlinks, so the trail has to name it as its own verb rather than as a
// spelling of repair. And a compaction row's record_path is the log itself, so
// without the detail line the row says only "prune touched this file" — the
// count of what it dropped lives nowhere else in this view.
func TestPrRepairHistory_RendersThePruneVerbAndItsDetail(t *testing.T) {
	dir := t.TempDir()
	client := repairCmdClient(t, dir)
	// One old settled pair for compaction to drop, and one set-aside file past
	// its window for the sweep to unlink — so both prune row shapes land.
	aside := filepath.Join(dir, "o-r-1-1.json.unreadable-1735689601")
	if err := os.WriteFile(aside, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A settled pair old enough to be past the log window. It is written by
	// hand because the CLI cannot backdate a row, and the point of the test is
	// what compaction reports about rows it drops.
	old := time.Now().UTC().Add(-200 * 24 * time.Hour).Format(time.RFC3339Nano)
	var log bytes.Buffer
	for _, outcome := range []string{"intent", "applied"} {
		row, err := json.Marshal(pr.RepairRow{
			TS: mustParseTime(t, old), ID: "0123456789abcdef", Verb: "repair",
			Mode: "--rollback", Outcome: outcome, RecordPath: "/tmp/gone.json",
		})
		if err != nil {
			t.Fatal(err)
		}
		log.Write(row)
		log.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "repair.jsonl"), log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runPrRepair(t, client, "--prune", "--yes", "--older-than=1h", "--log-retention=1h"); err != nil {
		t.Fatalf("pr repair --prune: %v", err)
	}

	out, _, err := runPrRepair(t, client, "--history")
	if err != nil {
		t.Fatalf("pr repair --history: %v", err)
	}
	if !strings.Contains(out, "prune") {
		t.Errorf("history does not carry the prune verb:\n%s", out)
	}
	if !strings.Contains(out, "detail: dropped ") {
		t.Errorf("history does not carry the compaction summary:\n%s", out)
	}

	// And the JSON view agrees, so a script hears the same answer.
	jsonOut, _, err := runPrRepair(t, client, "--history", "--json")
	if err != nil {
		t.Fatalf("pr repair --history --json: %v", err)
	}
	var rows []struct {
		Verb   string `json:"verb"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("history did not parse: %v\n%s", err, jsonOut)
	}
	var sawVerb, sawDetail bool
	for _, r := range rows {
		if r.Verb == "prune" {
			sawVerb = true
		}
		if strings.HasPrefix(r.Detail, "dropped ") {
			sawDetail = true
		}
	}
	if !sawVerb {
		t.Errorf("no row carries verb=prune: %s", jsonOut)
	}
	if !sawDetail {
		t.Errorf("no row carries the compaction detail: %s", jsonOut)
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
