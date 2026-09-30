package pr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// swapClaudeProbe points the dispatch-time checks at probe for one test.
func swapClaudeProbe(t *testing.T, probe func(ctx context.Context, bin, dir string, args ...string) (string, error)) {
	t.Helper()
	orig := claudeProbe
	claudeProbe = probe
	t.Cleanup(func() { claudeProbe = orig })
}

// claudeAnswering is a probe for a claude that prints version and rejects
// every document when rejectAll.
func claudeAnswering(version string, rejectAll bool) func(context.Context, string, string, ...string) (string, error) {
	return func(_ context.Context, _, _ string, args ...string) (string, error) {
		return fakeClaudeAnswer(version, rejectAll, args), nil
	}
}

func TestClaudeAcceptsReviewSettings(t *testing.T) {
	const doc = `{"sandbox":{"enabled":true}}`
	doctorSays := func(out string, err error) func(context.Context, string, string, ...string) (string, error) {
		return func(_ context.Context, _, _ string, args ...string) (string, error) {
			if slices.Equal(args, []string{"--version"}) {
				return "2.1.285 (Claude Code)", nil
			}
			return out, err
		}
	}
	cases := []struct {
		name    string
		probe   func(context.Context, string, string, ...string) (string, error)
		wantErr string
	}{
		{"current claude that accepts it", claudeAnswering("2.1.285 (Claude Code)", false), ""},
		{"exactly the floor", claudeAnswering(minReviewClaudeVersion+" (Claude Code)", false), ""},
		{"a later major", claudeAnswering("3.0.0 (Claude Code)", false), ""},
		{"one patch below the floor", claudeAnswering("2.1.283 (Claude Code)", false), "older than " + minReviewClaudeVersion},
		{"an old minor", claudeAnswering("2.0.999 (Claude Code)", false), "older than " + minReviewClaudeVersion},
		{"no version printed", claudeAnswering("", false), "printed nothing"},
		{"an unparseable version", claudeAnswering("v2.1.285 (Claude Code)", false), "names no version"},
		{"--version fails", func(context.Context, string, string, ...string) (string, error) {
			return "", errors.New("exit status 127")
		}, "exit status 127"},
		{"claude rejects the document", claudeAnswering("2.1.285 (Claude Code)", true), "rejects the reviewer's settings document"},
		{"doctor cannot see a rejection", doctorSays("Claude Code doctor\n\nRunning: native (2.1.285)\n", nil), "did not flag a settings document it must reject"},
		{"doctor prints something unrecognisable", doctorSays("Usage: claude [options]\n", nil), "nothing recognisable"},
		{"doctor fails", doctorSays("", errors.New("signal: killed")), "signal: killed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapClaudeProbe(t, tc.probe)
			err := claudeAcceptsReviewSettings(context.Background(), "/usr/local/bin/claude", doc)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected refusal: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want a refusal naming %q", err, tc.wantErr)
			}
		})
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

// TestLaunchInline_ChecksTheExactDocumentItDispatches: the acceptance check
// is only worth anything if doctor validates the bytes the reviewer is given,
// passed the way the reviewer is given them, from a directory that is not
// the PR head.
func TestLaunchInline_ChecksTheExactDocumentItDispatches(t *testing.T) {
	type call struct {
		bin, dir string
		args     []string
	}
	var calls []call
	swapClaudeProbe(t, func(ctx context.Context, bin, dir string, args ...string) (string, error) {
		calls = append(calls, call{bin, dir, args})
		return healthyClaudeProbe(ctx, bin, dir, args...)
	})
	claudeBin := fakeHarnessBin(t, "claude")
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	ws := fakeWorkspace(t)
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: ws, Agent: "claude"}
	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	dispatched := fake.Last().Args
	i := slices.Index(dispatched, "--settings")
	if i < 0 || i+1 >= len(dispatched) {
		t.Fatalf("dispatched argv carries no --settings: %v", dispatched)
	}
	want := []string{"--setting-sources", "", "--settings", dispatched[i+1], "doctor"}
	checked := false
	for _, cl := range calls {
		if cl.bin != claudeBin {
			t.Errorf("checked %s, but the review runs %s", cl.bin, claudeBin)
		}
		if cl.dir == "" || strings.HasPrefix(cl.dir, ws) {
			t.Errorf("claude was probed in %q; it must run in a scratch directory, never the workspace %s", cl.dir, ws)
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
// must be refused. Like the doctor contract test, it runs when claude is on
// PATH, and FORGECTL_REQUIRE_CLAUDE_CONTRACT=1 makes a missing claude fail.
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
	swapClaudeProbe(t, runClaudeProbe)

	docs := emittedDocuments(t)
	bad := decodeInstance(t, docs["local"])
	bad["sandbox"].(map[string]any)["network"].(map[string]any)["allowMachLookup"] = false
	badDoc, err := json.Marshal(bad) // termsafe:allow-raw-json test fixture, never command output
	if err != nil {
		t.Fatal(err)
	}
	if err := claudeAcceptsReviewSettings(t.Context(), claude, string(badDoc)); err == nil || !strings.Contains(err.Error(), "allowMachLookup") {
		t.Fatalf("a document the installed claude must reject was not refused (err = %v)", err)
	}
	for name, doc := range docs {
		if err := claudeAcceptsReviewSettings(t.Context(), claude, doc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
