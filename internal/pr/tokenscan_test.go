package pr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTokenBody is 36 characters of token-alphabet filler. Test tokens are
// built by concatenation so no literal credential shape sits in the source.
var fakeTokenBody = strings.Repeat("a1B2", 9)

// A drafted review carrying any GitHub token shape is refused before the
// approval gate, no gh argv reaches the Runner, and the error does not echo
// the token (forgectl#681).
//
// Mutations that turn it red: delete the scanReviewForTokens call from
// PostReview (every case posts); or narrow gh[pousr]_ to ghp_ in
// githubTokenShape (the gho_/ghu_/ghs_/ghr_ cases post).
func TestPostReview_TokenShapedReviewIsRefused(t *testing.T) {
	tokens := map[string]string{
		"personal":     "gh" + "p_" + fakeTokenBody,
		"oauth":        "gh" + "o_" + fakeTokenBody,
		"user":         "gh" + "u_" + fakeTokenBody,
		"server":       "gh" + "s_" + fakeTokenBody,
		"refresh":      "gh" + "r_" + fakeTokenBody,
		"fine-grained": "github" + "_pat_" + fakeTokenBody + "_" + fakeTokenBody,
		"glued":        "token=x" + "gh" + "p_" + fakeTokenBody,
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			fake := successfulLaunchRunner()
			approverAsked := false
			c := postClient(fake, true, true)
			c.approve = func(string) (bool, error) { approverAsked = true; return true, nil }
			review := "Looks good overall.\n\nDebug output:\n" + token + "\n\nOne nit in main.go."

			posted, err := c.PostReview(context.Background(), testSess, review, false)
			if !errors.Is(err, errReviewHasTokenShape) {
				t.Fatalf("PostReview error = %v, want errReviewHasTokenShape", err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), fakeTokenBody) {
				t.Errorf("refusal %q echoes the token", err)
			}
			if posted || len(fake.Calls) != 0 {
				t.Errorf("posted=%v calls=%+v, want refused with zero Runner calls", posted, fake.Calls)
			}
			if approverAsked {
				t.Error("the approval gate was shown for a review the scan refuses")
			}
		})
	}
}

// Prose that names a token prefix, or a short fragment after one, is not a
// token and still posts: the shape needs a token-length run after the prefix.
//
// Mutation that turns it red: drop the {30,} length floors from
// githubTokenShape (prose naming the prefix is refused).
func TestPostReview_TokenPrefixInProseStillPosts(t *testing.T) {
	fake := successfulLaunchRunner()
	c := postClient(fake, true, true)
	review := "Never log a gh" + "p_ token or a github" + "_pat_ value; gh" + "s_abc123 in the fixture is fine."

	posted, err := c.PostReview(context.Background(), testSess, review, false)
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if !posted {
		t.Error("a review that only names token prefixes was not posted")
	}
}
