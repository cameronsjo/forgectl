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

// GitHubHost is the DEFAULT GitHub host — the Host every GitHub-sourced Item
// carries when no [github].host is configured. The effective host is instance
// data on the GitHub source (NewGitHub) and a parameter to the work-ref
// parser; this constant is what they default to. Phase C's Gitea source
// stamps its own host, which is why Key() is host-qualified.
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

// gitHubWorkURLRe builds the accepted GitHub-family work-ref URL form for the
// effective host: an issue or /pull/ URL (optional trailing slash), FULLY
// anchored like every ref regex in internal/pr. `pull` (never `pulls`) keeps
// GitHub-family strictness for a configured enterprise host too — the pattern
// is per-host, the shape is not. The host is interpolated through
// regexp.QuoteMeta: it is validated config, but a raw interpolation would
// turn its dots into wildcards and any future metachar into pattern syntax
// (step-0 security review, finding 2).
func gitHubWorkURLRe(effectiveHost string) *regexp.Regexp {
	return regexp.MustCompile(`^https://` + regexp.QuoteMeta(effectiveHost) + `/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/(?:issues|pull)/([0-9]+)/?$`)
}

// reGitHubSlug matches the unqualified default-host slug form: owner/repo#N
// (exactly two slash-delimited segments before the '#').
var reGitHubSlug = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)

// hostSeg is the host-segment pattern shared by reHostSlug and
// reHostWorkURL: a bare hostname plus an OPTIONAL ":port", matching what
// reGiteaHost (internal/review/gitea.go) actually allows to be configured —
// a Gitea instance on a non-default port produces "host:port" keys, and both
// host-qualified forms must be able to parse them back.
const hostSeg = `[A-Za-z0-9._-]+(?::[0-9]{1,5})?`

// reHostSlug matches a host-qualified slug form: host[:port]/owner/repo#N
// (three segments before the '#'). Distinct from reGitHubSlug by segment
// count, so the two never collide on the same input.
var reHostSlug = regexp.MustCompile(`^(` + hostSeg + `)/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)

// reHostWorkURL matches a fully-qualified issue/pull(s) URL against ANY host
// — github.com's "pull" and Gitea's "pulls" are both accepted by the regex;
// the allowlist check happens after extraction, in ParseWorkRefForHosts, not
// in the pattern itself.
var reHostWorkURL = regexp.MustCompile(`^https://(` + hostSeg + `)/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/(?:issues|pulls?)/([0-9]+)/?$`)

// ParseWorkRef normalizes a user-typed work reference for the DEFAULT GitHub
// host, github.com — "owner/repo#N" or a full github.com issue/pull URL — to
// the host-qualified reviewed-store key. It is ParseWorkRefForHosts with the
// default effective host and no extra hosts configured; callers that live
// under a configured [github].host must use ParseWorkRefForHosts with that
// host instead, or their keys will disagree with the inventory's.
func ParseWorkRef(s string) (key string, err error) {
	return ParseWorkRefForHosts(s, GitHubHost, nil)
}

// ParseWorkRefForHosts normalizes a user-typed work reference against the
// deployment's effective GitHub host (githubauth ResolveHost output; the
// unqualified "owner/repo#N" shorthand and the GitHub-shaped URL form both
// resolve to it), plus two host-qualified forms — "host/owner/repo#N" and a
// full issue/pull(s) URL — gated on hosts, the caller-supplied allowlist of
// configured non-github hosts (mark/unmark pass their configured Gitea host
// through here). No host is special-cased: a literal github.com ref under a
// non-default effective host is REJECTED, deliberately — accepting it would
// mint keys no active source re-verifies, i.e. never-prunable marks. Every
// extracted owner/repo/number still rides pr.RefFromParts, the one anchored
// validator every ref path shares.
func ParseWorkRefForHosts(s, effectiveHost string, hosts []string) (key string, err error) {
	s = strings.Trim(s, " \t")
	if s == "" {
		return "", fmt.Errorf("empty work reference")
	}
	if m := gitHubWorkURLRe(effectiveHost).FindStringSubmatch(s); m != nil {
		return workKey(effectiveHost, m[1], m[2], m[3])
	}
	if m := reGitHubSlug.FindStringSubmatch(s); m != nil {
		return workKey(effectiveHost, m[1], m[2], m[3])
	}
	if m := reHostWorkURL.FindStringSubmatch(s); m != nil {
		canonical, ok := allowedHostSpelling(m[1], effectiveHost, hosts)
		if !ok {
			return "", fmt.Errorf("work reference host %q is not configured", m[1])
		}
		return workKey(canonical, m[2], m[3], m[4])
	}
	if m := reHostSlug.FindStringSubmatch(s); m != nil {
		canonical, ok := allowedHostSpelling(m[1], effectiveHost, hosts)
		if !ok {
			return "", fmt.Errorf("work reference host %q is not configured", m[1])
		}
		return workKey(canonical, m[2], m[3], m[4])
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

// allowedHostSpelling reports whether a typed host is acceptable for a
// host-qualified work ref — the effective GitHub host, or any member of
// hosts, the caller-supplied allowlist of configured review sources — and
// returns the CONFIGURED spelling, so the minted key always matches the
// spelling the source stamps on its items regardless of how the user typed
// the host. Comparison folds case (hostnames are case-insensitive; a user
// typing GitHub.MyCorp.com must not be told the host "is not configured").
// There is no unconditional github.com arm any more: under a non-default
// [github].host, github.com has no active source, and a key nobody
// re-verifies is a mark nobody can ever prune.
func allowedHostSpelling(host, effectiveHost string, hosts []string) (string, bool) {
	if strings.EqualFold(host, effectiveHost) {
		return effectiveHost, true
	}
	for _, h := range hosts {
		if strings.EqualFold(h, host) {
			return h, true
		}
	}
	return "", false
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
