package cli

// Test plan for review.go / review_list.go
//
// newReviewCmdForSource (Classification: API handler / cobra command)
//   [x] Happy: --json emits a valid array; reviewed field true for a marked
//       key; labels [] never null; empty result → [] not null
//   [x] Happy: human table lists KIND/REPO/#/TITLE/LABELS/STATE; reviewed row
//       dimmed (ANSI under a forced color profile), unreviewed plain
//   [x] Happy: --kind/--repo filter rows; invalid --kind is an error
//   [x] Happy: source notes land on stderr, not stdout

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/review"
)

var reviewTestTime = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

// fakeReviewSource is a canned review.Source for CLI tests.
type fakeReviewSource struct {
	items []review.Item
	notes []string
	err   error
}

func (f fakeReviewSource) Name() string { return "fake" }
func (f fakeReviewSource) Items(context.Context) ([]review.Item, []string, error) {
	return f.items, f.notes, f.err
}

func reviewItem(kind review.Kind, repo string, number int, labels ...string) review.Item {
	return review.Item{
		Kind: kind, Host: review.GitHubHost, Owner: "cameronsjo", Repo: repo, Number: number,
		Title: "title", Author: "cameronsjo", State: "open", Labels: labels,
		UpdatedAt: reviewTestTime,
		URL:       "https://github.com/cameronsjo/" + repo,
	}
}

// seedReviewedKey marks key reviewed at `at` in the store at path.
func seedReviewedKey(t *testing.T, path, key string, at time.Time) {
	t.Helper()
	store := pr.LoadReviewed(path, pr.WithNow(func() time.Time { return at }))
	if err := store.MarkKey(key); err != nil {
		t.Fatalf("seed reviewed %s: %v", key, err)
	}
}

func TestReviewCmd_JSON_ReviewedAndLabels(t *testing.T) {
	src := fakeReviewSource{items: []review.Item{
		reviewItem(review.KindIssue, "forgectl", 76, "epic"),
		reviewItem(review.KindPR, "forgectl", 77),
	}}
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/forgectl#76", reviewTestTime.Add(time.Hour))

	cmd := newReviewCmdForSource(src, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review --json: %v", err)
	}

	var rows []reviewRowJSON
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	byKey := map[string]reviewRowJSON{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if !byKey["github.com/cameronsjo/forgectl#76"].Reviewed {
		t.Errorf("#76 should be reviewed=true")
	}
	if byKey["github.com/cameronsjo/forgectl#77"].Reviewed {
		t.Errorf("#77 should be reviewed=false")
	}
	// labels must be [] never null.
	if strings.Contains(stdout.String(), `"labels": null`) {
		t.Errorf("labels must serialize as [], never null:\n%s", stdout.String())
	}
}

func TestReviewCmd_JSON_EmptyIsArray(t *testing.T) {
	cmd := newReviewCmdForSource(fakeReviewSource{}, filepath.Join(t.TempDir(), "r.json"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review --json empty: %v", err)
	}
	var rows []reviewRowJSON
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("empty --json not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rows) != 0 {
		t.Errorf("empty --json: want 0 rows, got %+v", rows)
	}
	if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, "[") {
		t.Errorf("empty --json: want array (never null), got %q", got)
	}
}

func TestReviewCmd_Table_DimsReviewedRow(t *testing.T) {
	forceColor(t)
	src := fakeReviewSource{items: []review.Item{
		reviewItem(review.KindIssue, "alpha", 1, "auto:execute"),
		reviewItem(review.KindPR, "bravo", 2),
	}}
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/alpha#1", reviewTestTime.Add(time.Hour))

	cmd := newReviewCmdForSource(src, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review: %v", err)
	}

	var alphaLine, bravoLine string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "cameronsjo/alpha") {
			alphaLine = line
		}
		if strings.Contains(line, "cameronsjo/bravo") {
			bravoLine = line
		}
	}
	if alphaLine == "" || bravoLine == "" {
		t.Fatalf("missing rows; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(alphaLine, "\x1b[") {
		t.Errorf("reviewed row should be dimmed (ANSI), got %q", alphaLine)
	}
	if strings.Contains(bravoLine, "\x1b[") {
		t.Errorf("unreviewed row should be plain, got %q", bravoLine)
	}
	if !strings.Contains(alphaLine, "auto:execute") {
		t.Errorf("labels column missing auto:execute: %q", alphaLine)
	}
	if !strings.Contains(stderr.String(), "1 reviewed") {
		t.Errorf("want '1 reviewed' in stderr summary, got %q", stderr.String())
	}
}

func TestReviewCmd_KindAndRepoFilters(t *testing.T) {
	src := fakeReviewSource{items: []review.Item{
		reviewItem(review.KindIssue, "alpha", 1),
		reviewItem(review.KindPR, "alpha", 2),
		reviewItem(review.KindIssue, "bravo", 3),
	}}

	run := func(args ...string) []reviewRowJSON {
		t.Helper()
		cmd := newReviewCmdForSource(src, filepath.Join(t.TempDir(), "r.json"))
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(append(args, "--json"))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("review %v: %v", args, err)
		}
		var rows []reviewRowJSON
		if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		return rows
	}

	if rows := run("--kind", "issue"); len(rows) != 2 {
		t.Errorf("--kind issue: got %d rows, want 2", len(rows))
	}
	if rows := run("--kind", "pr"); len(rows) != 1 {
		t.Errorf("--kind pr: got %d rows, want 1", len(rows))
	}
	if rows := run("--repo", "cameronsjo/bravo"); len(rows) != 1 || rows[0].Number != 3 {
		t.Errorf("--repo filter: got %+v", rows)
	}

	cmd := newReviewCmdForSource(src, filepath.Join(t.TempDir(), "r.json"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--kind", "bogus"})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Error("invalid --kind must be an error")
	}
}

func TestResolveReviewOwners(t *testing.T) {
	var cfg config.Config
	if got := resolveReviewOwners(cfg); len(got) != 1 || got[0] != defaultReviewOwner {
		t.Errorf("absent [review] section: got %v, want [%s]", got, defaultReviewOwner)
	}
	cfg.Review.Owners = []string{"someoneelse", "cameronsjo"}
	if got := resolveReviewOwners(cfg); len(got) != 2 || got[0] != "someoneelse" {
		t.Errorf("configured owners must win: got %v", got)
	}
}

func TestReviewCmd_NotesOnStderr(t *testing.T) {
	src := fakeReviewSource{
		items: []review.Item{reviewItem(review.KindIssue, "alpha", 1)},
		notes: []string{"issues(cameronsjo): gh: rate limited"},
	}
	cmd := newReviewCmdForSource(src, filepath.Join(t.TempDir(), "r.json"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("review (degraded): %v", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Errorf("notes must not leak to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note:") {
		t.Errorf("degradation note missing from stderr: %q", stderr.String())
	}
}
