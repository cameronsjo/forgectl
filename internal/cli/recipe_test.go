package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func withRecipeEnv(t *testing.T, values map[string]string) {
	t.Helper()
	previous := lookupRecipeEnv
	lookupRecipeEnv = func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
	t.Cleanup(func() { lookupRecipeEnv = previous })
}

// withoutRecipeSleep keeps the receipt poll's retry loop from making the suite
// wait twenty real seconds on the not-found path.
func withoutRecipeSleep(t *testing.T) {
	t.Helper()
	previous := recipeSleep
	recipeSleep = func(_ time.Duration) {}
	t.Cleanup(func() { recipeSleep = previous })
}

// compactedRunner answers the receipt read the way a real pane does: quiet
// before /compact is submitted, showing compaction after. It must NOT return the
// marker on the pre-submission snapshot — a fixture that does defeats the very
// staleness check the receipt exists for, and makes every receipt test pass
// against a pane that never compacted.
func compactedRunner() *exec.FakeRunner {
	var submitted bool
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 2 && args[1] == "get" {
			return agentGetJSON(args[2]), nil
		}
		if len(args) >= 4 && args[1] == "prompt" && args[3] == "/compact" {
			submitted = true
			return "", nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "read" && submitted {
			return "✽ Compacting conversation… (1s)", nil
		}
		return "", nil
	}}
}

// agentGetJSON is herdr's `agent get` response shape. The pane id it reports is
// what decides self-targeting, so a fixture that omits it silently exercises the
// not-self path.
func agentGetJSON(paneID string) string {
	return `{"id":"cli:agent:get","result":{"agent":{"pane_id":"` + paneID + `","agent_status":"idle"}}}`
}

// submissionCalls drops the preflight `agent get` and the trailing `agent read`
// receipt polls, leaving the calls a test means to assert on.
func submissionCalls(calls []exec.Call) []exec.Call {
	out := make([]exec.Call, 0, len(calls))
	for _, call := range calls {
		if len(call.Args) >= 2 && call.Args[0] == "agent" && (call.Args[1] == "get" || call.Args[1] == "read") {
			continue
		}
		out = append(out, call)
	}
	return out
}

