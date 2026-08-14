package githubauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// MaxOwners caps how many distinct owners one operation may enumerate. The
// owner set comes from low-trust config, and each owner becomes at least one
// subprocess, so the count is bounded before any of them is spawned.
const MaxOwners = 64

// MaxOwnerBytes caps a single owner value's length. GitHub logins are far
// shorter; the bound exists so a hostile config value cannot become an
// oversized argv component.
const MaxOwnerBytes = 256

// ErrLoginUnavailable is the safe sentinel returned when the authenticated
// GitHub.com login cannot be established — no auth, malformed output, a
// rejected value, or a cancelled discovery call. Callers match it with
// errors.Is; the raw subprocess cause never rides along, because gh stderr can
// carry tokens and terminal control sequences.
var ErrLoginUnavailable = errors.New("authenticated GitHub login unavailable")

// ValidOwner reports whether owner is usable as a `gh` argv owner component.
// It delegates to pr.ValidOwnerRepoPart — the repo's one anchored owner
// predicate (charset plus the leading-'-' and ".." guards) — deliberately
// rather than growing a second regex that could drift from it at an argv
// boundary.
func ValidOwner(owner string) bool { return pr.ValidOwnerRepoPart(owner) }

// ResolveOwners returns the owner set an operation should enumerate.
//
// A non-empty configured list is authoritative and makes zero discovery calls.
// An absent or explicitly empty list resolves the authenticated GitHub.com
// login exactly once, through the pinned Runner so an ambient GH_HOST cannot
// redirect the question at a GitHub Enterprise instance.
//
// Every value — configured or discovered — passes the same validation and
// byte budget, and the whole set is deduplicated case-insensitively (GitHub
// logins are case-insensitive; the first spelling and input order are kept)
// and count-bounded before it is returned. A refusal returns no owners at all:
// a bad later element must not leave earlier elements queryable.
func ResolveOwners(ctx context.Context, run exec.Runner, configured []string) ([]string, error) {
	if len(configured) == 0 {
		login, err := discoverLogin(ctx, run)
		if err != nil {
			return nil, err
		}
		return []string{login}, nil
	}
	return normalizeOwners(configured)
}

// discoverLogin asks GitHub.com who the operator is, through the pinned
// runner, and validates the answer as hostile input.
func discoverLogin(ctx context.Context, run exec.Runner) (string, error) {
	out, err := Runner(run).Run(ctx, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		// The pinned runner has already reduced a cancellation or expired
		// deadline to a bare standard sentinel; join it so callers keep both
		// identities. Anything else is a raw cause and is dropped entirely.
		if SafeContextSentinel(err) {
			return "", errors.Join(ErrLoginUnavailable, err)
		}
		return "", fmt.Errorf("%w: gh could not report the authenticated login", ErrLoginUnavailable)
	}
	login, ok := parseLogin(out)
	if !ok {
		return "", fmt.Errorf("%w: gh returned an unusable login value", ErrLoginUnavailable)
	}
	return login, nil
}

// parseLogin validates one login out of POST-Runner output.
//
// The input is what exec.OSRunner.Run already returned, which has had every
// trailing LF stripped — so a surviving "\n" means gh emitted more than one
// record, and a single trailing "\r" is the residue of one stripped CRLF. No
// claim is made about how many terminal bare LFs the child wrote: production
// cannot observe that, so neither can a test.
func parseLogin(out string) (string, bool) {
	if strings.Contains(out, "\n") {
		return "", false
	}
	login := strings.TrimSuffix(out, "\r")
	if strings.Contains(login, "\r") {
		return "", false
	}
	if login == "" || len(login) > MaxOwnerBytes || !ValidOwner(login) {
		return "", false
	}
	return login, true
}

// normalizeOwners validates, deduplicates, and bounds a configured owner list.
// Every value is checked before anything is returned, so a malformed element
// anywhere in the list means zero owner queries rather than a partial scope.
func normalizeOwners(configured []string) ([]string, error) {
	owners := make([]string, 0, len(configured))
	seen := make(map[string]struct{}, len(configured))
	for i, owner := range configured {
		if len(owner) > MaxOwnerBytes {
			return nil, fmt.Errorf("configured owner %d is longer than %d bytes", i+1, MaxOwnerBytes)
		}
		if !ValidOwner(owner) {
			return nil, fmt.Errorf("configured owner %d is outside the allowed owner charset", i+1)
		}
		key := strings.ToLower(owner)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		owners = append(owners, owner)
	}
	if len(owners) > MaxOwners {
		return nil, fmt.Errorf("configured owners resolve to %d distinct accounts, more than the %d allowed", len(owners), MaxOwners)
	}
	return owners, nil
}
