package pr

// Test plan for discover.go
//
// parseSearchPRs (Classification: data transformer / hostile-input parser)
//   [x] Happy: valid rows → PRs with Ref from repository.nameWithOwner
//   [x] Boundary: empty output → nil, no error
//   [x] Unhappy: malformed JSON → error
//   [x] Unhappy: out-of-charset owner/repo row skipped, valid rows kept
//
// PRs (Classification: concurrent enumeration on the Inventory model)
//   [x] Happy: three queries union + dedup by Ref.String(), sorted by (slug, number)
//   [x] Unhappy: a degraded query becomes a note, not a failure
//
// PrepareMany (Classification: the load-bearing concurrency)
//   [x] Invariant: two same-repo PRs never run their git checkout concurrently
//       (per-slug active counter hooked on `git clone` never exceeds 1)
//   [x] Invariant: results align to INPUT order regardless of completion order
//   [x] Happy: per-item errors captured in PrepResult.Err, never fatal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// searchRow renders one gh-search-prs JSON object for the given slug/number.
func searchRow(nameWithOwner string, number int) string {
	return fmt.Sprintf(`{"number":%d,"title":"t%d","url":"https://github.com/%s/pull/%d",`+
		`"author":{"login":"cameronsjo"},"updatedAt":"2026-07-09T12:00:00Z",`+
		`"isDraft":false,"state":"OPEN","repository":{"nameWithOwner":%q}}`,
		number, number, nameWithOwner, number, nameWithOwner)
}

func TestParseSearchPRs(t *testing.T) {
	t.Run("valid rows parse into PRs", func(t *testing.T) {
		out := "[" + searchRow("cameronsjo/forgectl", 42) + "]"
		prs, err := parseSearchPRs(out)
		if err != nil {
			t.Fatalf("parseSearchPRs: %v", err)
		}
		if len(prs) != 1 {
			t.Fatalf("got %d PRs, want 1", len(prs))
		}
		got := prs[0]
		if got.Ref.String() != "cameronsjo/forgectl#42" {
			t.Errorf("Ref = %q, want cameronsjo/forgectl#42", got.Ref.String())
		}
		if got.State != "OPEN" || got.Author != "cameronsjo" {
			t.Errorf("unexpected PR fields: %+v", got)
		}
		want := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
		if !got.UpdatedAt.Equal(want) {
			t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want)
		}
	})

	t.Run("empty output → nil, no error", func(t *testing.T) {
		prs, err := parseSearchPRs("   ")
		if err != nil {
			t.Fatalf("parseSearchPRs(empty): %v", err)
		}
		if prs != nil {
			t.Errorf("empty output: got %+v, want nil", prs)
		}
	})

	t.Run("malformed JSON → error", func(t *testing.T) {
		if _, err := parseSearchPRs("{not an array"); err == nil {
			t.Error("malformed JSON: want error, got nil")
		}
	})

	t.Run("out-of-charset row skipped, valid kept", func(t *testing.T) {
		// A nameWithOwner with a space cannot pass splitSlug/reOwner.
		bad := searchRow("bad owner/repo", 1)
		good := searchRow("cameronsjo/forgectl", 2)
		out := "[" + bad + "," + good + "]"
		prs, err := parseSearchPRs(out)
		if err != nil {
			t.Fatalf("parseSearchPRs: %v", err)
		}
		if len(prs) != 1 || prs[0].Ref.Number != 2 {
			t.Errorf("want only the valid row (#2), got %+v", prs)
		}
	})
}

func TestPRs_UnionDedupSorted(t *testing.T) {
	// authored: forgectl#42 + homeclaw#7 ; assigned: forgectl#42 (dup) ;
	// review-requested: forgectl#5 . Union deduped = 3 rows, sorted by (slug, number).
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "gh" {
			return "", nil
		}
		who := searchWhoFlag(args)
		switch who {
		case "--author":
			return "[" + searchRow("cameronsjo/forgectl", 42) + "," + searchRow("cameronsjo/homeclaw", 7) + "]", nil
		case "--assignee":
			return "[" + searchRow("cameronsjo/forgectl", 42) + "]", nil
		case "--review-requested":
			return "[" + searchRow("cameronsjo/forgectl", 5) + "]", nil
		}
		return "[]", nil
	}}
	client := New(fake)

	prs, notes, err := client.PRs(context.Background())
	if err != nil {
		t.Fatalf("PRs: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	gotRefs := make([]string, len(prs))
	for i, p := range prs {
		gotRefs[i] = p.Ref.String()
	}
	want := []string{"cameronsjo/forgectl#5", "cameronsjo/forgectl#42", "cameronsjo/homeclaw#7"}
	if strings.Join(gotRefs, ",") != strings.Join(want, ",") {
		t.Errorf("PRs order = %v, want %v", gotRefs, want)
	}
}

func TestPRs_DegradedQueryBecomesNote(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "gh" {
			return "", nil
		}
		if searchWhoFlag(args) == "--assignee" {
			return "", errors.New("gh: not authenticated")
		}
		return "[" + searchRow("cameronsjo/forgectl", 1) + "]", nil
	}}
	client := New(fake)

	prs, notes, err := client.PRs(context.Background())
	if err != nil {
		t.Fatalf("PRs: %v", err)
	}
	if len(prs) != 1 {
		t.Errorf("degraded query must not drop the healthy rows; got %d PRs", len(prs))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "assigned") {
		t.Errorf("want one 'assigned' degradation note, got %v", notes)
	}
}

