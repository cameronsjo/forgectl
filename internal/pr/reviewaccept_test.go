package pr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

type probeFunc = func(ctx context.Context, bin, dir string, env []string, args ...string) (string, error)

// swapClaudeProbe points the dispatch-time checks at probe for one test.
func swapClaudeProbe(t *testing.T, probe probeFunc) {
	t.Helper()
	orig := claudeProbe
	claudeProbe = probe
	t.Cleanup(func() { claudeProbe = orig })
}

// claudeAnswering is a probe for a claude that prints version and rejects
// every document when rejectAll.
func claudeAnswering(version string, rejectAll bool) probeFunc {
	return func(_ context.Context, _, _ string, _ []string, args ...string) (string, error) {
		return fakeClaudeAnswer(version, rejectAll, args), nil
	}
}

// settingsDocOf is the --settings value in args, or "".
func settingsDocOf(args []string) string {
	if i := slices.Index(args, "--settings"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// realDocAnswers is a probe for a current claude that answers --version and
// the negative control correctly, and answers the doctor run over any OTHER
// document — the reviewer's real one — with out and err. Answering per call
// is the point: a fake that answers every doctor run alike lets the
// control's verdict decide the outcome, and the real-document branches go
// untested.
func realDocAnswers(out string, err error) probeFunc {
	return func(ctx context.Context, bin, dir string, env []string, args ...string) (string, error) {
		if doc := settingsDocOf(args); doc != "" && doc != doctorNegativeControl {
			return out, err
		}
		return healthyClaudeProbe(ctx, bin, dir, env, args...)
	}
}

func TestClaudeAcceptsReviewSettings(t *testing.T) {
	const doc = `{"sandbox":{"enabled":true}}`
	whole := func(body string) string { return doctorHeader + "\n\n" + body + "\n\n" + doctorFooter + "\n" }
	controlSays := func(out string, err error) probeFunc {
		return func(ctx context.Context, bin, dir string, env []string, args ...string) (string, error) {
			if settingsDocOf(args) == doctorNegativeControl {
				return out, err
			}
			return healthyClaudeProbe(ctx, bin, dir, env, args...)
		}
	}
	cases := []struct {
		name    string
		probe   probeFunc
		wantErr string
	}{
		{"current claude that accepts it", claudeAnswering("2.1.285 (Claude Code)", false), ""},
		{"exactly the floor", claudeAnswering(minReviewClaudeVersion+" (Claude Code)", false), ""},
		{"a later major", claudeAnswering("3.0.0 (Claude Code)", false), ""},
		{"a whole clean report with warnings", realDocAnswers(whole("Running: native (2.1.285)\n\n3 warnings found\n- x"), nil), ""},

		{"one patch below the floor", claudeAnswering("2.1.283 (Claude Code)", false), "older than " + minReviewClaudeVersion},
		{"an old minor", claudeAnswering("2.0.999 (Claude Code)", false), "older than " + minReviewClaudeVersion},
		{"no version printed", claudeAnswering("", false), "printed nothing"},
		{"an unparseable version", claudeAnswering("v2.1.285 (Claude Code)", false), "names no version"},
		{"--version fails", func(context.Context, string, string, []string, ...string) (string, error) {
			return "", errors.New("exit status 127")
		}, "exit status 127"},

		{"control: doctor cannot see a rejection", controlSays(whole("Running: native (2.1.285)"), nil), "did not flag a settings document it must reject"},
		{"control: doctor fails", controlSays("", errors.New("signal: killed")), "signal: killed"},
		{"control: doctor prints no header", controlSays("Usage: claude [options]\n", nil), "did not print a complete report"},

		// The control is flagged correctly in every case below; only the
		// doctor run over the reviewer's real document misbehaves.
		{"real doc: claude rejects it", claudeAnswering("2.1.285 (Claude Code)", true), "rejects the reviewer's settings document"},
		{"real doc: Invalid settings in a whole report", realDocAnswers(whole("Invalid settings\n- /tmp/x.json › permissions.allow: Expected array"), nil), "permissions.allow: Expected array"},
		{"real doc: doctor errors", realDocAnswers("", errors.New("fork/exec: no such file")), "no such file"},
		{"real doc: doctor exits non-zero after a whole report", realDocAnswers(whole("Running: native (2.1.285)"), errors.New("exit status 3")), "exit status 3"},
		{"real doc: doctor hangs past the deadline", realDocAnswers(doctorHeader+"\n", context.DeadlineExceeded), "deadline exceeded"},
		{"real doc: no header", realDocAnswers("Usage: claude [options]\n\n"+doctorFooter+"\n", nil), "did not print a complete report"},
		{"real doc: stopped before the footer", realDocAnswers(doctorHeader+"\n\nRunning: native (2.1.285)\n", nil), "did not print a complete report"},
		{"real doc: footer not last", realDocAnswers(whole("x")+"Invalid settings? no: truncated\n", nil), "did not print a complete report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapClaudeProbe(t, tc.probe)
			err := claudeAcceptsReviewSettings(context.Background(), "/usr/local/bin/claude", doc, nil)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected refusal: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want a refusal naming %q", err, tc.wantErr)
			}
		})
	}
}

func TestProbeEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "HOME=/home/o", "TMPDIR=/tmp", "HTTPS_PROXY=http://old:1",
		"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_SESSION_ID=abc", "CLAUDE_CONFIG_DIR=/home/o/.claude",
	}
	window := []string{"HTTPS_PROXY=http://proxy:3128", "GH_TOKEN=", "CLAUDE_CODE_X=from-window", "GIT_CONFIG_NOSYSTEM=1"}
	got := probeEnv(base, window, "/scratch/tmp")
	want := []string{
		"PATH=/usr/bin", "HOME=/home/o", "CLAUDE_CONFIG_DIR=/home/o/.claude",
		"HTTPS_PROXY=http://proxy:3128", "GH_TOKEN=", "GIT_CONFIG_NOSYSTEM=1",
		"TMPDIR=/scratch/tmp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("probeEnv =\n  %v\nwant\n  %v", got, want)
	}
}

// writeScript writes an owner-only executable shell script under dir.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(filepath.Clean(path), []byte("#!/bin/sh\n"+body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// G302: an executable stub needs its execute bit; 0700 is owner-only.
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // see above
		t.Fatal(err)
	}
	return path
}

// TestRunClaudeProbe_DeadlineBoundsAForkedStdoutHolder runs the production
// probe against a claude that forks a helper holding stdout and then hangs
// itself. The deadline must end the call promptly: killing only the direct
// child, or waiting on the pipe the helper still holds, is what turned a 2s
// deadline into 1m40s.
func TestRunClaudeProbe_DeadlineBoundsAForkedStdoutHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell-script stub")
	}
	orig := reviewAcceptanceTimeout
	reviewAcceptanceTimeout = time.Second
	t.Cleanup(func() { reviewAcceptanceTimeout = orig })

	bin := writeScript(t, t.TempDir(), "claude", "sleep 30 &\necho started\nsleep 30")
	start := time.Now()
	_, err := runClaudeProbe(t.Context(), bin, t.TempDir(), os.Environ())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a probe killed at its deadline must report an error")
	}
	if elapsed > 10*time.Second {
		t.Errorf("the probe took %s under a 1s deadline; a forked stdout holder kept it waiting", elapsed)
	}
}

// TestClaudeAcceptsReviewSettings_RealDocHangRefuses is the end-to-end hang
// case: a claude that answers --version and the control, then hangs on the
// reviewer's document, is refused once the deadline passes.
func TestClaudeAcceptsReviewSettings_RealDocHangRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell-script stub")
	}
	orig := reviewAcceptanceTimeout
	reviewAcceptanceTimeout = time.Second
	t.Cleanup(func() { reviewAcceptanceTimeout = orig })
	swapClaudeProbe(t, runClaudeProbe)

	bin := writeScript(t, t.TempDir(), "claude", `
if [ "$1" = --version ]; then echo '2.1.285 (Claude Code)'; exit 0; fi
case "$*" in
*not-a-boolean*) printf 'Claude Code doctor\n\nInvalid settings\n- x: bad\n\n%s\n' '`+doctorFooter+`'; exit 0;;
esac
echo 'Claude Code doctor'
sleep 30 &
sleep 30`)
	err := claudeAcceptsReviewSettings(t.Context(), bin, `{"sandbox":{"enabled":true}}`, nil)
	if err == nil || !strings.Contains(err.Error(), "could not check the reviewer's settings") {
		t.Fatalf("err = %v, want a refusal because doctor never answered for the real document", err)
	}
}

// launchAndAssertRefused dispatches a remote review and asserts it was
// refused with wantErr, before any tmux window opened.
func launchAndAssertRefused(t *testing.T, wantErr string) {
	t.Helper()
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	_, err := c.Launch(context.Background(), sess, config.Config{})
	if err == nil || !strings.Contains(err.Error(), "refusing to dispatch the Claude reviewer") || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("Launch = %v, want a dispatch refusal naming %q", err, wantErr)
	}
	assertNoReviewWindow(t, fake)
}

func assertNoReviewWindow(t *testing.T, fake *exec.FakeRunner) {
	t.Helper()
	for _, call := range fake.Calls {
		if call.Name == "tmux" && len(call.Args) > 0 && call.Args[0] == "new-window" {
			t.Errorf("a refused dispatch must open no window: %v", call.Args)
		}
	}
}

func TestLaunchInline_RefusesWhenClaudeIsOlderThanTheFloor(t *testing.T) {
	swapClaudeProbe(t, claudeAnswering("2.1.200 (Claude Code)", false))
	launchAndAssertRefused(t, "older than "+minReviewClaudeVersion)
}

func TestLaunchInline_RefusesWhenClaudeRejectsTheSettings(t *testing.T) {
	swapClaudeProbe(t, claudeAnswering("2.1.285 (Claude Code)", true))
	launchAndAssertRefused(t, "rejects the reviewer's settings document")
}

func TestLaunchInline_RefusesWhenDoctorFailsOnTheRealDocument(t *testing.T) {
	swapClaudeProbe(t, realDocAnswers("", errors.New("exit status 3")))
	launchAndAssertRefused(t, "exit status 3")
}

