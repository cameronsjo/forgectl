package cli

// Test plan for pr_prs.go
//
// newPrPrsCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: --json emits a valid array; reviewed field true for a marked PR,
//       false otherwise; empty result → [] not null
//   [x] Happy: human table lists REPO/#/TITLE/STATE rows; reviewed row is dimmed
//       (ANSI wrap under a forced color profile), unreviewed row is plain
//   [x] Happy: per-query degradation notes land on stderr, not stdout
//
// renderPRTable / emitPRsJSON are exercised through the command above.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prSearchRow renders one gh-search-prs JSON object for slug#number.
func prSearchRow(slug string, number int) string {
	return fmt.Sprintf(`{"number":%d,"title":"title %d","url":"https://github.com/%s/pull/%d",`+
		`"author":{"login":"cameronsjo"},"updatedAt":"2026-07-09T12:00:00Z",`+
		`"isDraft":false,"state":"OPEN","repository":{"nameWithOwner":%q}}`,
		number, number, slug, number, slug)
}

// prsRunFunc serves the given JSON to every `gh search prs` call and "" to the
// rest. Because PRs dedups by ref, returning the same set to all three queries
// yields exactly that set.
func prsRunFunc(searchJSON string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "search" && args[1] == "prs" {
			return searchJSON, nil
		}
		return "", nil
	}
}

// seedReviewed marks ref reviewed at `at` in the store at path.
func seedReviewed(t *testing.T, path string, ref pr.Ref, at time.Time) {
	t.Helper()
	store := pr.LoadReviewed(path, pr.WithNow(func() time.Time { return at }))
	if err := store.Mark(ref); err != nil {
		t.Fatalf("seed reviewed %s: %v", ref, err)
	}
}

// forceColor forces a color profile so lipgloss emits ANSI to a non-TTY buffer,
// restoring the prior profile after the test.
func forceColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func TestPrsCmd_JSON_ReviewedField(t *testing.T) {
	searchJSON := "[" + prSearchRow("cameronsjo/forgectl", 42) + "," + prSearchRow("cameronsjo/forgectl", 7) + "]"
	client := pr.New(&exec.FakeRunner{RunFunc: prsRunFunc(searchJSON)})

	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	// Mark #42 reviewed at a time after its updatedAt (2026-07-09T12:00Z) → dimmed.
	seedReviewed(t, reviewedPath, pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42},
		time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC))

	cmd := newPrPrsCmdForClient(client, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("prs --json: %v", err)
	}

	var rows []prRowJSON
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("stdout not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	byRef := map[string]prRowJSON{}
	for _, r := range rows {
		byRef[r.Ref] = r
	}
	if !byRef["cameronsjo/forgectl#42"].Reviewed {
		t.Errorf("#42 should be reviewed=true")
	}
	if byRef["cameronsjo/forgectl#7"].Reviewed {
		t.Errorf("#7 should be reviewed=false")
	}
}

func TestPrsCmd_JSON_EmptyIsArray(t *testing.T) {
	client := pr.New(&exec.FakeRunner{RunFunc: prsRunFunc("[]")})
	cmd := newPrPrsCmdForClient(client, filepath.Join(t.TempDir(), "r.json"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("prs --json empty: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, "[") {
		t.Errorf("empty --json: want array, got %q", got)
	}
}

func TestPrsCmd_Table_DimsReviewedRow(t *testing.T) {
	forceColor(t)
	searchJSON := "[" + prSearchRow("cameronsjo/forgectl", 42) + "," + prSearchRow("cameronsjo/homeclaw", 7) + "]"
	client := pr.New(&exec.FakeRunner{RunFunc: prsRunFunc(searchJSON)})

	reviewedPath := filepath.Join(t.TempDir(), "pr-reviewed.json")
	seedReviewed(t, reviewedPath, pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42},
		time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC))

	cmd := newPrPrsCmdForClient(client, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("prs: %v", err)
	}

	// Locate the line for each PR and check ANSI presence.
	var forgeLine, homeLine string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "cameronsjo/forgectl") {
			forgeLine = line
		}
		if strings.Contains(line, "cameronsjo/homeclaw") {
			homeLine = line
		}
	}
	if forgeLine == "" || homeLine == "" {
		t.Fatalf("missing rows; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(forgeLine, "\x1b[") {
		t.Errorf("reviewed row (#42) should be dimmed (ANSI), got %q", forgeLine)
	}
	if strings.Contains(homeLine, "\x1b[") {
		t.Errorf("unreviewed row (#7) should be plain, got %q", homeLine)
	}
	// Count summary is a diagnostic → stderr.
	if !strings.Contains(stderr.String(), "1 reviewed") {
		t.Errorf("want '1 reviewed' in stderr summary, got %q", stderr.String())
	}
}

func TestPrsCmd_DegradationNotesOnStderr(t *testing.T) {
	// --author degrades; the other two queries return one healthy row.
	client := pr.New(&exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "search" && args[1] == "prs" {
			for _, a := range args {
				if a == "--author" {
					return "", errors.New("gh: not authenticated")
				}
			}
			return "[" + prSearchRow("cameronsjo/forgectl", 1) + "]", nil
		}
		return "", nil
	}})
	cmd := newPrPrsCmdForClient(client, filepath.Join(t.TempDir(), "r.json"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("prs (degraded): %v", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Errorf("notes must not leak to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note:") {
		t.Errorf("degradation note missing from stderr: %q", stderr.String())
	}
}
