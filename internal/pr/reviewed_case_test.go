package pr

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A ref typed in another case than the forge returns names the same PR:
// GitHub and Gitea owner and repo names are case-insensitive.
func TestReviewedStore_MarkIsCaseInsensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	stamp := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s := LoadReviewed(path, WithNow(func() time.Time { return stamp }))

	typed := Ref{Owner: "CameronSjo", Repo: "Forgectl", Number: 76}
	listed := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 76}
	if err := s.Mark(typed); err != nil {
		t.Fatal(err)
	}
	if !s.IsReviewed(listed, stamp.Add(-time.Hour)) {
		t.Fatal("a mark typed in another case does not dim the listed PR")
	}
	if err := s.Sync([]Ref{listed}); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadReviewed(path).ReviewedAt(listed); !ok {
		t.Error("sync with the PR still open deleted its mark")
	}
	if err := s.Unmark(listed); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadReviewed(path).ReviewedAt(typed); ok {
		t.Error("unmark in another case left the mark")
	}
}

func TestReviewedStore_KeyFunctionsAreCaseInsensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	stamp := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s := LoadReviewed(path, WithNow(func() time.Time { return stamp }))

	if err := s.MarkKey("github.com/CameronSjo/Forgectl#76"); err != nil {
		t.Fatal(err)
	}
	if !s.IsReviewedKey("github.com/cameronsjo/forgectl#76", stamp.Add(-time.Hour)) {
		t.Fatal("IsReviewedKey misses a key marked in another case")
	}
	if err := s.SyncKeysScoped([]string{"github.com/cameronsjo/forgectl#76"}, []string{"github.com"}); err != nil {
		t.Fatal(err)
	}
	if !LoadReviewed(path).IsReviewedKey("github.com/cameronsjo/forgectl#76", stamp.Add(-time.Hour)) {
		t.Error("scoped sync with the item still open deleted its mark")
	}
}

// Marks written before folding are read under the folded key, keeping the
// newest when two spellings collide.
func TestLoadReviewed_FoldsStoredKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	data := `{"github.com/CameronSjo/Forgectl#76":"2026-10-01T00:00:00Z","github.com/cameronsjo/forgectl#76":"2026-09-01T00:00:00Z","local/MyRepo#3":"2026-10-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadReviewed(path)
	at, ok := s.ReviewedAt(Ref{Host: "github.com", Owner: "cameronsjo", Repo: "forgectl", Number: 76})
	if !ok || !at.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("folded mark = %v, %v; want the newer 2026-10-01", at, ok)
	}
	if foldKey("local/MyRepo#3") != "local/MyRepo#3" {
		t.Error("a local key was folded")
	}
}
