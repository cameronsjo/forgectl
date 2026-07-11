package review

// Test plan for github.go
//
// GitHub.Items (Classification: concurrent enumeration / hostile-input parser)
//   [x] Happy: one issues + one prs query per owner, both --owner-scoped with
//       an explicit --limit; rows map to Items with labels
//   [x] Unhappy: a degraded query becomes a note, the other leg's rows survive
//   [x] Unhappy: every query failed → error; zero owners → error
//   [x] Boundary: rows == limit → truncation note
//
// parseSearchIssues (Classification: hostile-input parser)
//   [x] Unhappy: out-of-charset repo row skipped, valid rows kept
//   [x] Invariant: a /pull/ URL row is skipped (Kind stays honest even if
//       gh's issues-exclude-PRs default ever shifts)

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// issueRow renders one gh-search-issues JSON object.
func issueRow(nameWithOwner string, number int, labels ...string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, fmt.Sprintf(`{"name":%q}`, l))
	}
	return fmt.Sprintf(`{"number":%d,"title":"i%d","url":"https://github.com/%s/issues/%d",`+
		`"author":{"login":"cameronsjo"},"updatedAt":"2026-07-09T12:00:00Z",`+
		`"state":"open","labels":[%s],"repository":{"nameWithOwner":%q}}`,
		number, number, nameWithOwner, number, strings.Join(quoted, ","), nameWithOwner)
}

// prRow renders one gh-search-prs JSON object (the shared pr-package shape).
func prRow(nameWithOwner string, number int) string {
	return fmt.Sprintf(`{"number":%d,"title":"p%d","url":"https://github.com/%s/pull/%d",`+
		`"author":{"login":"cameronsjo"},"updatedAt":"2026-07-09T12:00:00Z",`+
		`"isDraft":true,"state":"OPEN","labels":[{"name":"auto:execute"}],`+
		`"repository":{"nameWithOwner":%q}}`,
		number, number, nameWithOwner, number, nameWithOwner)
}

// searchLeg reports whether a gh argv is the issues or prs search.
func searchLeg(args []string) string {
	if len(args) >= 2 && args[0] == "search" {
		return args[1]
	}
	return ""
}

func TestGitHubItems_BothLegsMapped(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch searchLeg(args) {
		case "issues":
			return "[" + issueRow("cameronsjo/forgectl", 76, "epic", "enhancement") + "]", nil
		case "prs":
			return "[" + prRow("cameronsjo/forgectl", 77) + "]", nil
		}
		return "", nil
	}}

	items, notes, err := NewGitHub(fake, []string{"cameronsjo"}).Items(context.Background())
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}

	byKey := map[string]Item{}
	for _, it := range items {
		byKey[it.Key()] = it
	}
	issue := byKey["github.com/cameronsjo/forgectl#76"]
	if issue.Kind != KindIssue || strings.Join(issue.Labels, ",") != "epic,enhancement" {
		t.Errorf("issue mapped wrong: %+v", issue)
	}
	pull := byKey["github.com/cameronsjo/forgectl#77"]
	if pull.Kind != KindPR || !pull.IsDraft || strings.Join(pull.Labels, ",") != "auto:execute" {
		t.Errorf("pr mapped wrong: %+v", pull)
	}

	// Both legs must be --owner-scoped with an explicit --limit.
	for _, call := range fake.Calls {
		argv := strings.Join(call.Args, " ")
		if !strings.Contains(argv, "--owner cameronsjo") {
			t.Errorf("query not owner-scoped: %s", argv)
		}
		if !strings.Contains(argv, "--limit 1000") {
			t.Errorf("query missing explicit --limit: %s", argv)
		}
		if strings.Contains(argv, "@me") {
			t.Errorf("inventory query must not use @me scoping: %s", argv)
		}
	}
}

func TestGitHubItems_DegradedLegBecomesNote(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch searchLeg(args) {
		case "issues":
			return "", errors.New("gh: rate limited")
		case "prs":
			return "[" + prRow("cameronsjo/forgectl", 1) + "]", nil
		}
		return "", nil
	}}

	items, notes, err := NewGitHub(fake, []string{"cameronsjo"}).Items(context.Background())
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("healthy leg's rows must survive; got %d", len(items))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "issues(cameronsjo)") {
		t.Errorf("want one issues(cameronsjo) note, got %v", notes)
	}
}

func TestGitHubItems_AllQueriesFailed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New("gh: not authenticated")
	}}
	if _, _, err := NewGitHub(fake, []string{"cameronsjo"}).Items(context.Background()); err == nil {
		t.Error("every query failing must be an error")
	}
}

func TestGitHubItems_NoOwners(t *testing.T) {
	if _, _, err := NewGitHub(&exec.FakeRunner{}, nil).Items(context.Background()); err == nil {
		t.Error("zero owners must be an error")
	}
}

func TestGitHubItems_TruncationNote(t *testing.T) {
	// Serve exactly searchLimit issue rows so the leg reports truncation.
	rows := make([]string, searchLimit)
	for i := range rows {
		rows[i] = issueRow("cameronsjo/forgectl", i+1)
	}
	issuesJSON := "[" + strings.Join(rows, ",") + "]"
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if searchLeg(args) == "issues" {
			return issuesJSON, nil
		}
		return "[]", nil
	}}

	_, notes, err := NewGitHub(fake, []string{"cameronsjo"}).Items(context.Background())
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "truncated") && strings.Contains(n, "issues(cameronsjo)") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a truncation note for the issues leg, got %v", notes)
	}
}

func TestParseSearchIssues_HostileRows(t *testing.T) {
	bad := issueRow("bad owner/repo", 1)
	pullSmuggle := strings.Replace(issueRow("cameronsjo/forgectl", 2), "/issues/2", "/pull/2", 1)
	good := issueRow("cameronsjo/forgectl", 3)

	items, rawCount, err := parseSearchIssues("[" + bad + "," + pullSmuggle + "," + good + "]")
	if err != nil {
		t.Fatalf("parseSearchIssues: %v", err)
	}
	if len(items) != 1 || items[0].Number != 3 {
		t.Errorf("want only the valid issue row (#3), got %+v", items)
	}
	// The truncation sentinel keys off the PRE-filter count: skipped hostile
	// rows must still count, or an exactly-full response with one bad row
	// would silence the cap.
	if rawCount != 3 {
		t.Errorf("rawCount = %d, want 3 (pre-filter)", rawCount)
	}
}

func TestParseSearchIssues_MalformedAndEmpty(t *testing.T) {
	if _, _, err := parseSearchIssues("{not an array"); err == nil {
		t.Error("malformed JSON: want error, got nil")
	}
	items, rawCount, err := parseSearchIssues("   ")
	if err != nil || items != nil || rawCount != 0 {
		t.Errorf("empty output: want (nil, 0, nil), got (%+v, %d, %v)", items, rawCount, err)
	}
}
