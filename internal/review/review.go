// Package review is the ops layer for `forgectl review`: a cross-project,
// cross-kind work-inventory read surface. It aggregates open issues AND pull
// requests across every configured owner, rendered live from the tracker —
// state is referenced, never copied (no ETL, no sync, no second store). The
// only persisted view-state is the reviewed-marks file, which reuses
// internal/pr's timestamp≥activity auto-un-dim store.
//
// Tracker output (titles, labels, repository slugs) is HOSTILE INPUT: every
// row routes through the same anchored validator internal/pr uses
// (pr.RefFromParts), and rendering layers sanitize before display. Like the
// sibling ops packages, it knows nothing of Cobra.
package review

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/pr"
)

// Kind discriminates the two work-item shapes a tracker holds.
type Kind string

const (
	KindIssue Kind = "issue"
	KindPR    Kind = "pr"
)

// GitHubHost is the Host every github.com-sourced Item carries. Phase C's
// Gitea source stamps its own host, which is why Key() is host-qualified now.
const GitHubHost = "github.com"

// Item is one open work item — an issue or a PR — surfaced by a Source.
// UpdatedAt is the item's latest activity timestamp; the reviewed store
// compares its mark against it, so any newer activity auto-un-dims the row.
type Item struct {
	Kind      Kind
	Host      string
	Owner     string
	Repo      string
	Number    int
	Title     string
	Author    string
	State     string
	IsDraft   bool // PRs only
	Labels    []string
	UpdatedAt time.Time
	URL       string
}

// Slug renders the "owner/repo" form.
func (i Item) Slug() string { return i.Owner + "/" + i.Repo }

// Key is the host-qualified identity: "github.com/owner/repo#N". Issues and
// PRs share GitHub's number space per repo, so Key is unique without Kind —
// and the host prefix keeps a future Gitea item from colliding with a
// same-slug GitHub one.
func (i Item) Key() string {
	return fmt.Sprintf("%s/%s/%s#%d", i.Host, i.Owner, i.Repo, i.Number)
}

// SortItems orders items deterministically by (host, slug, number) — the same
// tiebreak-for-stability discipline sortPRs applies.
func SortItems(items []Item) {
	sort.Slice(items, func(a, b int) bool {
		if items[a].Host != items[b].Host {
			return items[a].Host < items[b].Host
		}
		sa, sb := items[a].Slug(), items[b].Slug()
		if sa != sb {
			return sa < sb
		}
		return items[a].Number < items[b].Number
	})
}

// The accepted work-ref URL form: a github.com issue or pull URL (optional
// trailing slash), FULLY anchored like every ref regex in internal/pr.
var reWorkURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/(?:issues|pull)/([0-9]+)/?$`)

// ParseWorkRef normalizes a user-typed work reference — "owner/repo#N" or a
// full github.com issue/pull URL — to the host-qualified reviewed-store key.
// Validation rides pr.RefFromParts, the one anchored validator, so mark/unmark
// input gets the identical charset and guard checks as every other ref path.
func ParseWorkRef(s string) (key string, err error) {
	s = strings.Trim(s, " \t")
	if s == "" {
		return "", fmt.Errorf("empty work reference")
	}
	if m := reWorkURL.FindStringSubmatch(s); m != nil {
		ref, err := pr.RefFromParts(m[1], m[2], m[3])
		if err != nil {
			return "", err
		}
		return GitHubHost + "/" + ref.String(), nil
	}
	if owner, rest, ok := strings.Cut(s, "/"); ok {
		if repo, num, ok := strings.Cut(rest, "#"); ok {
			ref, err := pr.RefFromParts(owner, repo, num)
			if err != nil {
				return "", err
			}
			return GitHubHost + "/" + ref.String(), nil
		}
	}
	return "", fmt.Errorf("unrecognized work reference %q (want owner/repo#N or a github.com issue/PR URL)", s)
}

// itemFromParts builds a validated Item core from hostile tracker fields,
// routing owner/repo/number through pr.RefFromParts. Callers fill the
// remaining display fields on the returned Item.
func itemFromParts(kind Kind, host, owner, repo string, number int) (Item, error) {
	ref, err := pr.RefFromParts(owner, repo, strconv.Itoa(number))
	if err != nil {
		return Item{}, err
	}
	return Item{Kind: kind, Host: host, Owner: ref.Owner, Repo: ref.Repo, Number: ref.Number}, nil
}
