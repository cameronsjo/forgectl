package cli

// `forgectl update` never renders subprocess text raw (#778 item 5), the
// way `forgectl upgrade` does not (#777).
//
//   [x] update run: brew's stdout never reaches the terminal; a fixed line
//       points at the transcript file, which holds it escaped
//   [x] update run: a failed brew sub-command is named from the argv this
//       binary built, with its exit status, never with brew's stderr, on the
//       terminal, the stdout summary, and the returned error; the cause is
//       in the transcript file, escaped, and all three point at that file
//   [x] with no transcript file, nothing points at one: the pointer falls
//       back to --json and log_level
//   [x] update check: brew's outdated list is rebuilt from name and version
//       tokens, a pinned note included; a line that is not one is counted,
//       never shown
//   [x] any other step's output is escaped line by line
//   [x] --json keeps each step's raw output and error text (value-preserving,
//       escaped by JSONEncoder)
//   [x] the transcript path is quoted with QuotePath, so a path holding a
//       space copy-pastes whole (#808)
//   [x] the path is named once by each of the run's own writes: its first
//       line on stderr, the summary's last line on stdout, and the returned
//       error (which the root handler also prints to stderr, so a human sees
//       it twice there); FAIL lines and brew's note only say "see the
//       transcript" (#808)
//   [x] a failed single-command step's stdout (CommandError.Output) is in the
//       transcript file, escaped, and a sequence's is not written twice (#808)
//   [x] --json carries that stdout too, in the step's output field, raw
//       (#810)
//   [x] the transcript file names a failed sequence command once, not
//       "brew update: brew update: …", while --json keeps that text (#808)

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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

// transcriptFile returns the one update-*.log in dir and its contents.
func transcriptFile(t *testing.T, dir string) (path, contents string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "update-*.log"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one transcript file in %s, got %v (%v)", dir, matches, err)
	}
	b, err := os.ReadFile(filepath.Clean(matches[0]))
	if err != nil {
		t.Fatal(err)
	}
	return matches[0], string(b)
}

// escapedBrewText is brewText as termsafe.SafeLine writes it.
const escapedBrewText = `==> \x1b]0;pwned\a BREWTEXT`

func TestUpdateRun_BrewOutputGoesToTheTranscriptFileOnly(t *testing.T) {
	logBuf := captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return brewText, nil }}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))
	logDir := t.TempDir()

	stdout, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: logDir}, "run", "--yes")
	if err != nil {
		t.Fatalf("update run = %v", err)
	}
	path, file := transcriptFile(t, logDir)
	assertNoBrewText(t, "the terminal transcript", stderr)
	assertNoBrewText(t, "the summary", stdout)
	if !strings.Contains(stderr, "not shown here; see the transcript)") {
		t.Errorf("the terminal should point at the transcript file: %q", stderr)
	}
	if !strings.Contains(stderr, "logging transcript to "+termsafe.QuotePath(path)+"\n") || strings.Count(stderr, path) != 1 {
		t.Errorf("stderr should name the quoted transcript path once, in its first line: %q", stderr)
	}
	if !strings.Contains(file, escapedBrewText) || strings.ContainsAny(file, "\x1b\x07") {
		t.Errorf("the transcript file should hold brew's output, escaped: %q", file)
	}
	if !strings.Contains(logBuf.String(), "BREWTEXT") {
		t.Errorf("the debug log should get brew's output too: %q", logBuf.String())
	}
}

// The #802 review's case: a failed brew step's cause must stay recoverable
// from the transcript file, while the terminal gets only the fixed wording
// and a pointer to that file.
func TestUpdateRun_BrewFailureCauseIsInTheTranscriptFile(t *testing.T) {
	captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "upgrade" {
			return "", &exec.CommandError{Name: "brew", Args: args, Stderr: "fatal: " + brewText, Output: brewText, ExitCode: 1}
		}
		return "", nil
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))
	logDir := t.TempDir()

	stdout, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: logDir}, "run", "--yes")
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode = %d, want 1 (err %v)", ExitCode(err), err)
	}
	path, file := transcriptFile(t, logDir)
	assertNoBrewText(t, "the terminal transcript", stderr)
	assertNoBrewText(t, "the summary", stdout)
	assertNoBrewText(t, "the returned error", err.Error())
	wantFail := "brew upgrade --formula failed (exit 1); see the transcript\n"
	if !strings.Contains(stdout, wantFail) || !strings.Contains(stderr, wantFail) {
		t.Errorf("the FAIL line should name the command, its exit and the transcript:\nstdout %q\nstderr %q", stdout, stderr)
	}
	quoted := termsafe.QuotePath(path)
	if !strings.HasSuffix(stdout, "\ntranscript: "+quoted+"\n") || strings.Count(stdout, path) != 1 {
		t.Errorf("the summary should end with the quoted transcript path, and name it only there: %q", stdout)
	}
	if strings.Count(stderr, path) != 1 {
		t.Errorf("stderr should name the transcript path once, in its first line: %q", stderr)
	}
	if want := "update: brew failed; see the transcript " + quoted; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if !strings.Contains(file, "      error: brew upgrade --formula: fatal: "+escapedBrewText+"\n") ||
		strings.Contains(file, "brew upgrade --formula: brew upgrade --formula:") {
		t.Errorf("the transcript file should hold the failure's cause, escaped, naming the command once: %q", file)
	}
	// runSequence already put the failed command's stdout in Output; the
	// CommandError's copy of it must not be written a second time.
	if n := strings.Count(file, escapedBrewText+"\n"); n != 2 {
		t.Errorf("want brew's stdout once and its stderr once in the file, got %d: %q", n, file)
	}
	if strings.ContainsAny(file, "\x1b\x07") {
		t.Errorf("the transcript file carries a raw control: %q", file)
	}
}

