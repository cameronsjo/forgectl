package pr

import (
	"errors"
	"regexp"
)

// githubTokenShape matches the published GitHub credential formats: the
// prefixed tokens gh[pousr]_ (personal access, OAuth, user-to-server,
// server-to-server, refresh) and fine-grained personal access tokens,
// github_pat_. The length floors sit below the real lengths (36 characters
// after a gh?_ prefix, 82 after github_pat_) so a token cut short in a paste
// still matches, and above anything prose writes when it merely names the
// prefix. There is no leading word boundary: a token glued to other text is
// still a token.
//
// It is a shape match, not a proof. An agent that encodes a token (base64,
// split across lines) passes it; this is the tripwire behind the approval
// gate, not a replacement for it (forgectl#681).
var githubTokenShape = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`)

// errReviewHasTokenShape is PostReview's refusal of a drafted review that
// carries a GitHub token shape. Its text never includes the match.
var errReviewHasTokenShape = errors.New("refusing to post the review: it contains text shaped like a GitHub token; " +
	"remove it from the review before posting")

// scanReviewForTokens refuses a review whose text carries a GitHub token
// shape. A prompt-injected reviewer can read gh's stored credentials and
// paste a token into its draft, and a long review makes one easy to miss at
// the approval gate (forgectl#681).
func scanReviewForTokens(review string) error {
	if githubTokenShape.MatchString(review) {
		return errReviewHasTokenShape
	}
	return nil
}