func TestResolveRecipeHerdrTarget(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
		wantErr  bool
	}{
		{
			name:     "explicit target wins",
			explicit: "reviewer",
			env:      map[string]string{herdrPaneIDEnv: "w1:p2"},
			want:     "reviewer",
		},
		{
			name: "pane id env wins over active pane env",
			env: map[string]string{
				herdrPaneIDEnv:       "w1:p2",
				herdrActivePaneIDEnv: "w1:p3",
			},
			want: "w1:p2",
		},
		{
			name: "active pane fallback",
			env:  map[string]string{herdrActivePaneIDEnv: "w1:p3"},
			want: "w1:p3",
		},
		{
			name:    "missing target fails",
			env:     map[string]string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withRecipeEnv(t, tt.env)
			got, ok := resolveRecipeHerdrTarget(tt.explicit)
			if tt.wantErr {
				if ok {
					t.Fatal("resolveRecipeHerdrTarget() = ok, want missing target")
				}
				return
			}
			if !ok {
				t.Fatal("resolveRecipeHerdrTarget() = missing target, want ok")
			}
			if got != tt.want {
				t.Fatalf("resolveRecipeHerdrTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRecipeAfkSteps pins the ordering decision, including the flag that stops
// a self-targeted run from deadlocking against itself.
func TestRecipeAfkSteps(t *testing.T) {
	tests := []struct {
		name   string
		opts   recipeAfkOptions
		isSelf bool
		want   [][]string
	}{
		{
			name: "another pane journals with --wait then compacts",
			want: [][]string{
				{"agent", "prompt", "w1:p2", "/journal", "--wait"},
				{"agent", "prompt", "w1:p2", "/compact"},
			},
		},
		{
			name:   "self target drops --wait",
			isSelf: true,
			want: [][]string{
				{"agent", "prompt", "w1:p2", "/journal"},
				{"agent", "prompt", "w1:p2", "/compact"},
			},
		},
		{
			name: "compact-only skips the journal",
			opts: recipeAfkOptions{CompactOnly: true},
			want: [][]string{
				{"agent", "prompt", "w1:p2", "/compact"},
			},
		},
		{
			name: "rename runs between journal and compact",
			opts: recipeAfkOptions{Rename: "afk-parked"},
			want: [][]string{
				{"agent", "prompt", "w1:p2", "/journal", "--wait"},
				{"agent", "prompt", "w1:p2", "/rename afk-parked", "--wait"},
				{"agent", "prompt", "w1:p2", "/compact"},
			},
		},
		{
			name:   "compact-only with rename on self",
			opts:   recipeAfkOptions{CompactOnly: true, Rename: "afk-parked"},
			isSelf: true,
			want: [][]string{
				{"agent", "prompt", "w1:p2", "/rename afk-parked"},
				{"agent", "prompt", "w1:p2", "/compact"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps := recipeAfkSteps(tt.opts, "w1:p2", tt.isSelf)
			got := make([][]string, 0, len(steps))
			for _, step := range steps {
				got = append(got, step.Args)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("recipeAfkSteps() = %#v, want %#v", got, tt.want)
			}
			for _, step := range steps {
				if step.Describe == "" {
					t.Fatalf("step %v has no description", step.Args)
				}
			}
		})
	}
}

// TestRecipeAfkUsesAgentPromptForCompact is the regression for forgectl#469:
// `type-submit` does not exist in the released herdr client.
func TestRecipeAfkUsesAgentPromptForCompact(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	withoutRecipeSleep(t)
	fake := compactedRunner()

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Target: "reviewer"}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	want := []exec.Call{
		{Name: "herdr", Args: []string{"agent", "prompt", "reviewer", "/journal", "--wait"}},
		{Name: "herdr", Args: []string{"agent", "prompt", "reviewer", "/compact"}},
	}
	if got := submissionCalls(fake.Calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
	for _, call := range fake.Calls {
		for _, arg := range call.Args {
			if arg == "type-submit" {
				t.Fatal("recipe still calls herdr agent type-submit, which the released client does not have")
			}
		}
	}
}

func TestRecipeAfkPreflightsBeforeJournal(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	preflightErr := errors.New("server_not_running")
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 2 && args[1] == "get" {
			return "", preflightErr
		}
		return "", nil
	}}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{})
	if !errors.Is(err, preflightErr) {
		t.Fatalf("runRecipeAfk() error = %v, want wrapped preflight error", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls = %#v, want only the preflight; the journal must not spend a model call", fake.Calls)
	}
}

func TestRecipeAfkSelfTargetOmitsWait(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := compactedRunner()

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	submitted := submissionCalls(fake.Calls)
	if len(submitted) == 0 {
		t.Fatalf("calls = %#v, want at least one submission to inspect", fake.Calls)
	}
	for _, call := range submitted {
		for _, arg := range call.Args {
			if arg == "--wait" {
				t.Fatalf("self-targeted call %v carries --wait, which deadlocks the caller against itself", call.Args)
			}
		}
	}
}

func TestRecipeAfkCompactOnlySkipsJournal(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := compactedRunner()

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{CompactOnly: true}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	want := []exec.Call{{Name: "herdr", Args: []string{"agent", "prompt", "w1:p2", "/compact"}}}
	if got := submissionCalls(fake.Calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestRecipeAfkRenameSubmitsSlashRename(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := compactedRunner()

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Rename: "parked-for-reset"}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	var found bool
	for _, call := range fake.Calls {
		if len(call.Args) >= 4 && call.Args[3] == "/rename parked-for-reset" {
			found = true
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want one carrying %q", fake.Calls, "/rename parked-for-reset")
	}
}

// TestRecipeAfkFailsWhenPaneNeverShowsCompaction is the receipt check: herdr
// exits 0 for any accepted request, so a zero exit is not evidence /compact ran.
func TestRecipeAfkFailsWhenPaneNeverShowsCompaction(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	withoutRecipeSleep(t)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 2 && args[1] == "read" {
			// The payload sitting unsubmitted in the input box — accepted by
			// herdr, never run by the agent.
			return "❯ /compact", nil
		}
		return "", nil
	}}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Target: "reviewer"})
	if err == nil {
		t.Fatal("runRecipeAfk() error = nil, want a missing-receipt error")
	}
	if !strings.Contains(err.Error(), "no new") {
		t.Fatalf("runRecipeAfk() error = %v, want it to name the missing marker", err)
	}
}

// TestRecipeAfkRejectsStaleCompactMarker is the regression for a receipt check
// that could not fail: a pane compacted earlier still shows "Compacted", so
// matching raw text passes without this run's /compact doing anything.
func TestRecipeAfkRejectsStaleCompactMarker(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	withoutRecipeSleep(t)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 2 && args[1] == "read" {
			// Present before submission AND after — evidence of nothing.
			return "Compacted earlier today\n❯ /compact", nil
		}
		return "", nil
	}}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Target: "reviewer"})
	if err == nil {
		t.Fatal("runRecipeAfk() error = nil, want a stale-marker rejection")
	}
	if !strings.Contains(err.Error(), "no new") {
		t.Fatalf("runRecipeAfk() error = %v, want it to reject the pre-existing marker", err)
	}
}

