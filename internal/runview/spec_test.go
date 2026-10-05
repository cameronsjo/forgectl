// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const exampleSpec = "testdata/pipeline.toml"

func loadExample(t *testing.T) *Spec {
	t.Helper()
	spec, err := LoadSpec(exampleSpec)
	if err != nil {
		t.Fatalf("LoadSpec(%s): %v", exampleSpec, err)
	}
	return spec
}

func TestLoadSpecParsesEverySection(t *testing.T) {
	got := loadExample(t)
	want := &Spec{
		Path:      exampleSpec,
		Name:      "pipeline",
		RootEnv:   "PIPELINE_ROOT",
		Runs:      "runs/*",
		Summaries: "results/*.summary.json",
		Log: LogSpec{
			Path: "out/events.jsonl", Time: "at", Event: "kind", Step: "stage", Attempt: "try",
			Show: []string{"stage", "try", "rc", "err"},
		},
		Steps: []StepDef{{ID: "fetch", Note: "pull the inputs"}, {ID: "build", Note: "compile and package"}},
		Gate:  GateSpec{Label: "accepted?", MaxRejects: 3},
		On: map[string]Action{
			"stage_started": ActionStart, "stage_done": ActionClose, "stage_refused": ActionReject,
			"stage_gave_up": ActionFail, "worker_boot": ActionRestart, "run_finished": ActionEnd,
		},
		End:    EndSpec{ExitField: "rc"},
		Checks: []CheckDef{{ID: "audit", Event: "audit_result", Field: "rc", Label: "audit ok?", FailExit: 3}},
		Exits:  map[int]string{0: "finished, audit passed", 3: "audit rejected"},
		Metrics: MetricsSpec{
			Path: "out/usage.jsonl", Sum: []string{"in", "out"},
			ErrorWhen: &Field{Key: "status", Value: "error"},
		},
		Live: LiveSpec{StaleAfter: 10 * time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadSpec:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadSpecWithoutOptionalTables(t *testing.T) {
	text := `name = "pipeline"
root_env = "PIPELINE_ROOT"
runs = "runs/*"
[log]
path = "out/events.jsonl"
event = "kind"
step = "stage"
[[steps]]
id = "fetch"
[on]
stage_started = "start"
`
	spec := writeAndLoad(t, text)
	if spec.Gate.MaxRejects != 0 || spec.Live.StaleAfter != 0 || spec.Metrics.ErrorWhen != nil {
		t.Errorf("absent tables decoded non-zero: gate %+v live %+v metrics %+v", spec.Gate, spec.Live, spec.Metrics)
	}
}

func writeSpec(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil { //nolint:gosec // G703: test fixtures under t.TempDir
		t.Fatal(err)
	}
	return path
}

func writeAndLoad(t *testing.T, text string) *Spec {
	t.Helper()
	spec, err := LoadSpec(writeSpec(t, text))
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	return spec
}

// TestLoadSpecErrorsNameFileAndKey edits the example one way per case and
// checks the error names the spec file and the TOML key at fault.
func TestLoadSpecErrorsNameFileAndKey(t *testing.T) {
	base, err := os.ReadFile(exampleSpec)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, old, new, key string
	}{
		{"unknown action", `stage_done = "close"`, `stage_done = "finish"`, "on.stage_done"},
		{"check is not an on action", `stage_done = "close"`, `stage_done = "check"`, "on.stage_done"},
		{"duplicate step id", `id = "build"`, `id = "fetch"`, "steps[1].id"},
		{"empty step id", `id = "build"`, `id = ""`, "steps[1].id"},
		{"after names an unknown step", `id = "build"`, "id = \"build\"\nafter = [\"deploy\"]", "steps[1].after"},
		{"after cycle", `id = "fetch"`, "id = \"fetch\"\nafter = [\"build\"]\n", "steps[0].after"},
		{"max_rejects zero", `max_rejects = 3`, `max_rejects = 0`, "gate.max_rejects"},
		{"max_rejects negative", `max_rejects = 3`, `max_rejects = -1`, "gate.max_rejects"},
		{"bad duration", `stale_after = "10m"`, `stale_after = "ten minutes"`, "live.stale_after"},
		{"negative duration", `stale_after = "10m"`, `stale_after = "-1m"`, "live.stale_after"},
		{"absolute runs", `runs = "runs/*"`, `runs = "/runs/*"`, "runs"},
		{"backslash-absolute runs", `runs = "runs/*"`, `runs = '\runs\*'`, "runs"},
		{"volume runs", `runs = "runs/*"`, `runs = 'C:runs/*'`, "runs"},
		{"dotdot runs", `runs = "runs/*"`, `runs = "runs/../*"`, "runs"},
		{"two stars runs", `runs = "runs/*"`, `runs = "runs/*/*"`, "runs"},
		{"no star runs", `runs = "runs/*"`, `runs = "runs/all"`, "runs"},
		{"glob class in runs", `runs = "runs/*"`, `runs = "runs/[ab]*"`, "runs"},
		{"absolute summaries", `summaries = "results/*.summary.json"`, `summaries = "/results/*.summary.json"`, "summaries"},
		{"two stars summaries", `summaries = "results/*.summary.json"`, `summaries = "results/*.*.json"`, "summaries"},
		{"dotdot log path", `path = "out/events.jsonl"`, `path = "../out/events.jsonl"`, "log.path"},
		{"absolute log path", `path = "out/events.jsonl"`, `path = "/out/events.jsonl"`, "log.path"},
		{"two stars log path", `path = "out/events.jsonl"`, `path = "out/*/*.jsonl"`, "log.path"},
		{"one star log path", `path = "out/events.jsonl"`, `path = "out/*.jsonl"`, "must not contain '*'"},
		{"one star metrics path", `path = "out/usage.jsonl"`, `path = "out/*.jsonl"`, "metrics.path"},
		{"colon in a log path", `path = "out/events.jsonl"`, `path = "out/events.jsonl:alt"`, "log.path: must not contain ':'"},
		{"dotdot metrics path", `path = "out/usage.jsonl"`, `path = "out/../../usage.jsonl"`, "metrics.path"},
		{"backslash dotdot metrics path", `path = "out/usage.jsonl"`, `path = 'out\..\usage.jsonl'`, "metrics.path"},
		{"exits key over 255", `"3" = "audit rejected"`, `"256" = "audit rejected"`, `exits."256"`},
		{"exits key negative", `"3" = "audit rejected"`, `"-1" = "audit rejected"`, `exits."-1"`},
		{"exits key not a number", `"3" = "audit rejected"`, `"three" = "audit rejected"`, `exits.three`},
		{"exits key not canonical", `"3" = "audit rejected"`, `"03" = "audit rejected"`, `exits."03"`},
		{"error_when two keys", `error_when = { status = "error" }`, `error_when = { status = "error", level = "bad" }`, "metrics.error_when"},
		{"error_when no keys", `error_when = { status = "error" }`, `error_when = {}`, "metrics.error_when"},
		{"fail_exit out of range", `fail_exit = 3`, `fail_exit = 300`, "checks[0].fail_exit"},
		{"check event mapped in on", `event = "audit_result"`, `event = "stage_done"`, "checks[0].event"},
		{"check without field", `field = "rc"` + "\nlabel", "label", "checks[0].field"},
		{"end without exit_field", `exit_field = "rc"`, `exit_field = ""`, "end.exit_field"},
		{"missing root_env", `root_env = "PIPELINE_ROOT"`, ``, "root_env"},
		{"missing name", `name = "pipeline"`, ``, "name"},
		{"missing log event", `event = "kind"`, ``, "log.event"},
		{"unknown key", `[live]`, "[live]\ncolour = \"red\"", "live.colour"},
		{"wrong type", `max_rejects = 3`, `max_rejects = "3"`, "gate.max_rejects"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := string(base)
			if !strings.Contains(text, c.old) {
				t.Fatalf("fixture has no %q", c.old)
			}
			text = strings.Replace(text, c.old, c.new, 1)
			path := writeSpec(t, text)
			_, err := LoadSpec(path)
			if err == nil {
				t.Fatal("LoadSpec accepted the spec")
			}
			if !errors.Is(err, ErrSpec) {
				t.Errorf("error is not ErrSpec: %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error does not name the file %s: %v", path, err)
			}
			if !strings.Contains(msg, c.key) {
				t.Errorf("error does not name the key %s: %v", c.key, err)
			}
		})
	}
}

func TestLoadSpecSyntaxErrorNamesFile(t *testing.T) {
	path := writeSpec(t, "name = \n")
	_, err := LoadSpec(path)
	if err == nil || !errors.Is(err, ErrSpec) || !strings.Contains(err.Error(), path) {
		t.Fatalf("LoadSpec = %v, want an ErrSpec naming %s", err, path)
	}
}

func TestLoadSpecMissingFileNamesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.toml")
	_, err := LoadSpec(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), path) {
		t.Fatalf("LoadSpec = %v, want a not-exist error naming %s", err, path)
	}
	if n := strings.Count(err.Error(), "absent.toml"); n != 1 {
		t.Errorf("the error names the file %d times: %v", n, err)
	}
}

// A drive letter reads as an absolute path; any other ':' is named as such.
func TestCheckPatternNamesTheColon(t *testing.T) {
	for p, want := range map[string]string{
		"C:runs/*":   "must be a relative path",
		"runs:x/*":   "must not contain ':'",
		"runs/*":     "",
		"out/a.json": "must contain exactly one '*'",
	} {
		if got := checkPattern(p, 1); got != want {
			t.Errorf("checkPattern(%q) = %q, want %q", p, got, want)
		}
	}
}
