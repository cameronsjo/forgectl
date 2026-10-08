// Package pr is the ops layer for `forgectl pr`: a clean-room PR review core.
// It resolves a PR reference, sandboxes the head into a throwaway workspace,
// quarantines any AI-instruction files as a reversible clean-room control,
// writes a deny-by-default agent allowlist, and dispatches a review agent into
// a tmux window — never posting anything without a human approval gate.
//
// Fetched PR content (the ref string, gh JSON, the checked-out file tree) is
// HOSTILE INPUT. Every seam that feeds a shell-out (ref parsing, breadcrumb
// location+content, teardown membership) validates before acting: anchored
// regexes, deny-by-default, exact-match set membership. It knows nothing of
// Cobra — that decoupling is the house pattern (see internal/tmux, net).
package pr

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Ref identifies a single pull request. A bare-number form leaves Owner/Repo
// empty until ResolveRef fills them from the cwd repo's origin.
type Ref struct {
	Owner  string
	Repo   string
	Number int

	// Host is the GitHub host the PR lives on, as a validated lowercase
	// hostname (ValidHostSegment). Empty means the client's configured
	// [github] host (#413): a typed owner/repo#N, a row from the pinned @me
	// search, and a session record written before records carried a host all
	// mean that host. A PR URL carries its own host, and a bare number
	// resolved from the checkout carries its remote's host. Every gh call
	// about the PR names this host twice over — `--repo HOST/OWNER/REPO` and a
	// runner pinned to the same host — so one PR can never be listed on one
	// host and viewed, cloned, or posted to on another.
	//
	// Host is NOT part of the session identity (window name, record filename,
	// session key): the same owner/repo#N on two hosts is one slot, and the
	// second is refused as already having a record.
	Host string

	// local marks a synthetic, offline-review Ref. It is the SECURITY
	// BOUNDARY behind IsLocal(), which decides whether PostReview may fire
	// and whether launchCodex widens the sandbox from read-only to
	// workspace-write.
	//
	// It does NOT decide whether the Codex reviewer is refused. That gate is
	// CheckAgentForReview (provenance.go), which reads the operator's
	// authorship declaration — locality is not authorship (forgectl#232).
	// Locality reaches provenance only as a DOWNGRADE: EffectiveProvenance
	// forces third-party on a non-local ref and can never upgrade one.
	//
	// It is unexported and set by exactly one constructor, newLocalRef
	// (local.go), so no external string — a gh response, a config value, a
	// typed ref — can spell it. Owner is a display value and carries no
	// locality; a real forge owner named "local" is an ordinary remote ref.
	//
	// WARNING: a field-by-field rebuild — Ref{Owner: r.Owner, Repo: r.Repo,
	// Number: r.Number} — silently DROPS this flag and yields a non-local
	// Ref. Copy the whole value, or re-apply asLocal(). Persistence across a
	// breadcrumb reload is likewise explicit: Breadcrumb.Local carries it,
	// and manage.go's loadSession re-applies it.
	local bool
}

// localOwnerSentinel is the Owner value newLocalRef (local.go) stamps on a
// synthetic, offline-review Ref.
//
// It is a DISPLAY value, not a security boundary: it names the local session
// in Ref.String(), the tmux window name, the breadcrumb filename, and the
// reviewed-store key. Locality itself lives in Ref.local. Nothing rejects a
// real forge owner spelled "local" — git.sjo.lol/local/tools#5 is an ordinary
// remote ref and parses as one.
const localOwnerSentinel = "local"

// Slug renders the "owner/repo" form. gh's --repo flag is given
// HOST/OWNER/REPO instead (see Client.prHost), so the host is never left to
// gh's defaults.
func (r Ref) Slug() string { return r.Owner + "/" + r.Repo }

// sameIdentity reports whether a and b name the same session identity:
// everything but Host, which is not part of it (see Ref.Host).
func (a Ref) sameIdentity(b Ref) bool {
	a.Host, b.Host = "", ""
	return a == b
}

// defaultGitHubHost is the host an empty Ref.Host means when no [github] host
// is wired in (tests; internal/cli always wires the configured one). It
// mirrors githubauth.DefaultHost, which this package cannot import.
const defaultGitHubHost = "github.com"