// TestRecipeAfkSelfTargetSkipsReceipt pins why: /compact cannot run until this
// process's turn ends, so any read taken here necessarily predates it and a
// receipt check would always fail.
func TestRecipeAfkSelfTargetSkipsReceipt(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 2 && args[1] == "get" {
			return agentGetJSON("w1:p2"), nil
		}
		return "", nil
	}}

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}
	for _, call := range fake.Calls {
		if len(call.Args) >= 2 && call.Args[1] == "read" {
			t.Fatalf("calls = %#v, want no pane read on a self-target", fake.Calls)
		}
	}
}

func TestRecipeAfkSkipReceiptStopsAfterSubmission(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := &exec.FakeRunner{}

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Target: "reviewer", SkipReceipt: true}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}
	if len(fake.Calls) == 0 {
		t.Fatal("calls = none, want at least the preflight to inspect")
	}
	for _, call := range fake.Calls {
		if len(call.Args) >= 2 && call.Args[1] == "read" {
			t.Fatalf("calls = %#v, want no receipt read under --skip-receipt", fake.Calls)
		}
	}
}

func TestRecipeAfkUsesExplicitTarget(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	withoutRecipeSleep(t)
	fake := compactedRunner()

	if err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{Target: "reviewer"}); err != nil {
		t.Fatalf("runRecipeAfk() error = %v, want nil", err)
	}

	if len(fake.Calls) == 0 {
		t.Fatal("calls = none, want at least the preflight to inspect")
	}
	for _, call := range fake.Calls {
		if len(call.Args) >= 3 && call.Args[2] != "reviewer" {
			t.Fatalf("call %v does not address the explicit target", call.Args)
		}
	}
}

func TestRecipeAfkFailsBeforeSendingWithoutTarget(t *testing.T) {
	withRecipeEnv(t, map[string]string{})
	fake := &exec.FakeRunner{}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{})
	if err == nil {
		t.Fatal("runRecipeAfk() error = nil, want target error")
	}
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode(error) = %d, want 2", ExitCode(err))
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("calls = %#v, want none", fake.Calls)
	}
}

func TestRecipeAfkRejectsMalformedTargetAndRename(t *testing.T) {
	tests := []struct {
		name string
		opts recipeAfkOptions
	}{
		{name: "leading dash target", opts: recipeAfkOptions{Target: "--output=/tmp/x"}},
		{name: "whitespace target", opts: recipeAfkOptions{Target: "w1:p2 extra"}},
		{name: "escape byte in target", opts: recipeAfkOptions{Target: "w1:p2\x1b[2J"}},
		{name: "newline rename", opts: recipeAfkOptions{Target: "w1:p2", Rename: "one\n/compact"}},
		{name: "blank rename", opts: recipeAfkOptions{Target: "w1:p2", Rename: "   "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withRecipeEnv(t, map[string]string{})
			fake := &exec.FakeRunner{}
			err := runRecipeAfk(context.Background(), fake, tt.opts)
			if err == nil {
				t.Fatal("runRecipeAfk() error = nil, want validation error")
			}
			if ExitCode(err) != 2 {
				t.Fatalf("ExitCode(error) = %d, want 2", ExitCode(err))
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("calls = %#v, want none — validation runs before any side effect", fake.Calls)
			}
		})
	}
}

