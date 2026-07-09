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
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ref identifies a single pull request. A bare-number form leaves Owner/Repo
// empty until ResolveRef fills them from the cwd repo's origin.
type Ref struct {
	Owner  string
	Repo   string
	Number int
}

// localOwnerSentinel is the reserved Owner value localRef (local.go) uses to
// mark a synthetic, offline-review Ref. It must never be assignable to a real
// PR ref — see IsLocal and the reservation check in refFrom/ResolveRef —
// otherwise a real GitHub owner literally named "local" could produce a Ref
// indistinguishable from a local-mode session, defeating the window-name and
// PostReview guards that key off it.
const localOwnerSentinel = "local"

// Slug renders the "owner/repo" form gh's --repo flag expects.
func (r Ref) Slug() string { return r.Owner + "/" + r.Repo }

// String renders the canonical "owner/repo#N" breadcrumb form.
func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// Complete reports whether Owner and Repo are both populated (a bare-number
// Ref is incomplete until ResolveRef runs).
func (r Ref) Complete() bool { return r.Owner != "" && r.Repo != "" }

// IsLocal reports whether ref identifies a synthetic local (offline) review
// session rather than a real PR — the canonical, persisted-and-reload-safe
// signal (unlike Session.Local, which is never restored from a breadcrumb).
func (r Ref) IsLocal() bool { return r.Owner == localOwnerSentinel }

// GitHub owner/repo charset: letters, digits, dot, underscore, hyphen. Kept
// deliberately tighter than a shell-safe set — anything outside it is rejected
// outright rather than escaped.
const ownerRepoClass = `[A-Za-z0-9._-]+`

// The three accepted forms, each FULLY anchored (^…$) so no prefix/suffix
// smuggling is possible. A partial match is a rejected match.
var (
	// owner/repo#N
	reSlug = regexp.MustCompile(`^(` + ownerRepoClass + `)/(` + ownerRepoClass + `)#([0-9]+)$`)
	// https://github.com/<owner>/<repo>/pull/<N>  (optional trailing slash)
	reURL = regexp.MustCompile(`^https://github\.com/(` + ownerRepoClass + `)/(` + ownerRepoClass + `)/pull/([0-9]+)/?$`)
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
		return refFrom(m[1], m[2], m[3])
	}
	if m := reURL.FindStringSubmatch(s); m != nil {
		return refFrom(m[1], m[2], m[3])
	}
	if m := reBare.FindStringSubmatch(s); m != nil {
		n, err := parseNumber(m[1])
		if err != nil {
			return Ref{}, err
		}
		return Ref{Number: n}, nil
	}
	return Ref{}, fmt.Errorf("unrecognized PR reference %q (want owner/repo#N, a github.com PR URL, or a bare number)", s)
}

// refFrom builds a validated Ref from regex-captured components. The owner and
// repo already passed the anchored charset; ".." is impossible under that
// class but rejected explicitly for defense in depth.
func refFrom(owner, repo, num string) (Ref, error) {
	if owner == ".." || repo == ".." {
		return Ref{}, fmt.Errorf("PR reference must not contain %q", "..")
	}
	// Deliberately NOT rejecting owner == localOwnerSentinel here: ParseRef
	// must stay permissive so a synthetic local Ref's String() round-trips
	// through it on breadcrumb reload (validateBreadcrumb calls ParseRef
	// directly). The reservation is enforced one layer up, in ResolveRef —
	// the actual entry point for a real, user-typed PR reference.
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
		return 0, fmt.Errorf("PR number %q is not a valid integer: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("PR number must be positive, got %d", n)
	}
	return n, nil
}

// ResolveRef parses s and, when it is a bare number, resolves Owner/Repo from
// the cwd repo's origin through the Runner. The resolved owner/repo are
// re-validated against the same anchored charset — `gh`/`git` output is itself
// hostile input.
//
// ResolveRef is the real entry point for a user-typed PR reference (the CLI's
// `pr <ref>` command calls it, never bare ParseRef), so this is where
// localOwnerSentinel is enforced as reserved: an owner of "local" — whether
// typed directly ("local/repo#5") or resolved from the cwd's origin — is
// refused here, never in ParseRef itself (which must stay permissive so a
// synthetic local Ref's String() still round-trips through it on breadcrumb
// reload).
func (c *Client) ResolveRef(ctx context.Context, s string) (Ref, error) {
	ref, err := ParseRef(s)
	if err != nil {
		return Ref{}, err
	}
	if ref.Complete() {
		if ref.Owner == localOwnerSentinel {
			return Ref{}, fmt.Errorf("PR reference owner %q is reserved for local (offline) review sessions", localOwnerSentinel)
		}
		return ref, nil
	}
	owner, repo, err := c.resolveOrigin(ctx)
	if err != nil {
		return Ref{}, fmt.Errorf("resolve bare PR number against origin: %w", err)
	}
	if !reOwner.MatchString(owner) || !reOwner.MatchString(repo) {
		return Ref{}, fmt.Errorf("origin owner/repo %q/%q outside allowed charset", owner, repo)
	}
	if owner == localOwnerSentinel {
		return Ref{}, fmt.Errorf("origin owner %q is reserved for local (offline) review sessions", localOwnerSentinel)
	}
	ref.Owner, ref.Repo = owner, repo
	return ref, nil
}

// reOwner matches a single owner or repo component in isolation (anchored).
var reOwner = regexp.MustCompile(`^` + ownerRepoClass + `$`)

// resolveOrigin asks gh for the cwd repo's owner/name, falling back to parsing
// the git origin URL. Both paths run through the Runner seam.
func (c *Client) resolveOrigin(ctx context.Context) (owner, repo string, err error) {
	out, ghErr := c.run.Run(ctx, "gh", "repo", "view", "--json", "owner,name", "-q", ".owner.login + \"/\" + .name")
	if ghErr == nil {
		if o, r, ok := splitSlug(out); ok {
			return o, r, nil
		}
	}
	url, gitErr := c.run.Run(ctx, "git", "remote", "get-url", "origin")
	if gitErr != nil {
		return "", "", fmt.Errorf("gh repo view and git remote both failed: %v; %w", ghErr, gitErr)
	}
	o, r, ok := parseRemoteURL(url)
	if !ok {
		return "", "", fmt.Errorf("could not parse owner/repo from origin URL %q", url)
	}
	return o, r, nil
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

// parseRemoteURL extracts owner/repo from an https or ssh GitHub remote URL,
// stripping any trailing ".git".
func parseRemoteURL(url string) (owner, repo string, ok bool) {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	switch {
	case strings.HasPrefix(url, "git@github.com:"):
		url = strings.TrimPrefix(url, "git@github.com:")
	case strings.HasPrefix(url, "https://github.com/"):
		url = strings.TrimPrefix(url, "https://github.com/")
	case strings.HasPrefix(url, "ssh://git@github.com/"):
		url = strings.TrimPrefix(url, "ssh://git@github.com/")
	default:
		return "", "", false
	}
	return splitSlug(url)
}
