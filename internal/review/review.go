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

// reGitHubSlug matches the unqualified default-host slug form: owner/repo#N
// (exactly two slash-delimited segments before the '#').
var reGitHubSlug = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)

// reHostSlug matches a host-qualified slug form: host/owner/repo#N (three
// segments before the '#'). Distinct from reGitHubSlug by segment count, so
// the two never collide on the same input.
var reHostSlug = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)

// reHostWorkURL matches a fully-qualified issue/pull(s) URL against ANY host
// — github.com's "pull" and Gitea's "pulls" are both accepted by the regex;
// the allowlist check happens after extraction, in ParseWorkRefForHosts, not
// in the pattern itself.
var reHostWorkURL = regexp.MustCompile(`^https://([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/(?:issues|pulls?)/([0-9]+)/?$`)

// ParseWorkRef normalizes a user-typed work reference for the default host,
// github.com — "owner/repo#N" or a full github.com issue/pull URL — to the
// host-qualified reviewed-store key. It is ParseWorkRefForHosts with no
// extra hosts configured; see that function for the host-qualified form
// multi-source setups (Gitea) need.
func ParseWorkRef(s string) (key string, err error) {
	return ParseWorkRefForHosts(s, nil)
}

// ParseWorkRefForHosts normalizes a user-typed work reference the same way
// ParseWorkRef does, plus two host-qualified forms — "host/owner/repo#N" and
// a full issue/pull(s) URL against a non-github host — gated on hosts, the
// caller-supplied allowlist of configured non-github hosts (mark/unmark pass
// their configured Gitea host through here). github.com is always accepted
// regardless of hosts. Every extracted owner/repo/number still rides
// pr.RefFromParts, the one anchored validator every ref path shares.
func ParseWorkRefForHosts(s string, hosts []string) (key string, err error) {
	s = strings.Trim(s, " \t")
	if s == "" {
		return "", fmt.Errorf("empty work reference")
	}
	if m := reWorkURL.FindStringSubmatch(s); m != nil {
		return workKey(GitHubHost, m[1], m[2], m[3])
	}
	if m := reGitHubSlug.FindStringSubmatch(s); m != nil {
		return workKey(GitHubHost, m[1], m[2], m[3])
	}
	if m := reHostWorkURL.FindStringSubmatch(s); m != nil {
		if !hostAllowed(m[1], hosts) {
			return "", fmt.Errorf("work reference host %q is not configured", m[1])
		}
		return workKey(m[1], m[2], m[3], m[4])
	}
	if m := reHostSlug.FindStringSubmatch(s); m != nil {
		if !hostAllowed(m[1], hosts) {
			return "", fmt.Errorf("work reference host %q is not configured", m[1])
		}
		return workKey(m[1], m[2], m[3], m[4])
	}
	return "", fmt.Errorf("unrecognized work reference %q (want owner/repo#N, host/owner/repo#N, or an issue/PR URL)", s)
}

// workKey validates owner/repo/num through pr.RefFromParts and renders the
// host-qualified reviewed-store key.
func workKey(host, owner, repo, num string) (string, error) {
	ref, err := pr.RefFromParts(owner, repo, num)
	if err != nil {
		return "", err
	}
	return host + "/" + ref.String(), nil
}

// hostAllowed reports whether host is acceptable for a host-qualified work
// ref: github.com always is; anything else must appear in hosts, the
// caller-supplied allowlist of configured review sources.
func hostAllowed(host string, hosts []string) bool {
	if host == GitHubHost {
		return true
	}
	for _, h := range hosts {
		if h == host {
			return true
		}
	}
	return false
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