// TestRecipeAfkRenameAllowlist pins the allowlist, not a denylist: herdr writes
// the payload to the target pane's pty verbatim, so a control byte or an
// end-of-paste marker in a name reaches a live terminal as input.
func TestRecipeAfkRenameAllowlist(t *testing.T) {
	rejected := []struct {
		name  string
		value string
	}{
		{"escape byte", "parked\x1b"},
		{"end-of-paste marker", "parked\x1b[201~"},
		{"interrupt", "parked\x03"},
		{"newline", "one\n/compact"},
		{"tab", "parked\tmore"},
		{"delete", "parked\x7f"},
		{"blank", "   "},
		{"over length", strings.Repeat("a", maxRecipeRenameRunes+1)},
		{"compact marker defeats the receipt", "Compacting"},
		{"compact marker embedded", "run-Compacted-now"},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateRecipeRename(tt.value); err == nil {
				t.Fatalf("validateRecipeRename(%q) = nil, want rejection", tt.value)
			}
		})
	}
	for _, ok := range []string{"parked", "parked-for-reset", "afk 2026_09_06", "a.b-c_d 1"} {
		if err := validateRecipeRename(ok); err != nil {
			t.Fatalf("validateRecipeRename(%q) = %v, want nil", ok, err)
		}
	}
}

// TestTargetIsSelfUsesOwnPaneOnly is the regression for the false positive that
// matters: HERDR_ACTIVE_PANE_ID names the FOCUSED pane, so treating it as "self"
// submits an unsequenced, unverified /compact into a stranger's live session.
func TestTargetIsSelfUsesOwnPaneOnly(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		target    string
		preflight string
		want      bool
	}{
		{
			name:   "own pane id",
			env:    map[string]string{herdrPaneIDEnv: "w1:p2"},
			target: "w1:p2",
			want:   true,
		},
		{
			// Both vars set, and the target is the FOCUSED pane rather than
			// ours. Setting only HERDR_ACTIVE_PANE_ID would return false for the
			// unrelated reason that HERDR_PANE_ID is empty, and the assertion
			// would hold even if the focused pane were treated as self — a case
			// that cannot go red proves nothing.
			name: "focused pane is NOT self",
			env: map[string]string{
				herdrPaneIDEnv:       "w1:p2",
				herdrActivePaneIDEnv: "w9:p9",
			},
			target:    "w9:p9",
			preflight: agentGetJSON("w9:p9"),
			want:      false,
		},
		{
			name:      "focused pane is not self even with no own pane set",
			env:       map[string]string{herdrActivePaneIDEnv: "w9:p9"},
			target:    "w9:p9",
			preflight: agentGetJSON("w9:p9"),
			want:      false,
		},
		{
			name:      "agent name resolving to our pane is self",
			env:       map[string]string{herdrPaneIDEnv: "w1:p2"},
			target:    "reviewer",
			preflight: agentGetJSON("w1:p2"),
			want:      true,
		},
		{
			name:      "agent name resolving elsewhere is not self",
			env:       map[string]string{herdrPaneIDEnv: "w1:p2"},
			target:    "reviewer",
			preflight: agentGetJSON("w4:p7"),
			want:      false,
		},
		{
			name:      "unparseable preflight is not self",
			env:       map[string]string{herdrPaneIDEnv: "w1:p2"},
			target:    "reviewer",
			preflight: "not json",
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withRecipeEnv(t, tt.env)
			if got := targetIsSelf(tt.target, tt.preflight); got != tt.want {
				t.Fatalf("targetIsSelf(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestRecipeAfkStopsBeforeCompactWhenJournalFails(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	journalErr := errors.New("journal failed")
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 4 && args[3] == "/journal" {
			return "", journalErr
		}
		return "", nil
	}}

	err := runRecipeAfk(context.Background(), fake, recipeAfkOptions{})
	if !errors.Is(err, journalErr) {
		t.Fatalf("runRecipeAfk() error = %v, want wrapped journal error", err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("calls = %#v, want exactly the preflight and the journal prompt", fake.Calls)
	}
	for _, call := range fake.Calls {
		if len(call.Args) >= 4 && call.Args[3] == "/compact" {
			t.Fatalf("calls = %#v, want no /compact after a failed journal", fake.Calls)
		}
	}
}

func TestRecipeCommandAliasAndAfkSubcommand(t *testing.T) {
	withRecipeEnv(t, map[string]string{herdrPaneIDEnv: "w1:p2"})
	fake := compactedRunner()
	root := newRoot(module.Deps{Runner: fake})
	root.SetArgs([]string{"r", "afk"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v, want nil", err)
	}
	if len(submissionCalls(fake.Calls)) != 2 {
		t.Fatalf("calls = %#v, want journal and compact", fake.Calls)
	}
}

func TestRecipeAfkCommandRejectsArgs(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	root.SetArgs([]string{"recipe", "afk", "extra"})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext() error = nil, want arg error")
	}
	if !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts 0 arg") {
		t.Fatalf("ExecuteContext() error = %v, want Cobra argument error", err)
	}
}
