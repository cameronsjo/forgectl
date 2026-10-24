package pr

import (
	"encoding/json"
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
	data := `{"github.com/CameronSjo/Forgectl#76":"2026-10-01T00:00:00Z","github.com/cameronsjo/forgectl#76":"2026-09-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadReviewed(path)
	at, ok := s.ReviewedAt(Ref{Host: "github.com", Owner: "cameronsjo", Repo: "forgectl", Number: 76})
	if !ok || !at.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("folded mark = %v, %v; want the newer 2026-10-01", at, ok)
	}
	if _, ok := s.ReviewedAt(Ref{Owner: localOwnerSentinel, Repo: "abc1234", Number: 3, local: true}); ok {
		t.Error("an unrelated local mark matched")
	}

	// The next write persists one folded key in place of both spellings.
	if err := s.MarkKey("github.com/other/repo#1"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]time.Time
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["github.com/CameronSjo/Forgectl#76"]; ok {
		t.Errorf("mixed-case key still on disk: %v", onDisk)
	}
	if got := onDisk["github.com/cameronsjo/forgectl#76"]; !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("folded key on disk = %v, want the newer 2026-10-01", got)
	}
}

// A Gitea host configured with capitals still scopes pruning: stored keys are
// folded, so the active host must be too.
func TestReviewedStore_SyncKeysScopedFoldsActiveHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	s := LoadReviewed(path)
	if err := s.MarkKey("Git.Example.com/o/closed#1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncKeysScoped(nil, []string{"Git.Example.com"}); err != nil {
		t.Fatal(err)
	}
	if s.IsReviewedKey("git.example.com/o/closed#1", time.Time{}) {
		t.Error("a closed item on a capitalised active host was not pruned")
	}
}
