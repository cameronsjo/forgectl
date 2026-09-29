package pr

// Test plan for host-qualified reviewed marks (#668)
//   [x] A mark on github.com/acme/app#12 does not dim ghe.example/acme/app#12
//   [x] A legacy host-less mark dims only the default host's PR
//   [x] Mark round-trips through disk under the qualified key, migrating legacy
//   [x] Unmark clears both qualified and legacy entries
//   [x] Sync keeps default-host legacy marks for open PRs, prunes closed ones,
//       leaves hosts absent from the open set alone

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var hostTestAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func hostStore(t *testing.T, seed map[string]time.Time, opts ...ReviewedOption) (*ReviewedStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	if seed != nil {
		data, err := json.Marshal(seed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts = append([]ReviewedOption{WithNow(func() time.Time { return hostTestAt })}, opts...)
	return LoadReviewed(path, opts...), path
}

func TestReviewedStore_MarkIsHostScoped(t *testing.T) {
	store, _ := hostStore(t, nil, WithDefaultHost("github.com"))
	gh := Ref{Owner: "acme", Repo: "app", Number: 12}
	ghe := Ref{Owner: "acme", Repo: "app", Number: 12, Host: "ghe.example"}
	if err := store.Mark(gh); err != nil {
		t.Fatal(err)
	}
	if !store.IsReviewed(gh, hostTestAt) {
		t.Error("marked PR must read reviewed")
	}
	if store.IsReviewed(ghe, hostTestAt) {
		t.Error("a mark on github.com must not dim the same-numbered PR on ghe.example")
	}
	if _, ok := store.ReviewedAt(ghe); ok {
		t.Error("ReviewedAt must not see another host's mark")
	}
	// An explicit Host equal to the default is the same PR as an empty Host.
	if !store.IsReviewed(Ref{Owner: "acme", Repo: "app", Number: 12, Host: "github.com"}, hostTestAt) {
		t.Error("explicit default host must match the empty-host ref")
	}
}

func TestReviewedStore_LegacyMarkIsDefaultHostOnly(t *testing.T) {
	seed := map[string]time.Time{"acme/app#12": hostTestAt}
	store, _ := hostStore(t, seed, WithDefaultHost("ghe.example"))
	if !store.IsReviewed(Ref{Owner: "acme", Repo: "app", Number: 12}, hostTestAt) {
		t.Error("legacy mark must dim the configured host's PR")
	}
	if !store.IsReviewed(Ref{Owner: "acme", Repo: "app", Number: 12, Host: "ghe.example"}, hostTestAt) {
		t.Error("legacy mark must dim the configured host's PR when the host is explicit")
	}
	if store.IsReviewed(Ref{Owner: "acme", Repo: "app", Number: 12, Host: "github.com"}, hostTestAt) {
		t.Error("legacy mark must not dim a PR on any other host")
	}
}

func TestReviewedStore_MarkMigratesLegacyAndRoundTrips(t *testing.T) {
	old := hostTestAt.Add(-time.Hour)
	store, path := hostStore(t, map[string]time.Time{"acme/app#12": old}, WithDefaultHost("github.com"))
	ref := Ref{Owner: "acme", Repo: "app", Number: 12}
	if err := store.Mark(ref); err != nil {
		t.Fatal(err)
	}
	reloaded := LoadReviewed(path, WithDefaultHost("github.com"))
	if _, ok := reloaded.at["acme/app#12"]; ok {
		t.Error("legacy key must be dropped on write")
	}
	if got := reloaded.at["github.com/acme/app#12"]; !got.Equal(hostTestAt) {
		t.Errorf("qualified mark = %v, want %v", got, hostTestAt)
	}
	if !reloaded.IsReviewed(ref, hostTestAt) {
		t.Error("round-tripped mark must read reviewed")
	}
	if reloaded.IsReviewed(ref, hostTestAt.Add(time.Second)) {
		t.Error("later activity must auto-un-dim")
	}
}

func TestReviewedStore_UnmarkClearsLegacyAndQualified(t *testing.T) {
	seed := map[string]time.Time{"acme/app#12": hostTestAt, "github.com/acme/app#12": hostTestAt, "ghe.example/acme/app#12": hostTestAt}
	store, path := hostStore(t, seed, WithDefaultHost("github.com"))
	if err := store.Unmark(Ref{Owner: "acme", Repo: "app", Number: 12}); err != nil {
		t.Fatal(err)
	}
	reloaded := LoadReviewed(path, WithDefaultHost("github.com"))
	if len(reloaded.at) != 1 || reloaded.at["ghe.example/acme/app#12"].IsZero() {
		t.Errorf("only the other host's mark should survive, got %v", reloaded.at)
	}
}

func TestReviewedStore_SyncIsHostAware(t *testing.T) {
	seed := map[string]time.Time{
		"acme/open#1":            hostTestAt, // legacy, open
		"acme/closed#2":          hostTestAt, // legacy, closed
		"github.com/acme/cl#3":   hostTestAt, // qualified, host queried, closed
		"ghe.example/acme/app#4": hostTestAt, // host absent from the open set
	}
	store, path := hostStore(t, seed, WithDefaultHost("github.com"))
	if err := store.Sync([]Ref{{Owner: "acme", Repo: "open", Number: 1}}); err != nil {
		t.Fatal(err)
	}
	got := LoadReviewed(path).at
	if len(got) != 2 || got["acme/open#1"].IsZero() || got["ghe.example/acme/app#4"].IsZero() {
		t.Errorf("sync kept %v, want legacy open + unqueried host", got)
	}
}

func TestReviewedStore_InvalidHostNeverMarks(t *testing.T) {
	store, _ := hostStore(t, nil, WithDefaultHost("github.com"))
	bad := Ref{Owner: "acme", Repo: "app", Number: 1, Host: "evil/../x"}
	if err := store.Mark(bad); err == nil {
		t.Error("Mark with an invalid host must fail")
	}
	if store.IsReviewed(bad, hostTestAt) {
		t.Error("invalid host must never read reviewed")
	}
}
