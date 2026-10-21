package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// TestPrintWorkerPosture is forgectl#1125: `launch which` shows the worker's
// posture with each field's source, a floor refusal, and the defaults notes.
func TestPrintWorkerPosture(t *testing.T) {
	var buf bytes.Buffer
	printWorkerPosture(&buf, theme.Theme{}, launch.WorkerPosture{
		Harness: "codex",
		Values: []launch.PostureValue{
			{Field: "sandbox", Value: "read-only", Source: launch.PostureFromRepo},
			{Field: "approval_policy", Value: "on-request", Source: launch.PostureFromDefault},
		},
		Notes: []string{"defaults note"},
	})
	got := buf.String()
	for _, want := range []string{"worker", "codex  sandbox read-only (repo-profile), approval_policy on-request (default)", "note: defaults note"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}

	buf.Reset()
	printWorkerPosture(&buf, theme.Theme{}, launch.WorkerPosture{Harness: "pi"})
	if !strings.Contains(buf.String(), "pi  (no posture flags)") {
		t.Errorf("pi row: %q", buf.String())
	}

	buf.Reset()
	printWorkerPosture(&buf, theme.Theme{}, launch.WorkerPosture{Harness: "claude", Refused: "bad\x1b[31m"})
	if got := buf.String(); !strings.Contains(got, "refused: ") || strings.Contains(got, "\x1b[31m") {
		t.Errorf("refusal row unescaped or missing: %q", got)
	}
}

// TestLaunchWhichWorkerJSON pins the worker object's keys and that empty
// lists encode as [] rather than null.
func TestLaunchWhichWorkerJSON(t *testing.T) {
	doc := buildLaunchWhichJSON(launch.Profile{Harness: "pi"}, "/tmp/cwd", "", launchConfigSource{Path: "/tmp/config.toml"}, nil)
	doc.Worker = buildLaunchWhichWorkerJSON(launch.WorkerPosture{Harness: "pi"})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"worker":{"harness":"pi","posture":[],"notes":[]}`) {
		t.Errorf("worker JSON: %s", raw)
	}
}

// TestResolveLaunchView_Worker: `forgectl config` carries the worker posture
// with no project matched.
func TestResolveLaunchView_Worker(t *testing.T) {
	view := resolveLaunchView(config.Config{Launch: config.LaunchConfig{Worker: config.LaunchWorker{PermissionMode: "plan"}}})
	if view.Worker == nil || len(view.Worker.Posture) != 1 || view.Worker.Posture[0].Value != "plan" || view.Worker.Posture[0].Source != launch.PostureFromWorker {
		t.Fatalf("worker view %+v", view.Worker)
	}
}

// TestWorkerSettingsInLaunchWhich is forgectl#1323: the settings file shows on
// its own line, escaped, and under --json as worker.settings.
func TestWorkerSettingsInLaunchWhich(t *testing.T) {
	var buf bytes.Buffer
	printWorkerPosture(&buf, theme.Theme{}, launch.WorkerPosture{Harness: "claude", Settings: "/cfg/w.json\x1b[31m"})
	if got := buf.String(); !strings.Contains(got, "settings: /cfg/w.json") || strings.Contains(got, "\x1b[31m") {
		t.Errorf("settings row unescaped or missing: %q", got)
	}
	raw, err := json.Marshal(buildLaunchWhichWorkerJSON(launch.WorkerPosture{Harness: "claude", Settings: "/cfg/w.json"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"settings":"/cfg/w.json"`) {
		t.Errorf("worker JSON lacks settings: %s", raw)
	}
	view := resolveLaunchView(config.Config{Launch: config.LaunchConfig{Worker: config.LaunchWorker{Settings: "/nonexistent/w.json"}}})
	if view.Worker == nil || view.Worker.Settings != "/nonexistent/w.json" || !strings.Contains(view.Worker.Refused, "[launch.worker] settings") {
		t.Fatalf("config view did not carry the file and its refusal: %+v", view.Worker)
	}
}

// TestWorkerToolsInLaunchWhich: the tools list shows as a row, escaped, and
// in the worker JSON; config carries it with its refusal.
func TestWorkerToolsInLaunchWhich(t *testing.T) {
	var buf bytes.Buffer
	printWorkerPosture(&buf, theme.Theme{}, launch.WorkerPosture{Harness: "claude", Tools: []string{"Bash", "Read\x1b[31m"}})
	if got := buf.String(); !strings.Contains(got, "tools: Bash, Read") || strings.Contains(got, "\x1b[31m") {
		t.Errorf("tools row unescaped or missing: %q", got)
	}
	raw, err := json.Marshal(buildLaunchWhichWorkerJSON(launch.WorkerPosture{Harness: "claude", Tools: []string{"Bash", "Read"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tools":["Bash","Read"]`) {
		t.Errorf("worker JSON lacks tools: %s", raw)
	}
	view := resolveLaunchView(config.Config{Launch: config.LaunchConfig{Worker: config.LaunchWorker{Tools: []string{"read"}}}})
	if view.Worker == nil || !strings.Contains(view.Worker.Refused, "[launch.worker] tools") {
		t.Fatalf("config view did not carry the list's refusal: %+v", view.Worker)
	}
}
