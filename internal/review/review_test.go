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

// TestParseWorkRefForHosts_GHEEffectiveHost: under a configured GitHub host,
// the shorthand and the GitHub-shaped URL forms resolve to that host — and a
// literal github.com ref is REJECTED, because github.com has no active source
// under a GHE config and a key nobody re-verifies is a mark nobody can prune.
func TestParseWorkRefForHosts_GHEEffectiveHost(t *testing.T) {
	const ghe = "github.example.com"

	for in, want := range map[string]string{
		"acme/tool#7": ghe + "/acme/tool#7",
		"https://github.example.com/acme/tool/pull/7":   ghe + "/acme/tool#7",
		"https://github.example.com/acme/tool/issues/7": ghe + "/acme/tool#7",
		"github.example.com/acme/tool#7":                ghe + "/acme/tool#7",
		"https://GitHub.Example.COM/acme/tool/pull/7":   ghe + "/acme/tool#7",
	} {
		got, err := ParseWorkRefForHosts(in, ghe, nil)
		if err != nil {
			t.Errorf("ParseWorkRefForHosts(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWorkRefForHosts(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{
		"https://github.com/acme/tool/pull/7",
		"github.com/acme/tool#7",
	} {
		if got, err := ParseWorkRefForHosts(in, ghe, nil); err == nil {
			t.Errorf("ParseWorkRefForHosts(%q) = %q, want rejection under a GHE effective host", in, got)
		}
	}
}

// TestParseWorkRefForHosts_DefaultByteForByte asserts the no-config behavior
// is unchanged: with the default effective host, outcomes are identical to
// the pre-parameterization parser for accept AND reject cases.
func TestParseWorkRefForHosts_DefaultByteForByte(t *testing.T) {
	for in, want := range map[string]string{
		"cameronsjo/forgectl#76":                          "github.com/cameronsjo/forgectl#76",
		"https://github.com/cameronsjo/forgectl/pull/76":  "github.com/cameronsjo/forgectl#76",
		"https://github.com/cameronsjo/forgectl/issues/9": "github.com/cameronsjo/forgectl#9",
		"github.com/cameronsjo/forgectl#76":               "github.com/cameronsjo/forgectl#76",
	} {
		got, err := ParseWorkRefForHosts(in, GitHubHost, nil)
		if err != nil || got != want {
			t.Errorf("ParseWorkRefForHosts(%q, default) = %q, %v; want %q", in, got, err, want)
		}
	}
	// "pulls" on the default host was ALWAYS accepted — via the generic
	// any-host URL branch, whose allowlist admitted github.com — so it stays
	// accepted; byte-for-byte compatibility outranks tidying it away.
	if got, err := ParseWorkRefForHosts("https://github.com/cameronsjo/forgectl/pulls/76", GitHubHost, nil); err != nil || got != "github.com/cameronsjo/forgectl#76" {
		t.Errorf("pulls form on default host = %q, %v; want accepted (pre-change behavior)", got, err)
	}
	if _, err := ParseWorkRefForHosts("git.sjo.lol/cameron/tools#5", GitHubHost, nil); err == nil {
		t.Error("unconfigured gitea host: want rejection")
	}
}

// TestParseWorkRefForHosts_MetacharHostCannotSpoof is the finding-2 RED test:
// the effective host is interpolated through regexp.QuoteMeta, so a dotted
// host's '.' must not act as a wildcard. "githubXexampleYcom" would match a
// raw interpolation of "github.example.com".
func TestParseWorkRefForHosts_MetacharHostCannotSpoof(t *testing.T) {
	if got, err := ParseWorkRefForHosts("https://githubXexampleYcom/acme/tool/pull/7", "github.example.com", nil); err == nil {
		t.Errorf("wildcard-dot spoof accepted as %q, want rejection", got)
	}
}

// TestAllowedHostSpelling_FoldsCaseAndCanonicalizes: a mixed-case typed host
// is accepted and the minted key carries the CONFIGURED spelling.
func TestAllowedHostSpelling_FoldsCaseAndCanonicalizes(t *testing.T) {
	got, err := ParseWorkRefForHosts("Git.Sjo.LOL/cameron/tools#5", GitHubHost, []string{"git.sjo.lol"})
	if err != nil {
		t.Fatalf("mixed-case configured host rejected: %v", err)
	}
	if want := "git.sjo.lol/cameron/tools#5"; got != want {
		t.Errorf("key = %q, want the configured spelling %q", got, want)
	}
}