// searchWhoFlag extracts the who-scope flag (--author/--assignee/
// --review-requested) from a `gh search prs …` argv.
func searchWhoFlag(args []string) string {
	for _, a := range args {
		switch a {
		case "--author", "--assignee", "--review-requested":
			return a
		}
	}
	return ""
}

// TestPrepareMany_SameRepoSerialized proves the keyed mutex holds: two PRs from
// one repo never run their git checkout concurrently. The fake increments a
// per-slug active counter on `git clone` (Prepare uses alwaysClone=true, so the
// sandbox step is a clone), sleeps to widen the race window, and fails if the
// count ever exceeds 1 for a slug.
func TestPrepareMany_SameRepoSerialized(t *testing.T) {
	var mu sync.Mutex
	active := make(map[string]int)
	var maxSeen int

	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			// Return a valid head so Prepare proceeds to the clone step.
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		case name == "git" && len(args) >= 1 && args[0] == "clone":
			// The sandbox checkout. Track same-slug concurrency: the clone
			// target URL carries owner/repo, so derive the slug from it.
			slug := cloneSlug(args)
			mu.Lock()
			active[slug]++
			if active[slug] > maxSeen {
				maxSeen = active[slug]
			}
			if active[slug] > 1 {
				mu.Unlock()
				t.Errorf("two %s checkouts ran concurrently — keyed mutex failed", slug)
				return "", errors.New("concurrent same-repo clone")
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			active[slug]--
			mu.Unlock()
			return "", nil
		}
		return "", nil
	}}

	client := New(fake, WithSessionsDir(t.TempDir()))
	// Four PRs, all from the same repo → all must serialize.
	refs := []Ref{testRef(1), testRef(2), testRef(3), testRef(4)}
	results := client.PrepareMany(context.Background(), refs, PrepareOpts{})

	if len(results) != len(refs) {
		t.Fatalf("got %d results, want %d", len(results), len(refs))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d] (%s) unexpected err: %v", i, r.Ref, r.Err)
		}
	}
}

// cloneSlug reads owner/repo out of a `git clone … <url> <dir>` argv (the URL
// is https://github.com/<owner>/<repo>).
func cloneSlug(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "https://github.com/") {
			return strings.TrimPrefix(a, "https://github.com/")
		}
	}
	return "unknown"
}

// TestPrepareMany_InputOrder proves results[i] aligns to refs[i] regardless of
// which goroutine finishes first. Distinct repos let them run concurrently; a
// staggered per-repo sleep scrambles completion order.
func TestPrepareMany_InputOrder(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		case name == "git" && len(args) >= 1 && args[0] == "clone":
			// Make later-input repos finish sooner, scrambling completion order.
			time.Sleep(time.Duration(3) * time.Millisecond)
			return "", nil
		}
		return "", nil
	}}
	client := New(fake, WithSessionsDir(t.TempDir()))

	// Distinct slugs so they don't serialize on the keyed mutex.
	refs := []Ref{
		{Owner: "cameronsjo", Repo: "alpha", Number: 1},
		{Owner: "cameronsjo", Repo: "bravo", Number: 2},
		{Owner: "cameronsjo", Repo: "charlie", Number: 3},
	}
	results := client.PrepareMany(context.Background(), refs, PrepareOpts{})
	for i, r := range results {
		if r.Ref != refs[i] {
			t.Errorf("results[%d].Ref = %s, want %s (input order broken)", i, r.Ref, refs[i])
		}
	}
}

// TestPrepareMany_PerItemErrorCaptured proves a failing prepare lands in
// PrepResult.Err without sinking the rest of the batch.
func TestPrepareMany_PerItemErrorCaptured(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		// Fail gh pr view for #2 only.
		if name == "gh" && len(args) >= 3 && args[0] == "pr" && args[1] == "view" && args[2] == "2" {
			return "", errors.New("gh: pr not found")
		}
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		return "", nil
	}}
	client := New(fake, WithSessionsDir(t.TempDir()))

	refs := []Ref{
		{Owner: "cameronsjo", Repo: "alpha", Number: 1},
		{Owner: "cameronsjo", Repo: "bravo", Number: 2}, // this one fails
	}
	results := client.PrepareMany(context.Background(), refs, PrepareOpts{})
	if results[0].Err != nil {
		t.Errorf("results[0] should have succeeded, got err: %v", results[0].Err)
	}
	if results[1].Err == nil {
		t.Errorf("results[1] should carry the gh pr view failure, got nil")
	}
}