// HostnamePattern is the anchored hostname charset for a GitHub host:
// lowercase DNS labels only, no port, no scheme, no path. It lives here, not
// in internal/githubauth, because githubauth imports this package and both
// must apply the one pattern; githubauth compiles it for ResolveHost.
const HostnamePattern = `^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`

var reHostname = regexp.MustCompile(HostnamePattern)

// MaxHostSegmentBytes bounds a hostname used as a path segment, store key, or
// argv component: 253 is the DNS name limit. githubauth.MaxHostSegmentBytes
// is this value.
const MaxHostSegmentBytes = 253

// ValidHostSegment reports whether s is a normalized hostname: non-empty, at
// most MaxHostSegmentBytes, and inside HostnamePattern. It is a predicate, not
// a normalizer — callers lowercase first. githubauth.ValidHostSegment
// delegates here.
func ValidHostSegment(s string) bool {
	return s != "" && len(s) <= MaxHostSegmentBytes && reHostname.MatchString(s)
}

// String renders the canonical "owner/repo#N" breadcrumb form.
func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// Complete reports whether Owner and Repo are both populated (a bare-number
// Ref is incomplete until ResolveRef runs).
func (r Ref) Complete() bool { return r.Owner != "" && r.Repo != "" }

// IsLocal reports whether ref identifies a synthetic local (offline) review
// session rather than a real PR. It is the canonical locality predicate; the
// flag it reads is set only by newLocalRef and restored on breadcrumb reload
// from Breadcrumb.Local.
func (r Ref) IsLocal() bool { return r.local }

// asLocal returns a copy of r marked local. It is unexported and exists for
// the one reload path that must restore locality from a breadcrumb
// (manage.go's loadSession); construction goes through newLocalRef.
func (r Ref) asLocal() Ref {
	r.local = true
	return r
}

// GitHub owner/repo charset: letters, digits, dot, underscore, hyphen. Kept
// deliberately tighter than a shell-safe set — anything outside it is rejected
// outright rather than escaped.
const ownerRepoClass = `[A-Za-z0-9._-]+`

// The three accepted forms, each FULLY anchored (^…$) so no prefix/suffix
// smuggling is possible. A partial match is a rejected match.
var (
	// owner/repo#N
	reSlug = regexp.MustCompile(`^(` + ownerRepoClass + `)/(` + ownerRepoClass + `)#([0-9]+)$`)
	// https://<host>/<owner>/<repo>/pull/<N>  (optional trailing slash). The
	// host capture is loose here and must then pass ValidHostSegment after
	// lowercasing: no port, userinfo, or path can reach it.
	reURL = regexp.MustCompile(`^https://([A-Za-z0-9.-]+)/(` + ownerRepoClass + `)/(` + ownerRepoClass + `)/pull/([0-9]+)/?$`)
	// bare N
	reBare = regexp.MustCompile(`^([0-9]+)$`)
)

// ParseRef parses a PR reference string into a Ref, accepting exactly three
// forms — "owner/repo#N", a full GitHub PR URL, and a bare "N" — each matched
// by a fully-anchored regex. A bare number yields a Ref with empty Owner/Repo
// (resolve the origin with ResolveRef). Anything else — shell metacharacters,
// "..", a leading '-', extra path segments, an oversized number — is rejected.
//
// ParseRef is pure and takes no Runner: origin resolution for the bare form is
// a separate, Runner-backed step (ResolveRef) so the parse/validation surface
// stays trivially unit-testable and side-effect-free.
//
// Every Ref it returns is non-local, whatever the owner spells: locality is a
// separate unexported flag no parsed string can set.
func ParseRef(s string) (Ref, error) {
	// Trim only spaces/tabs — a convenience for copy-paste — but deliberately
	// NOT newlines or other control bytes: a trailing '\n' must fail the anchored
	// match (Go's $ is end-of-text) rather than be silently accepted, so an
	// embedded-newline injection is rejected, not normalized away.
	s = strings.Trim(s, " \t")
	if s == "" {
		return Ref{}, fmt.Errorf("empty PR reference")
	}
	if m := reSlug.FindStringSubmatch(s); m != nil {
		return RefFromParts(m[1], m[2], m[3])
	}
	if m := reURL.FindStringSubmatch(s); m != nil {
		host := strings.ToLower(m[1])
		if !ValidHostSegment(host) {
			return Ref{}, errors.New("PR URL host is outside the allowed hostname charset (lowercase dns name, no port)")
		}
		ref, err := RefFromParts(m[2], m[3], m[4])
		if err != nil {
			return Ref{}, err
		}
		ref.Host = host
		return ref, nil
	}
	if m := reBare.FindStringSubmatch(s); m != nil {
		n, err := parseNumber(m[1])
		if err != nil {
			return Ref{}, err
		}
		return Ref{Number: n}, nil
	}
	// s is argv the operator typed, or a record's ref string whose callers
	// replace this error with a categorical one; echo it capped (#562).
	return Ref{}, fmt.Errorf("unrecognized PR reference %s (want owner/repo#N, an https PR URL, or a bare number)", termsafe.QuoteArgMax(s, termsafe.ArgEchoMaxRunes))
}

