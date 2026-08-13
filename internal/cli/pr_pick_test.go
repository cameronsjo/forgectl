package cli

// Test plan for pr_pick.go
//
// launchPicked (Classification: bulk-launch orchestration)
//   [x] Happy: a non-dimmed selected PR is prepared + launched (tmux new-window
//       for its pr-<N> window)
//   [x] Invariant: a reviewed (dimmed) selected PR is SKIPPED at launch — no
//       prepare, no launch — with a one-line skip note on stderr (decision 1)
//   [x] Boundary: all selections dimmed → nothing launched, explanatory note
//   [x] Boundary: selections exceed the concurrency cap → only the cap's worth
//       prepared/launched, the rest deferred with a one-line note (decision 2)
//   [x] Unhappy: the live tmux window count is unreadable → refuse the whole
//       batch, fail-closed — no prepare (no clone) for anything
//
// pickPRs itself remains unexecuted because it drives huh directly. The
// choosePRs boundary below covers its headless candidate path, first writer
// error, and live-TTY seam; launchPicked covers the selected-set contract.
// The esc-to-cancel keymap is likewise unassertable without running huh.

import (
	"bytes"
	"context"
	"errors"
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

func TestChoosePRs_HeadlessWritesExecutableSanitizedCandidates(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickPRsFn
	isInteractiveTTY = func() bool { return interactiveTTY(false, true) }
	pickerCalls := 0
	pickPRsFn = func([]pr.PR, *pr.ReviewedStore) ([]pr.PR, error) {
		pickerCalls++
		return nil, errors.New("picker reached")
	}
	t.Cleanup(func() { isInteractiveTTY, pickPRsFn = prevTTY, prevPicker })

	store := pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json"))
	prs := []pr.PR{{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}, Title: "safe"}, {Ref: pr.Ref{Owner: "c", Repo: "r", Number: 7}, Title: "bad\x1b[31m\n"}}
	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	_, err := choosePRs(cmd, prs, store)
	if got, want := stdout.String(), "cameronsjo/forgectl#42  safe\nc/r#7  bad [31m \n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "2 open PRs require a selection") || ExitCode(err) != 1 {
		t.Errorf("error = %v, want coded selection error", err)
	}
	if pickerCalls != 0 {
		t.Errorf("picker calls = %d, want 0", pickerCalls)
	}
}

func TestChoosePRs_HeadlessPreservesFirstWriterError(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickPRsFn
	isInteractiveTTY = func() bool { return interactiveTTY(true, false) }
	pickerCalls := 0
	pickPRsFn = func([]pr.PR, *pr.ReviewedStore) ([]pr.PR, error) { pickerCalls++; return nil, nil }
	t.Cleanup(func() { isInteractiveTTY, pickPRsFn = prevTTY, prevPicker })
	sentinel := errors.New("writer failed")
	cmd := &cobra.Command{}
	cmd.SetOut(failingWriter{err: sentinel})
	_, err := choosePRs(cmd, []pr.PR{{Ref: pr.Ref{Owner: "c", Repo: "r", Number: 1}}}, pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json")))
	if !errors.Is(err, sentinel) || err != sentinel {
		t.Errorf("error = %v, want original sentinel", err)
	}
	if pickerCalls != 0 {
		t.Errorf("picker calls = %d, want 0", pickerCalls)
	}
}

func TestChoosePRs_InteractiveCallsPickerOnceWithoutCandidateOutput(t *testing.T) {
	prevTTY, prevPicker := isInteractiveTTY, pickPRsFn
	isInteractiveTTY = func() bool { return interactiveTTY(true, true) }
	want := []pr.PR{{Ref: pr.Ref{Owner: "c", Repo: "r", Number: 1}}}
	pickerCalls := 0
	pickPRsFn = func([]pr.PR, *pr.ReviewedStore) ([]pr.PR, error) { pickerCalls++; return want, nil }
	t.Cleanup(func() { isInteractiveTTY, pickPRsFn = prevTTY, prevPicker })

	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	got, err := choosePRs(cmd, want, pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json")))
	if err != nil || len(got) != 1 || got[0].Ref != want[0].Ref {
		t.Errorf("choosePRs = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	if pickerCalls != 1 {
		t.Errorf("picker calls = %d, want 1", pickerCalls)
	}
	if stdout.Len() != 0 {
		t.Errorf("candidate stdout = %q, want empty", stdout.String())
	}
}

func TestPRCandidateLine_MarksReviewedWithoutTerminalStyling(t *testing.T) {
	ref := pr.Ref{Owner: "c", Repo: "r", Number: 1}
	store := pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json"), pr.WithNow(func() time.Time {
		return time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	}))
	if err := store.Mark(ref); err != nil {
		t.Fatal(err)
	}
	got := prCandidateLine(pr.PR{Ref: ref, Title: "done", UpdatedAt: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)}, store)
	if got != "c/r#1  done  (reviewed)" {
		t.Errorf("candidate = %q, want reviewed marker without ANSI styling", got)
	}
}

