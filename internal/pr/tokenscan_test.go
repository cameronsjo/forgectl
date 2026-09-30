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
// PostReview (every case posts); narrow gh[pousr]_ to ghp_ in
// githubTokenShape (the gho_/ghu_/ghs_/ghr_ cases post); drop
// stripFormatChars (the Cf cases, and the ref-spelled zwsp case, post); drop
// the switch in invisibleInToken (the joiner and Hangul filler cases post),
// or its unicode.Variation_Selector test (the selector cases post); drop
// the html.UnescapeString loop (the reference cases post), or cap it at one
// round (the double-encoded case posts); drop
// unescapeMarkdownPunct (the markdown case posts).
func TestPostReview_TokenShapedReviewIsRefused(t *testing.T) {
	tokens := map[string]string{
		"personal":     "gh" + "p_" + fakeTokenBody,
		"oauth":        "gh" + "o_" + fakeTokenBody,
		"user":         "gh" + "u_" + fakeTokenBody,
		"server":       "gh" + "s_" + fakeTokenBody,
		"refresh":      "gh" + "r_" + fakeTokenBody,
		"fine-grained": "github" + "_pat_" + fakeTokenBody + "_" + fakeTokenBody,
		"glued":        "token=x" + "gh" + "p_" + fakeTokenBody,
		// Invisible format characters (Cf) split the token.
		"zero-width space": "gh" + "p_" + fakeTokenBody[:10] + "\u200b" + fakeTokenBody[10:],
		"word joiner":      "gh" + "\u2060" + "p_" + fakeTokenBody,
		"bom":              "gh" + "p\ufeff_" + fakeTokenBody,
		"soft hyphen":      "github" + "_pat_" + fakeTokenBody[:5] + "\u00ad" + fakeTokenBody[5:],
		// Invisible characters outside Cf split it too (forgectl#764).
		"combining grapheme joiner": "gh" + "p_" + fakeTokenBody[:10] + "\u034f" + fakeTokenBody[10:],
		"variation selector":        "gh" + "p\ufe0f_" + fakeTokenBody,
		"supplementary selector":    "gh" + "p_" + fakeTokenBody[:10] + "\U000e0100" + fakeTokenBody[10:],
		"mongolian selector":        "gh" + "p_" + fakeTokenBody[:10] + "\u180b" + fakeTokenBody[10:],
		"hangul choseong filler":    "gh" + "p_" + fakeTokenBody[:10] + "\u115f" + fakeTokenBody[10:],
		"hangul jungseong filler":   "gh" + "p_" + fakeTokenBody[:10] + "\u1160" + fakeTokenBody[10:],
		"hangul filler":             "gh" + "\u3164" + "p_" + fakeTokenBody,
		"halfwidth hangul filler":   "github" + "_pat_" + fakeTokenBody[:5] + "\uffa0" + fakeTokenBody[5:],
		// HTML character references render as the character.
		"decimal ref":        "gh&#112;_" + fakeTokenBody,
		"hex ref":            "gh&#x70;_" + fakeTokenBody,
		"named ref":          "gh" + "p&lowbar;" + fakeTokenBody,
		"double-encoded ref": "gh&amp;#112;_" + fakeTokenBody,
		"ref-spelled zwsp":   "gh" + "p_" + fakeTokenBody[:10] + "&#8203;" + fakeTokenBody[10:],
		// A markdown backslash escape renders as the bare punctuation.
		"markdown escape": "gh" + "p\\_" + fakeTokenBody,
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

// A headless (staged) review carrying a token shape is refused too, rather
// than staged silently for a later post.
//
// Mutation that turns it red: move the scanReviewForTokens call back below
// the headless return in PostReview.
func TestPostReview_HeadlessTokenShapedReviewIsRefused(t *testing.T) {
	fake := successfulLaunchRunner()
	c := postClient(fake, true, true)
	review := "staged: " + "gh" + "p_" + fakeTokenBody

	posted, err := c.PostReview(context.Background(), testSess, review, true)
	if !errors.Is(err, errReviewHasTokenShape) {
		t.Fatalf("PostReview(headless) error = %v, want errReviewHasTokenShape", err)
	}
	if posted || len(fake.Calls) != 0 {
		t.Errorf("posted=%v calls=%+v, want refused with zero Runner calls", posted, fake.Calls)
	}
}
