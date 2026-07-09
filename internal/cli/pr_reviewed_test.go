package cli

// Test plan for pr_reviewed.go
//
// newPrReviewedMarkCmd (Classification: API handler / cobra command)
//   [x] Happy: mark <ref> stamps the store and prints a confirmation
//   [x] Unhappy: an unresolvable ref (malformed) errors before any write
//
// newPrReviewedUnmarkCmd (Classification: API handler / cobra command)
//   [x] Happy: unmark <ref> clears a prior mark and prints a confirmation
//   [x] Boundary: unmark of a never-marked ref is a no-op that still succeeds
//
// newPrReviewedSyncCmd (Classification: API handler / cobra command)
//   [x] Happy: sync prunes marks for refs no longer in the open set
//   [x] Unhappy: sync refuses when a query degrades (partial open set), leaving
//       the store untouched

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// reviewedSearchRunner serves searchJSON to every `gh search prs` call — used
// by the sync tests, which don't need gh pr view or git.
func reviewedSearchRunner(searchJSON string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "search" && args[1] == "prs" {
			return searchJSON, nil
		}
		return "", nil
	}}
}

func TestReviewedMarkCmd_WritesStore(t *testing.T) {
	client := pr.New(&exec.FakeRunner{})
	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	cmd := newPrReviewedMarkCmd(client, reviewedPath)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"cameronsjo/forgectl#42"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !strings.Contains(stdout.String(), "marked cameronsjo/forgectl#42 reviewed") {
		t.Errorf("want confirmation message, got %q", stdout.String())
	}

	store := pr.LoadReviewed(reviewedPath)
	if _, ok := store.ReviewedAt(pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}); !ok {
		t.Errorf("store on disk missing the mark")
	}
}

func TestReviewedMarkCmd_UnresolvableRef_NoWrite(t *testing.T) {
	client := pr.New(&exec.FakeRunner{})
	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	cmd := newPrReviewedMarkCmd(client, reviewedPath)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"not a valid ref"})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("want error for an unparseable ref, got nil")
	}
	store := pr.LoadReviewed(reviewedPath)
	if _, ok := store.ReviewedAt(pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}); ok {
		t.Errorf("unresolvable ref must not have written a mark")
	}
}

func TestReviewedUnmarkCmd_ClearsPriorMark(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	seedReviewed(t, reviewedPath, ref, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))

	client := pr.New(&exec.FakeRunner{})
	cmd := newPrReviewedUnmarkCmd(client, reviewedPath)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"cameronsjo/forgectl#42"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if !strings.Contains(stdout.String(), "unmarked cameronsjo/forgectl#42") {
		t.Errorf("want confirmation message, got %q", stdout.String())
	}
	if _, ok := pr.LoadReviewed(reviewedPath).ReviewedAt(ref); ok {
		t.Errorf("store on disk still has the mark after unmark")
	}
}

func TestReviewedUnmarkCmd_NeverMarked_Succeeds(t *testing.T) {
	client := pr.New(&exec.FakeRunner{})
	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	cmd := newPrReviewedUnmarkCmd(client, reviewedPath)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"cameronsjo/forgectl#7"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unmark of never-marked ref: %v", err)
	}
	if !strings.Contains(stdout.String(), "unmarked cameronsjo/forgectl#7") {
		t.Errorf("want confirmation message even for a no-op unmark, got %q", stdout.String())
	}
}

func TestReviewedSyncCmd_PrunesClosedRefs(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	closedRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	openRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 2}
	seedReviewed(t, reviewedPath, closedRef, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	seedReviewed(t, reviewedPath, openRef, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))

	// Every `gh search prs` query returns only #2 as open.
	searchJSON := "[" + prSearchRow("cameronsjo/forgectl", 2) + "]"
	client := pr.New(reviewedSearchRunner(searchJSON))
	cmd := newPrReviewedSyncCmd(client, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(stdout.String(), "synced reviewed store against 1 open PRs") {
		t.Errorf("want sync summary, got %q", stdout.String())
	}

	store := pr.LoadReviewed(reviewedPath)
	if _, ok := store.ReviewedAt(openRef); !ok {
		t.Errorf("sync pruned the still-open ref (#2)")
	}
	if _, ok := store.ReviewedAt(closedRef); ok {
		t.Errorf("sync kept the closed ref (#1)")
	}
}

func TestReviewedSyncCmd_RefusesOnDegradedQuery(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	seedReviewed(t, reviewedPath, ref, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))

	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "search" && args[1] == "prs" {
			for _, a := range args {
				if a == "--assignee" {
					return "", errors.New("gh: not authenticated")
				}
			}
			return "[]", nil
		}
		return "", nil
	}}
	client := pr.New(fake)
	cmd := newPrReviewedSyncCmd(client, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("sync (degraded): %v", err)
	}
	if !strings.Contains(stderr.String(), "refusing to sync") {
		t.Errorf("want refusal note on stderr, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "synced reviewed store") {
		t.Errorf("must not report a sync summary when refusing: %q", stdout.String())
	}

	// The pre-existing mark must survive untouched.
	store := pr.LoadReviewed(reviewedPath)
	if _, ok := store.ReviewedAt(ref); !ok {
		t.Errorf("refused sync must not have pruned the existing mark")
	}
}