// When no transcript file can be opened, nothing may point at one: the
// pointer falls back to --json and log_level.
func TestUpdateRun_NoTranscriptFilePointsAtJSON(t *testing.T) {
	captureDebugLog(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "upgrade" {
			return "", &exec.CommandError{Name: "brew", Args: args, Stderr: brewText, ExitCode: 1}
		}
		return "", nil
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))

	stdout, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: blocker}, "run", "--yes")
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode = %d, want 1 (err %v)", ExitCode(err), err)
	}
	for where, text := range map[string]string{"stdout": stdout, "stderr": stderr, "error": err.Error()} {
		if strings.Contains(text, "see the transcript") {
			t.Errorf("%s points at a transcript that was never opened: %q", where, text)
		}
		assertNoBrewText(t, where, text)
	}
	if !strings.Contains(err.Error(), "rerun with --json") {
		t.Errorf("err = %q, want the --json fallback", err)
	}
}

func TestUpdateCheck_BrewListIsRebuiltFromTokens(t *testing.T) {
	logBuf := captureDebugLog(t)
	out := "node\nhomebrew/core/python@3.12  (3.12.1) < 3.12.2\ngo (1.22) < 1.23 [pinned at 1.22]\n" + brewText + "\n"
	fr := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
	client := updatepkg.New(fr, updatepkg.WithSteps(realBrewStep(t)))

	_, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "check")
	if err != nil {
		t.Fatalf("update check = %v", err)
	}
	for _, want := range []string{"      node\n", "      homebrew/core/python@3.12 (3.12.1) < 3.12.2\n", "      go (1.22) < 1.23 [pinned at 1.22]\n", "1 line(s) of brew output not shown"} {
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

// #808: a step that runs one command (go clean, npm update -g) returns that
// command's stdout only in the CommandError, and the transcript file must
// still hold it, escaped.
func TestUpdateRun_SingleCommandFailureOutputIsInTheTranscriptFile(t *testing.T) {
	captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", &exec.CommandError{Name: name, Args: args, Stderr: "GOERR", Output: "GOOUT \x1b[2J", ExitCode: 1}
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("go", true, nil)}))
	logDir := t.TempDir()

	_, _, err := runUpdate(t, client, config.UpdateConfig{LogDir: logDir}, "run", "--yes")
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode = %d, want 1 (err %v)", ExitCode(err), err)
	}
	_, file := transcriptFile(t, logDir)
	if !strings.Contains(file, "      | GOOUT \\x1b[2J\n") || strings.ContainsRune(file, 0x1b) {
		t.Errorf("the transcript file should hold the failed command's stdout, escaped: %q", file)
	}
	if !strings.Contains(file, "      error: go apply: GOERR\n") {
		t.Errorf("the transcript file should hold the error text: %q", file)
	}
}

// #808: the macOS default log dir sits under "Application Support". The
// pointer quotes the path, so a copy-pasted `cat <path>` gets it whole.
func TestUpdateRun_TranscriptPointerQuotesAPathWithASpace(t *testing.T) {
	captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", &exec.CommandError{Name: name, Args: args, ExitCode: 1}
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("go", true, nil)}))
	logDir := filepath.Join(t.TempDir(), "Application Support", "update-logs")

	stdout, stderr, err := runUpdate(t, client, config.UpdateConfig{LogDir: logDir}, "run", "--yes")
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode = %d, want 1 (err %v)", ExitCode(err), err)
	}
	path, _ := transcriptFile(t, logDir)
	quoted := `"` + path + `"`
	for where, text := range map[string]string{"stdout": stdout, "stderr": stderr, "error": err.Error()} {
		if !strings.Contains(text, quoted) {
			t.Errorf("%s should name the transcript as %s: %q", where, quoted, text)
		}
	}
}

// #810: the no-transcript pointer says "rerun with --json … for the details",
// so --json must carry a failed single-command step's stdout, which only its
// CommandError holds. It is value-preserving, as the rest of --json is.
//
// Mutation: drop the failedCommandOutput append from writeUpdateJSON and
// output is empty.
func TestUpdateRun_JSONCarriesAFailedSingleCommandStepsOutput(t *testing.T) {
	captureDebugLog(t)
	fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", &exec.CommandError{Name: name, Args: args, Stderr: "GOERR", Output: "GOOUT \x1b[2J", ExitCode: 1}
	}}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("go", true, nil)}))

	stdout, _, _ := runUpdate(t, client, config.UpdateConfig{LogDir: t.TempDir()}, "run", "--yes", "--json")
	var report struct {
		Steps []struct {
			Failed bool   `json:"failed"`
			Output string `json:"output"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || len(report.Steps) != 1 {
		t.Fatalf("stdout is not the one-step JSON report: %v %q", err, stdout)
	}
	if !report.Steps[0].Failed || report.Steps[0].Output != "GOOUT \x1b[2J" {
		t.Errorf("json step = %+v, want failed with the command's stdout", report.Steps[0])
	}
	if strings.ContainsRune(stdout, 0x1b) {
		t.Errorf("the JSON stream carries a raw control: %q", stdout)
	}
}