// prepareRunner fakes gh pr view (valid head), git, and tmux for a Prepare +
// Launch round-trip. has-session and list-windows report a healthy, empty
// review session (no live "pr-*" windows) — admission always finds free
// capacity — since generic tmux calls (new-window, has-session,
// list-windows) all fall through the same "", nil no-op branch below.
func prepareRunner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		if name == "tmux" && len(args) > 0 && args[0] == "has-session" {
			return "", nil // session exists
		}
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return "", nil // no live windows
		}
		return "", nil // git clone / tmux new-window succeed as no-ops
	}}
}

// unreadableWindowCountRunner fakes gh pr view + git the same as
// prepareRunner, but tmux list-windows fails with an error that is NOT "no
// server running" — a genuinely unreadable window count, which the
// admission gate must treat as fail-closed (never a free-capacity guess).
func unreadableWindowCountRunner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		if name == "tmux" && len(args) > 0 && args[0] == "has-session" {
			return "", nil // session exists
		}
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
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
	if !containsStr(windows, "pr-cameronsjo-forgectl-7") {
		t.Errorf("non-dimmed PR #7 should have launched (window pr-cameronsjo-forgectl-7); windows=%v", windows)
	}
	if containsStr(windows, "pr-cameronsjo-42") {
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

func TestLaunchPicked_CapDefersExcess(t *testing.T) {
	fakeClaudeBin(t)

	fake := prepareRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))
	store := pr.LoadReviewed(filepath.Join(t.TempDir(), "pr-reviewed.json"))

	updated := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	selected := []pr.PR{
		{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}, Title: "one", UpdatedAt: updated},
		{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 2}, Title: "two", UpdatedAt: updated},
		{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 3}, Title: "three", UpdatedAt: updated},
	}

	cmd, _, errOut := newTestCmd()
	cfg := config.Config{Pr: config.PrConfig{MaxConcurrent: 2}}
	if err := launchPicked(context.Background(), client, cfg, cmd, selected, store); err != nil {
		t.Fatalf("launchPicked: %v", err)
	}

	windows := tmuxWindows(fake.Calls)
	if len(windows) != 2 {
		t.Errorf("want exactly 2 launched windows under cap 2, got %d: %v", len(windows), windows)
	}
	if !strings.Contains(errOut.String(), "1 PR(s) deferred") {
		t.Errorf("want deferred note on stderr, got %q", errOut.String())
	}
}

func TestLaunchPicked_UnreadableWindowCount_RefusesBatch(t *testing.T) {
	fakeClaudeBin(t)

	fake := unreadableWindowCountRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))
	store := pr.LoadReviewed(filepath.Join(t.TempDir(), "pr-reviewed.json"))

	updated := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	selected := []pr.PR{
		{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}, Title: "one", UpdatedAt: updated},
	}

	cmd, _, _ := newTestCmd()
	if err := launchPicked(context.Background(), client, config.Config{}, cmd, selected, store); err == nil {
		t.Fatal("launchPicked: want error on unreadable window count, got nil")
	}

	if windows := tmuxWindows(fake.Calls); len(windows) != 0 {
		t.Errorf("unreadable count must launch nothing; windows=%v", windows)
	}
	for _, c := range fake.Calls {
		if c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "clone" {
			t.Errorf("unreadable count must refuse BEFORE any prepare/clone; saw git clone call: %+v", c)
		}
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