// RefFromParts builds a validated Ref from separate owner/repo/number strings.
// It is the ONE anchored validator every reference path shares: ParseRef feeds
// it regex captures, the discovery parser feeds it gh-output fields, and
// internal/review feeds it un-prevalidated issue/PR rows — so it re-checks the
// anchored charset itself rather than assuming a caller already did. ".." is
// impossible under that class but rejected explicitly for defense in depth.
//
// The Ref it returns is always non-local — see ParseRef.
func RefFromParts(owner, repo, num string) (Ref, error) {
	if !reOwner.MatchString(owner) || !reOwner.MatchString(repo) {
		// Categorical (#562): RefFromParts is also fed gh output and review
		// rows, so the rejected value is never echoed.
		return Ref{}, errors.New("reference owner/repo outside allowed charset")
	}
	if owner == ".." || repo == ".." {
		return Ref{}, fmt.Errorf("PR reference must not contain %q", "..")
	}
	// The charset class permits '-' anywhere; a leading '-' would be option-like
	// if it ever reached git/gh as a positional (and GitHub owners/repos cannot
	// begin with '-' regardless). Reject it explicitly.
	if strings.HasPrefix(owner, "-") || strings.HasPrefix(repo, "-") {
		return Ref{}, fmt.Errorf("PR reference owner/repo must not begin with %q", "-")
	}
	n, err := parseNumber(num)
	if err != nil {
		return Ref{}, err
	}
	return Ref{Owner: owner, Repo: repo, Number: n}, nil
}

// parseNumber converts a digit-only PR number, rejecting zero, negatives (the
// regex already excludes a sign), and anything that overflows an int.
func parseNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		// strconv's error repeats the whole input, so it is dropped; s is a
		// digit run (the regex admits nothing else), echoed capped (#562).
		return 0, fmt.Errorf("PR number %s is out of range", termsafe.QuoteArgMax(s, termsafe.ArgEchoMaxRunes))
	}
	if n <= 0 {
		return 0, fmt.Errorf("PR number must be positive, got %d", n)
	}
	return n, nil
}

// ResolveRef parses s and, when it is a bare number, resolves Host/Owner/Repo
// from the cwd repo's origin through the Runner. The resolved owner/repo are
// re-validated with ValidOwnerRepoPart — the same bundled guard the typed-ref
// path applies (anchored charset plus the leading-'-' and ".." rejections) —
// because `gh`/`git` output is itself hostile input.
func (c *Client) ResolveRef(ctx context.Context, s string) (Ref, error) {
	ref, err := ParseRef(s)
	if err != nil {
		return Ref{}, err
	}
	if ref.Complete() {
		return ref, nil
	}
	host, owner, repo, err := c.resolveOrigin(ctx)
	if err != nil {
		return Ref{}, fmt.Errorf("resolve bare PR number against origin: %w", err)
	}
	if !ValidOwnerRepoPart(owner) || !ValidOwnerRepoPart(repo) {
		return Ref{}, errors.New("origin owner/repo is outside the allowed charset")
	}
	ref.Host, ref.Owner, ref.Repo = host, owner, repo
	return ref, nil
}

