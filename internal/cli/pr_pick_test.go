package cli

// Test plan for pr_pick.go
//
// launchPicked (Classification: bulk-launch orchestration)
//   [x] Happy: a non-dimmed selected PR is prepared + launched (tmux new-window
//       for its pr-<N> window)
//   [x] Invariant: a reviewed (dimmed) selected PR is SKIPPED at launch — no
//       prepare, no launch — with a one-line skip note on stderr (decision 1)
//   [x] Boundary: all selections dimmed → nothing launched, explanatory note
//
// pickPRs is not unit-tested here: it drives an interactive huh multiselect
// that requires a TTY. Its selection→launch contract is covered via
// launchPicked, which is the load-bearing skip authority.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prepareRunner fakes gh pr view (valid head), git, and tmux for a Prepare +
// Launch round-trip.
func prepareRunner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		return "", nil // git clone / tmux succeed as no-ops
	}}
}

// fakeClaudeBin writes an executable stub and points FORGECTL_CLAUDE_BIN at it
// so Launch resolves a claude binary without one on PATH.
func fakeClaudeBin(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", bin)
}

func newTestCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return cmd, &out, &errOut
}

// tmuxWindows returns the window names (-n values) of every tmux new-window call.
func tmuxWindows(calls []exec.Call) []string {
	var names []string
	for _, c := range calls {
		if c.Name != "tmux" || len(c.Args) == 0 || c.Args[0] != "new-window" {
			continue
		}
		for i := 0; i+1 < len(c.Args); i++ {
			if c.Args[i] == "-n" {
				names = append(names, c.Args[i+1])
			}
		}
	}
	return names
}

func TestLaunchPicked_SkipsReviewedLaunchesRest(t *testing.T) {
	fakeClaudeBin(t)

	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	dimmed := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	seedReviewed(t, reviewedPath, dimmed, time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC))
	store := pr.LoadReviewed(reviewedPath)

	fake := prepareRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))

	updated := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	selected := []pr.PR{
		{Ref: dimmed, Title: "reviewed one", UpdatedAt: updated},                                            // dimmed → skip
		{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 7}, Title: "fresh", UpdatedAt: updated}, // launch
	}

	cmd, out, errOut := newTestCmd()
	if err := launchPicked(context.Background(), client, config.Config{}, cmd, selected, store); err != nil {
		t.Fatalf("launchPicked: %v", err)
	}

	windows := tmuxWindows(fake.Calls)
	if !containsStr(windows, "pr-7") {
		t.Errorf("non-dimmed PR #7 should have launched (window pr-7); windows=%v", windows)
	}
	if containsStr(windows, "pr-42") {
		t.Errorf("reviewed PR #42 must be skipped at launch; windows=%v", windows)
	}
	if !strings.Contains(errOut.String(), "skip cameronsjo/forgectl#42") {
		t.Errorf("want skip note for #42 on stderr, got %q", errOut.String())
	}
	if !strings.Contains(out.String(), "launched clean-room review of cameronsjo/forgectl#7") {
		t.Errorf("want launch line for #7 on stdout, got %q", out.String())
	}
	// The dimmed PR must never reach gh pr view (skipped before prepare).
	for _, c := range fake.Calls {
		if c.Name == "gh" && len(c.Args) >= 3 && c.Args[0] == "pr" && c.Args[1] == "view" && c.Args[2] == "42" {
			t.Errorf("reviewed PR #42 must not be prepared; saw gh pr view 42")
		}
	}
}

func TestLaunchPicked_AllReviewed_NothingLaunched(t *testing.T) {
	fakeClaudeBin(t)

	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	seedReviewed(t, reviewedPath, ref, time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC))
	store := pr.LoadReviewed(reviewedPath)

	fake := prepareRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))

	selected := []pr.PR{{Ref: ref, UpdatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)}}

	cmd, _, errOut := newTestCmd()
	if err := launchPicked(context.Background(), client, config.Config{}, cmd, selected, store); err != nil {
		t.Fatalf("launchPicked: %v", err)
	}
	if len(tmuxWindows(fake.Calls)) != 0 {
		t.Errorf("all-reviewed selection must launch nothing; calls=%v", fake.Calls)
	}
	if !strings.Contains(errOut.String(), "all selected PRs already reviewed") {
		t.Errorf("want all-reviewed note on stderr, got %q", errOut.String())
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
