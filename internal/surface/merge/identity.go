// Package merge is the worker PR merge policy (ADR-0011, 2026-10-09
// amendment): what forgectl reads from GitHub about one worker's pull
// request, and the one pure function that decides whether the policy passes
// for it. `surface status`, `surface merge` and the drain all call Evaluate;
// nothing else decides.
package merge

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// IdentityQuery reads a repository's canonical nameWithOwner and numeric
// id, and whether a branch exists on it. A worker launch records the first
// two on its ledger row, so the merge policy never re-derives the repository
// from a .git/config a worker can rewrite. GitHub follows a renamed
// repository under its old name, and answers with the current one.
const IdentityQuery = `query($owner: String!, $name: String!, $ref: String!) {
  repository(owner: $owner, name: $name) {
    databaseId
    nameWithOwner
    ref(qualifiedName: $ref) { name }
  }
}`

// Identity is a repository as GitHub names it, and whether the branch asked
// about exists there.
type Identity struct {
	NameWithOwner string
	DatabaseID    int64
	// RefExists is true when the branch the query named exists on GitHub.
	RefExists bool
}

// ErrIdentityResponse reports an identity response forgectl will not read.
var ErrIdentityResponse = errors.New("merge: unusable GitHub repository response")

// maxErrorText bounds a GraphQL error message quoted in an error.
const maxErrorText = 200

// graphQLError is one entry of a GraphQL response's errors array.
type graphQLError struct {
	Message string `json:"message"`
}

// firstError words a non-empty GraphQL errors array for an error message.
func firstError(errs []graphQLError) string {
	msg := errs[0].Message
	if len(msg) > maxErrorText {
		msg = msg[:maxErrorText]
	}
	return fmt.Sprintf("GitHub returned %d error(s), the first %q", len(errs), msg)
}

// DecodeIdentity reads an IdentityQuery response. Any GraphQL error refuses
// it, as does a repository with no name or a non-positive id.
func DecodeIdentity(data []byte) (Identity, error) {
	var resp struct {
		Data *struct {
			Repository *struct {
				DatabaseID    int64  `json:"databaseId"`
				NameWithOwner string `json:"nameWithOwner"`
				Ref           *struct {
					Name string `json:"name"`
				} `json:"ref"`
			} `json:"repository"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return Identity{}, fmt.Errorf("%w: not JSON: %w", ErrIdentityResponse, err)
	}
	if len(resp.Errors) > 0 {
		return Identity{}, fmt.Errorf("%w: %s", ErrIdentityResponse, firstError(resp.Errors))
	}
	if resp.Data == nil || resp.Data.Repository == nil {
		return Identity{}, fmt.Errorf("%w: no repository in the response", ErrIdentityResponse)
	}
	r := resp.Data.Repository
	if r.DatabaseID <= 0 {
		return Identity{}, fmt.Errorf("%w: the repository's databaseId is %d, expected a positive id", ErrIdentityResponse, r.DatabaseID)
	}
	if !validNameWithOwner(r.NameWithOwner) {
		return Identity{}, fmt.Errorf("%w: the repository's nameWithOwner is %q, expected owner/name", ErrIdentityResponse, r.NameWithOwner)
	}
	return Identity{NameWithOwner: r.NameWithOwner, DatabaseID: r.DatabaseID, RefExists: r.Ref != nil}, nil
}

// validNameWithOwner reports an owner/name pair of GitHub's charset: two
// non-empty parts of letters, digits, '-', '_' and '.', the parts not "."
// or "..".
func validNameWithOwner(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	if !ok {
		return false
	}
	for _, part := range []string{owner, name} {
		if part == "" || part == "." || part == ".." || len(part) > 100 {
			return false
		}
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			default:
				return false
			}
		}
	}
	return true
}

// SplitNameWithOwner splits a recorded owner/name into its parts.
func SplitNameWithOwner(s string) (owner, name string, err error) {
	if !validNameWithOwner(s) {
		return "", "", fmt.Errorf("merge: %q is not an owner/name repository", s)
	}
	owner, name, _ = strings.Cut(s, "/")
	return owner, name, nil
}
