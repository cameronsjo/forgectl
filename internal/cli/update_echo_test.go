package cli

// `forgectl update` never renders subprocess text raw (#778 item 5), the
// way `forgectl upgrade` does not (#777).
//
//   [x] update run: brew's stdout never reaches the transcript; a fixed line
//       points at the debug log, which gets the text
//   [x] update run: a failed brew sub-command is named from the argv this
//       binary built, with its exit status, never with brew's stderr, on the
//       transcript, the stdout summary, and the returned error
//   [x] update check: brew's outdated list is rebuilt from name and version
//       tokens; a line that is not one is counted, never shown
//   [x] any other step's output is escaped line by line
//   [x] --json keeps each step's raw output and error text (value-preserving,
//       escaped by JSONEncoder)

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	updatepkg "github.com/cameronsjo/forgectl/internal/update"
)

// brewText is what a hostile tap could get brew to print: a terminal
// control and a marker to look for.
const brewText = "==> \x1b]0;pwned\x07 BREWTEXT"

func realBrewStep(t *testing.T) []updatepkg.Step {
	t.Helper()
	for _, s := range updatepkg.DefaultSteps() {
		if s.Name == updatepkg.StepBrew {
			return []updatepkg.Step{s}
		}
	}
	t.Fatal("no brew step in the default roster")
	return nil
}

func captureDebugLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func assertNoBrewText(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, "BREWTEXT") || strings.ContainsAny(text, "\x1b\x07") {
		t.Errorf("%s renders brew's text: %q", where, text)
	}
}

func TestUpdateRun_BrewOutputGoesToTheDebugLogOnly(t *testing.T) {
	logBuf := captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return brewText, nil }}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))

	stdout, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "run", "--yes")
	if err != nil {
		t.Fatalf("update run = %v", err)
	}
	assertNoBrewText(t, "the transcript", stderr)
	assertNoBrewText(t, "the summary", stdout)
	if !strings.Contains(stderr, "debug log") {
		t.Errorf("the transcript should point at the debug log: %q", stderr)
	}
	if !strings.Contains(logBuf.String(), "BREWTEXT") {
		t.Errorf("the debug log should get brew's output: %q", logBuf.String())
	}
}

func TestUpdateRun_BrewFailureIsCategorical(t *testing.T) {
	captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "upgrade" {
			return "", &exec.CommandError{Name: "brew", Args: args, Stderr: brewText, Output: brewText, ExitCode: 1}
		}
		return "", nil
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))

	stdout, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "run", "--yes")
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode = %d, want 1 (err %v)", ExitCode(err), err)
	}
	assertNoBrewText(t, "the transcript", stderr)
	assertNoBrewText(t, "the summary", stdout)
	assertNoBrewText(t, "the returned error", err.Error())
	if !strings.Contains(stdout, "brew upgrade --formula failed (exit 1)") {
		t.Errorf("the FAIL line should name the failed command and its exit: %q", stdout)
	}
	if !strings.Contains(err.Error(), "update: brew failed") {
		t.Errorf("the returned error should name the failed step: %q", err)
	}
}

func TestUpdateCheck_BrewListIsRebuiltFromTokens(t *testing.T) {
	logBuf := captureDebugLog(t)
	out := "node\nhomebrew/core/python@3.12  (3.12.1) < 3.12.2\n" + brewText + "\n"
	fr := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))

	_, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "check")
	if err != nil {
		t.Fatalf("update check = %v", err)
	}
	for _, want := range []string{"      node\n", "      homebrew/core/python@3.12 (3.12.1) < 3.12.2\n", "1 line(s) of brew output not shown"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the transcript is missing %q: %q", want, stderr)
		}
	}
	assertNoBrewText(t, "the transcript", stderr)
	if !strings.Contains(logBuf.String(), "BREWTEXT") {
		t.Errorf("the debug log should get brew's whole output: %q", logBuf.String())
	}
}

func TestUpdateCheck_OtherStepOutputIsEscaped(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "pkg \x1b[2J 1.0", nil }}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("npm", false, nil)}))

	_, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "check")
	if err != nil {
		t.Fatalf("update check = %v", err)
	}
	if strings.ContainsRune(stderr, 0x1b) || !strings.Contains(stderr, `pkg \x1b[2J 1.0`) {
		t.Errorf("npm's output should be escaped, not raw: %q", stderr)
	}
}

func TestUpdateRun_JSONKeepsRawOutputAndError(t *testing.T) {
	captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "upgrade" {
			return "", &exec.CommandError{Name: "brew", Args: args, Stderr: "BREWERR", ExitCode: 1}
		}
		return "BREWOUT", nil
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))

	stdout, _, _ := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "run", "--yes", "--json")
	var report struct {
		Steps []struct {
			Error  string `json:"error"`
			Output string `json:"output"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || len(report.Steps) != 1 {
		t.Fatalf("stdout is not the one-step JSON report: %v %q", err, stdout)
	}
	if got, want := report.Steps[0].Error, "brew upgrade --formula: brew upgrade --formula: BREWERR"; got != want {
		t.Errorf("json error = %q, want %q (unchanged by #778)", got, want)
	}
	if report.Steps[0].Output != "BREWOUT" {
		t.Errorf("json output = %q, want brew's stdout unchanged", report.Steps[0].Output)
	}
}
