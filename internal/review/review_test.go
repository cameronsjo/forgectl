package review

// Test plan for review.go
//
// Item.Key (Classification: identity)
//   [x] Happy: host-qualified "github.com/owner/repo#N"
//
// SortItems (Classification: deterministic ordering)
//   [x] Happy: orders by (host, slug, number)
//
// ParseWorkRef (Classification: hostile-input parser)
//   [x] Happy: owner/repo#N and both URL kinds normalize to the same key
//   [x] Unhappy: empty, malformed, out-of-charset, option-like, and
//       path-smuggling refs are rejected

import (
	"testing"
	"time"
)

func TestItemKey(t *testing.T) {
	it := Item{Kind: KindIssue, Host: GitHubHost, Owner: "cameronsjo", Repo: "forgectl", Number: 76}
	if got := it.Key(); got != "github.com/cameronsjo/forgectl#76" {
		t.Errorf("Key = %q, want github.com/cameronsjo/forgectl#76", got)
	}
}

func TestSortItems(t *testing.T) {
	items := []Item{
		{Host: GitHubHost, Owner: "cameronsjo", Repo: "zeta", Number: 1},
		{Host: GitHubHost, Owner: "cameronsjo", Repo: "alpha", Number: 9},
		{Host: GitHubHost, Owner: "cameronsjo", Repo: "alpha", Number: 2},
		{Host: "git.example.com", Owner: "cameronsjo", Repo: "alpha", Number: 5},
	}
	SortItems(items)
	want := []string{
		"git.example.com/cameronsjo/alpha#5",
		"github.com/cameronsjo/alpha#2",
		"github.com/cameronsjo/alpha#9",
		"github.com/cameronsjo/zeta#1",
	}
	for i, w := range want {
		if items[i].Key() != w {
			t.Errorf("items[%d] = %s, want %s", i, items[i].Key(), w)
		}
	}
}

func TestParseWorkRef_Forms(t *testing.T) {
	want := "github.com/cameronsjo/forgectl#76"
	for _, in := range []string{
		"cameronsjo/forgectl#76",
		"https://github.com/cameronsjo/forgectl/issues/76",
		"https://github.com/cameronsjo/forgectl/pull/76",
		"https://github.com/cameronsjo/forgectl/pull/76/",
	} {
		got, err := ParseWorkRef(in)
		if err != nil {
			t.Errorf("ParseWorkRef(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWorkRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWorkRef_Rejects(t *testing.T) {
	for _, in := range []string{
		"",
		"76",
		"cameronsjo/forgectl",
		"-owner/repo#1",
		"owner/repo#0",
		"owner/re po#1",
		"https://github.com/owner/repo/pull/1/files",
		"https://evil.com/owner/repo/pull/1",
		"owner/repo#1\n",
	} {
		if _, err := ParseWorkRef(in); err == nil {
			t.Errorf("ParseWorkRef(%q): want error, got nil", in)
		}
	}
}

// testItem builds a minimal valid Item for aggregation tests.
func testItem(kind Kind, repo string, number int) Item {
	return Item{
		Kind: kind, Host: GitHubHost, Owner: "cameronsjo", Repo: repo, Number: number,
		Title: "t", State: "open", UpdatedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
	}
}