// reOwner matches a single owner or repo component in isolation (anchored).
var reOwner = regexp.MustCompile(`^` + ownerRepoClass + `$`)

// ValidOwnerRepoPart reports whether s is usable as a single owner or repo
// argv component: the anchored charset plus the leading-'-' and ".." guards
// RefFromParts applies — for callers that must vet a config-sourced value
// before it becomes an argv (the --owner scoping in search paths).
func ValidOwnerRepoPart(s string) bool {
	return reOwner.MatchString(s) && !strings.HasPrefix(s, "-") && s != ".."
}

// resolveOrigin resolves the cwd repository's host, owner, and name. It asks
// gh first (`gh repo view --json url`, which picks the checkout's base repo
// from its git remotes) and falls back to parsing the git origin URL. Either
// way the HOST comes from that remote URL, never from [github] host or gh's
// defaults: a bare number names a PR on the checkout's own forge (#413).
// Both paths run through the Runner seam, and both outputs are hostile input.
func (c *Client) resolveOrigin(ctx context.Context) (host, owner, repo string, err error) {
	out, ghErr := c.run.Run(ctx, "gh", "repo", "view", "--json", "url", "-q", ".url")
	if ghErr == nil {
		if h, o, r, ok := ParseRemoteURL(out); ok {
			return h, o, r, nil
		}
	}
	// Every error below is categorical (#562). The origin URL can embed a
	// credential (https://user:TOKEN@host/...), and the subprocess errors
	// carry gh's and git's stderr verbatim, so none of that text is echoed.
	url, gitErr := gitenv.Run(ctx, c.run, gitenv.Local, "remote", "get-url", "origin")
	if gitErr != nil {
		return "", "", "", errors.New("could not resolve the origin repository: gh repo view failed and git has no readable origin remote")
	}
	h, o, r, ok := ParseRemoteURL(url)
	if !ok {
		return "", "", "", errors.New("origin URL is not a recognised GitHub remote")
	}
	return h, o, r, nil
}

// reSCPRemote matches git's scp-like remote form, user@host:owner/repo.
var reSCPRemote = regexp.MustCompile(`^[A-Za-z0-9._-]+@([A-Za-z0-9.-]+):([^/]+)/([^/]+)$`)

// ParseRemoteURL extracts host, owner, and repo from a git remote URL in any
// of the three forms GitHub hands out — https://host/owner/repo,
// ssh://user@host[:port]/owner/repo, and user@host:owner/repo — stripping a
// trailing ".git". The host is lowercased and must pass ValidHostSegment.
// Owner and repo are returned unvalidated; callers vet them with
// ValidOwnerRepoPart.
//
// An https URL with a port is refused: a GitHub host is port-free by
// HostnamePattern, and guessing which host a ported URL means is how a PR
// ends up on the wrong forge. An ssh port is the ssh daemon's, not the web
// host's, and is ignored. Userinfo (an https credential) is dropped, never
// returned: callers must not echo the raw URL either (#562).
func ParseRemoteURL(raw string) (host, owner, repo string, ok bool) {
	raw = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(raw), "/"), ".git")
	var path string
	switch {
	case strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "ssh://"):
		u, err := neturl.Parse(raw)
		if err != nil || (u.Scheme == "https" && u.Port() != "") {
			return "", "", "", false
		}
		host, path = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	default:
		m := reSCPRemote.FindStringSubmatch(raw)
		if m == nil {
			return "", "", "", false
		}
		host, path = m[1], m[2]+"/"+m[3]
	}
	host = strings.ToLower(host)
	if !ValidHostSegment(host) {
		return "", "", "", false
	}
	o, r, found := strings.Cut(path, "/")
	if !found || o == "" || r == "" || strings.Contains(r, "/") {
		return "", "", "", false
	}
	return host, o, r, true
}

// splitSlug splits a trimmed "owner/repo" into its parts.
func splitSlug(s string) (owner, repo string, ok bool) {
	s = strings.TrimSpace(s)
	o, r, found := strings.Cut(s, "/")
	if !found || o == "" || r == "" {
		return "", "", false
	}
	return o, r, true
}