// TestLaunchInline_ChecksTheExactDocumentItDispatches: the acceptance check
// is only worth anything if doctor validates the bytes the reviewer is given,
// passed the way the reviewer is given them, by the binary the review runs,
// under the review window's environment, from a directory that is not the
// PR head. FORGECTL_CLAUDE_BIN is a symlink here, the shape of a native
// install, and both the check and the window must use its target.
func TestLaunchInline_ChecksTheExactDocumentItDispatches(t *testing.T) {
	type call struct {
		bin, dir string
		env      []string
		args     []string
	}
	var calls []call
	swapClaudeProbe(t, func(ctx context.Context, bin, dir string, env []string, args ...string) (string, error) {
		calls = append(calls, call{bin, dir, env, args})
		return healthyClaudeProbe(ctx, bin, dir, env, args...)
	})
	target, err := filepath.EvalSymlinks(fakeHarnessBin(t, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", link)
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "from-the-invoking-session")
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"),
		WithWindowEnv(func() ([]string, error) { return []string{"HTTPS_PROXY=http://review-proxy:3128"}, nil }))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	dispatched := fake.Last().Args
	if !slices.Contains(dispatched, target) || slices.Contains(dispatched, link) {
		t.Errorf("the review window must run the resolved binary %s, not the link %s: %v", target, link, dispatched)
	}
	want := []string{"--setting-sources", "", "--settings", settingsDocOf(dispatched), "doctor"}
	if want[3] == "" {
		t.Fatalf("dispatched argv carries no --settings: %v", dispatched)
	}
	checked := false
	for _, cl := range calls {
		if cl.bin != target {
			t.Errorf("checked %s, but the review runs %s", cl.bin, target)
		}
		if cl.dir == "" || strings.HasPrefix(cl.dir, ws) {
			t.Errorf("claude was probed in %q; it must run in a scratch directory, never the workspace %s", cl.dir, ws)
		}
		if !slices.Contains(cl.env, "HTTPS_PROXY=http://review-proxy:3128") {
			t.Errorf("the probe must run under the review window's environment; env lacks its proxy: %v", cl.env)
		}
		for _, e := range cl.env {
			if strings.HasPrefix(e, "CLAUDE_CODE_") || strings.HasPrefix(e, "CLAUDECODE=") {
				t.Errorf("the probe carries the invoking session's %s", e)
			}
		}
		if slices.Equal(cl.args, want) {
			checked = true
		}
	}
	if !checked {
		t.Errorf("no doctor run validated the dispatched document as %v; probe calls: %v", want, calls)
	}
}

// TestClaudeAcceptsReviewSettings_LiveClaude runs the production check
// against the installed claude: every document forgectl emits must pass,
// and a document with the round-2 defect (allowMachLookup as a boolean)
// must be refused. It also pins the doctor header and footer the check
// requires, since a reworded one refuses every dispatch. Like the doctor
// contract test, it runs when claude is on PATH, and
// FORGECTL_REQUIRE_CLAUDE_CONTRACT=1 makes a missing claude fail.
func TestClaudeAcceptsReviewSettings_LiveClaude(t *testing.T) {
	required := os.Getenv("FORGECTL_REQUIRE_CLAUDE_CONTRACT") == "1"
	claude, err := osexec.LookPath("claude")
	if err != nil {
		if required {
			t.Fatalf("claude is not on PATH and FORGECTL_REQUIRE_CLAUDE_CONTRACT=1")
		}
		t.Skip("claude is not on PATH; set FORGECTL_REQUIRE_CLAUDE_CONTRACT=1 to make this a failure")
	}
	if !required && testing.Short() {
		t.Skip("-short: the claude contract check starts claude")
	}
	if claude, err = filepath.EvalSymlinks(claude); err != nil {
		t.Fatal(err)
	}
	swapClaudeProbe(t, runClaudeProbe)

	tmp := t.TempDir()
	out, err := runClaudeProbe(t.Context(), claude, t.TempDir(), probeEnv(os.Environ(), nil, tmp),
		"--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "doctor")
	if err != nil {
		t.Fatalf("claude doctor: %v\n%s", err, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), doctorHeader) || lastLine(out) != doctorFooter {
		t.Fatalf("claude doctor's report no longer opens with %q and closes with %q, so every dispatch would be refused:\n%s", doctorHeader, doctorFooter, out)
	}

	docs := emittedDocuments(t)
	bad := decodeInstance(t, docs["local"])
	bad["sandbox"].(map[string]any)["network"].(map[string]any)["allowMachLookup"] = false
	badDoc, err := json.Marshal(bad) // termsafe:allow-raw-json test fixture, never command output
	if err != nil {
		t.Fatal(err)
	}
	if err := claudeAcceptsReviewSettings(t.Context(), claude, string(badDoc), nil); err == nil || !strings.Contains(err.Error(), "allowMachLookup") {
		t.Fatalf("a document the installed claude must reject was not refused (err = %v)", err)
	}
	for name, doc := range docs {
		if err := claudeAcceptsReviewSettings(t.Context(), claude, doc, nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
