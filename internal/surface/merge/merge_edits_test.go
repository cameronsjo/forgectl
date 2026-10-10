package merge

import (
	"errors"
	"strings"
	"testing"
)

// Tests for cameronsjo/forgectl#1208: posts ordered by their last edit, the
// wider loose marker scan, and inline review comments.

const findingURL = "https://github.com/cameronsjo/forgectl/pull/1204#pullrequestreview-77"

// polishPass is a strict passing polish marker at the head, an hour after
// passingFacts' own (17:00:01), so tests can place posts around it.
func polishPass(at string) Review {
	r := markerReview("polish", head1204, 0, 0, at)
	r.URL = "https://github.com/cameronsjo/forgectl/pull/1204#pullrequestreview-88"
	return r
}

// TestOpenFindingsOrderedByEdit pins that a post counts at its last edit: a
// finding posted before a pass and edited after it is not cleared by it.
func TestOpenFindingsOrderedByEdit(t *testing.T) {
	finding := func(edited string) Review {
		r := markerReview("polish", head1204, 1, 0, "2026-10-09T18:00:00Z")
		r.URL, r.LastEditedAt = findingURL, edited
		return r
	}
	t.Run("a finding edited after a later pass refuses", func(t *testing.T) {
		f := passingFacts(t)
		f.Reviews = append(f.Reviews, finding("2026-10-09T20:00:00Z"), polishPass("2026-10-09T19:00:00Z"))
		v := evalManual(f)
		wantRefusal(t, v, "it was edited at 2026-10-09T20:00:00Z; an edited post never counts as a passing marker; polish reported crit=1 imp=0")
		wantRefusal(t, v, "(review "+findingURL+", 2026-10-09T20:00:00Z), expected a later passing marker by polish")
	})
	t.Run("control: the same finding unedited is cleared by the later pass", func(t *testing.T) {
		f := passingFacts(t)
		f.Reviews = append(f.Reviews, finding(""), polishPass("2026-10-09T19:00:00Z"))
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("control: a finding edited before the later pass is cleared by it", func(t *testing.T) {
		f := passingFacts(t)
		f.Reviews = append(f.Reviews, finding("2026-10-09T18:30:00Z"), polishPass("2026-10-09T19:00:00Z"))
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("a conversation comment edited after the pass refuses", func(t *testing.T) {
		f := passingFacts(t)
		f.Comments = append(f.Comments, Comment{Author: markerReview("x", head1204, 0, 0, "").Author, Body: "cadence-review: polish crit=1",
			URL: "https://example.test/c/5", CreatedAt: "2026-10-09T16:00:00Z", LastEditedAt: "2026-10-09T18:00:00Z"})
		wantRefusal(t, evalManual(f), "(conversation comment https://example.test/c/5, 2026-10-09T18:00:00Z), expected a later passing marker by polish")
	})
}

// TestEditedPassClearsNothing pins that an edit never turns a finding into a
// pass: an edited strict marker at the head clears no earlier finding, is
// itself an open finding until a later unedited pass, and is never the
// approver's passing marker.
func TestEditedPassClearsNothing(t *testing.T) {
	edited := polishPass("2026-10-09T19:00:00Z")
	edited.LastEditedAt = "2026-10-09T19:30:00Z"
	t.Run("an earlier finding stays open", func(t *testing.T) {
		f := passingFacts(t)
		early := markerReview("polish", head1204, 0, 2, "2026-10-09T18:00:00Z")
		early.URL = findingURL
		f.Reviews = append(f.Reviews, early, edited)
		v := evalManual(f)
		wantRefusal(t, v, "polish reported crit=0 imp=2 at head 3afe70e8bff8 (review "+findingURL)
		wantRefusal(t, v, "it was edited at 2026-10-09T19:30:00Z; an edited post never counts as a passing marker (review https://github.com/cameronsjo/forgectl/pull/1204#pullrequestreview-88")
		wantRefusal(t, v, "polish's latest marker is head=3afe70e8bff8 (review commit 3afe70e8bff8) crit=0 imp=0, edited after it was posted (an edited marker never passes)")
	})
	t.Run("a later unedited pass clears the edited one", func(t *testing.T) {
		f := passingFacts(t)
		f.Reviews = append(f.Reviews, edited, polishPass("2026-10-09T20:00:00Z"))
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("a pass between the post and its edit does not clear it", func(t *testing.T) {
		f := passingFacts(t)
		f.Reviews = append(f.Reviews, edited, polishPass("2026-10-09T19:15:00Z"))
		wantRefusal(t, evalManual(f), "(review https://github.com/cameronsjo/forgectl/pull/1204#pullrequestreview-88, 2026-10-09T19:30:00Z), expected a later passing marker by polish")
	})
	t.Run("an edit the read did not report refuses outright", func(t *testing.T) {
		f := passingFacts(t)
		r := polishPass("2026-10-09T15:00:00Z")
		r.EditUnread = true
		f.Reviews = append(f.Reviews, r)
		wantRefusal(t, evalManual(f), "the read did not say whether it was edited (no lastEditedAt)")
	})
	t.Run("an edit time that does not parse refuses outright", func(t *testing.T) {
		f := passingFacts(t)
		r := polishPass("2026-10-09T15:00:00Z")
		r.LastEditedAt = "yesterday"
		f.Reviews = append(f.Reviews, r)
		wantRefusal(t, evalManual(f), `has lastEditedAt "yesterday", expected a timestamp`)
	})
}

// TestLooseMarkerScanIsWide pins the normalized scan: each spelling below,
// posted after the last pass, is an open finding for polish.
func TestLooseMarkerScanIsWide(t *testing.T) {
	spellings := map[string]string{
		"space before the colon":     "cadence-review : polish crit=1",
		"a space for the hyphen":     "cadence review: polish crit=1",
		"an underscore":              "cadence_review: polish crit=1",
		"a markdown escape":          `cadence\-review: polish crit=1`,
		"no separator, upper case":   "CADENCEREVIEW: polish crit=1",
		"U+2010 hyphen":              "cadence\u2010review: polish crit=1",
		"U+2011 non-breaking hyphen": "cadence\u2011review: polish crit=1",
		"U+2013 en dash":             "cadence\u2013review: polish crit=1",
		"U+2014 em dash":             "cadence\u2014review: polish crit=1",
		"U+2212 minus":               "cadence\u2212review: polish crit=1",
		"U+FE63 small hyphen":        "cadence\ufe63review: polish crit=1",
		"a zero-width space":         "cadence-re\u200bview: polish crit=1",
		"a word joiner":              "cadence-rev\u2060iew: polish crit=1",
		"a soft hyphen":              "cadence-re\u00adview: polish crit=1",
		"a bidi override":            "cadence-\u202ereview: polish crit=1",
		"a zero-width joiner":        "cadence\u200d-review: polish crit=1",
		"full-width letters":         "\uff43\uff41\uff44\uff45\uff4e\uff43\uff45\uff0d\uff52\uff45\uff56\uff49\uff45\uff57\uff1a polish crit=1",
		"a no-break space":           "cadence\u00a0review\u00a0: polish crit=1",
		"an entity hyphen":           "cadence&#45;review: polish crit=1",
		"an entity colon":            "cadence-review&#58; polish crit=1",
		"an entity zero-width space": "cadence&#8203;-review: polish crit=1",
		"a named entity":             "cadence&hyphen;review: polish crit=1",
		"split across a line end":    "cadence-\nreview\n: polish crit=1",
		"invalid UTF-8 inside":       "cadence-\xffreview: polish crit=1",
	}
	for name, body := range spellings {
		t.Run(name, func(t *testing.T) {
			f := passingFacts(t)
			r := markerReview("polish", head1204, 0, 0, "2026-10-09T18:00:00Z")
			r.URL, r.Body = findingURL, "Notes.\n"+body
			f.Reviews = append(f.Reviews, r)
			wantRefusal(t, evalManual(f), "(review "+findingURL+", 2026-10-09T18:00:00Z), expected a later passing marker by polish")
		})
	}
	// Controls: text that does not spell the word is not a mention.
	for name, body := range map[string]string{
		"another word":    "cadence-preview: polish crit=1",
		"a plain comment": "Looks good; the cadence of these reviews is fine.",
		"the words apart": "cadence and then a review: polish crit=1",
	} {
		t.Run("control: "+name, func(t *testing.T) {
			f := passingFacts(t)
			r := markerReview("polish", head1204, 0, 0, "2026-10-09T18:00:00Z")
			r.URL, r.Body = findingURL, body
			f.Reviews = append(f.Reviews, r)
			if v := evalManual(f); v.Result != Pass {
				t.Fatalf("%s %q", v.Result, v.Reasons)
			}
		})
	}
}

// TestLooseMarkerCatchesAnyStyling pins the skeleton scan (independent
// review I3 of cameronsjo/forgectl#1212): a body whose letters spell
// "cadencereview", however it is styled, is a marker mention. One whose
// reviewer name the loose parse can read is an open finding for that
// reviewer; one it cannot read a name from refuses outright.
func TestLooseMarkerCatchesAnyStyling(t *testing.T) {
	post := func(t *testing.T, body string) Verdict {
		t.Helper()
		f := passingFacts(t)
		r := markerReview("polish", head1204, 0, 0, "2026-10-09T18:00:00Z")
		r.URL, r.Body = findingURL, "Notes.\n"+body
		f.Reviews = append(f.Reviews, r)
		return evalManual(f)
	}
	outright := map[string]string{
		"bold":                  "**cadence-review**: sec crit=1",
		"a code span":           "`cadence-review`: sec crit=1",
		"an asterisk":           "cadence*review: sec crit=1",
		"a full stop":           "cadence.review: sec crit=1",
		"U+2043 hyphen bullet":  "cadence\u2043review: sec crit=1",
		"U+2236 ratio":          "cadence-review\u2236 sec crit=1",
		"strike-through":        "~~cadence-review~~: sec crit=1",
		"reviewer, not review":  "cadence-reviewer: polish crit=1",
		"no colon":              "the cadence-review marker is below",
		"a second, bold one":    "cadence-review: polish crit=1\n**cadence-review**: sec crit=1",
		"Cyrillic and a colon":  "c\u0430dence-review\u2236 sec",
		"look-alikes, no colon": "\u0421\u0410DENCE R\u0415V\u0406EW",
	}
	for name, body := range outright {
		t.Run("no name: "+name, func(t *testing.T) {
			wantRefusal(t, post(t, body), "open finding: review "+findingURL+" mentions cadence-review")
			wantRefusal(t, post(t, body), "in a form no reviewer name can be read from")
		})
	}
	named := map[string]string{
		"Cyrillic i in review":    "cadence-rev\u0456ew: polish crit=1",
		"Greek alpha and epsilon": "c\u03b1d\u03b5nce-review: polish crit=1",
		"Cyrillic capitals":       "\u0421ADEN\u0421E-REVIEW: polish crit=1",
		"small capitals":          "\u1d04\u1d00\u1d05\u1d07\u0274\u1d04\u1d07-\u0280\u1d07\u1d20\u026a\u1d07\u1d21: polish crit=1",
		"Greek omicron elsewhere": "cadence-review: p\u03bflish crit=1",
		"full-width, Cyrillic e":  "\uff43\uff41\uff44\u0435\uff4e\uff43\uff45-review: polish crit=1",
	}
	for name, body := range named {
		t.Run("named: "+name, func(t *testing.T) {
			wantRefusal(t, post(t, body), "(review "+findingURL+", 2026-10-09T18:00:00Z), expected a later passing marker by polish")
		})
	}
	t.Run("the strict pass still passes", func(t *testing.T) {
		if v := evalManual(passingFacts(t)); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
}

// TestInlineReviewCommentsAreFindings pins that an inline review comment by
// marker_author_id mentioning the marker is an open finding, never a pass.
func TestInlineReviewCommentsAreFindings(t *testing.T) {
	strict := "<!-- cadence-review: polish head=" + head1204 + " crit=0 imp=0 -->"
	inline := func(at string, author int64) Comment {
		c := Comment{Author: markerReview("x", head1204, 0, 0, "").Author, Body: strict, URL: "https://example.test/r/1", CreatedAt: at}
		c.Author.DatabaseID = author
		return c
	}
	t.Run("a strict marker inline refuses", func(t *testing.T) {
		f := passingFacts(t)
		f.ReviewComments = append(f.ReviewComments, inline("2026-10-09T18:00:00Z", operatorID))
		wantRefusal(t, evalManual(f), "a inline review comment never counts as a passing marker (inline review comment https://example.test/r/1, 2026-10-09T18:00:00Z), expected a later passing marker by polish")
	})
	t.Run("an inline marker cannot clear an earlier finding", func(t *testing.T) {
		f := passingFacts(t)
		early := markerReview("polish", head1204, 1, 0, "2026-10-09T18:00:00Z")
		early.URL = findingURL
		f.Reviews = append(f.Reviews, early)
		f.ReviewComments = append(f.ReviewComments, inline("2026-10-09T19:00:00Z", operatorID))
		wantRefusal(t, evalManual(f), "polish reported crit=1 imp=0 at head 3afe70e8bff8 (review "+findingURL)
	})
	t.Run("control: an inline mention before the pass is cleared", func(t *testing.T) {
		f := passingFacts(t)
		f.ReviewComments = append(f.ReviewComments, inline("2026-10-09T16:00:00Z", operatorID))
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
	t.Run("control: another account's inline marker is not read", func(t *testing.T) {
		f := passingFacts(t)
		f.ReviewComments = append(f.ReviewComments, inline("2026-10-09T18:00:00Z", 556))
		if v := evalManual(f); v.Result != Pass {
			t.Fatalf("%s %q", v.Result, v.Reasons)
		}
	})
}

// TestDecodePRReadsEditsAndInlineComments pins the decode side: a review's
// inline comments and every lastEditedAt are read, a missing lastEditedAt is
// unread (not "never edited"), and a review with no inline comment
// connection, or one with another page, refuses the read.
func TestDecodePRReadsEditsAndInlineComments(t *testing.T) {
	pr := string(readFixture(t, "pr_1204.json"))
	const emptyNodes = `"nodes": []
       }`
	if !strings.Contains(pr, emptyNodes) {
		t.Fatal("the fixture has no empty inline comment list to edit")
	}
	withInline := strings.Replace(pr, emptyNodes, `"nodes": [{"author": {"__typename": "User", "login": "cameronsjo", "databaseId": 4084915},
         "body": "cadence-review: polish crit=1", "url": "https://example.test/r/2", "createdAt": "2026-10-09T18:00:00Z", "lastEditedAt": "2026-10-09T18:05:00Z"}]
       }`, 1)
	r, err := DecodePR([]byte(withInline))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ReviewComments) != 1 || r.ReviewComments[0].LastEditedAt != "2026-10-09T18:05:00Z" || r.ReviewComments[0].URL != "https://example.test/r/2" || r.ReviewComments[0].EditUnread {
		t.Fatalf("inline comments %+v", r.ReviewComments)
	}
	if len(r.Reviews) != 1 || r.Reviews[0].EditUnread || r.Reviews[0].LastEditedAt != "" {
		t.Fatalf("the fixture review's lastEditedAt null should read as never edited: %+v", r.Reviews)
	}
	// The conversation comment in the fixture has no lastEditedAt: unread.
	if len(r.Comments) != 1 || !r.Comments[0].EditUnread {
		t.Fatalf("a comment with no lastEditedAt field: %+v", r.Comments)
	}
	if !strings.Contains(PRQuery, "lastEditedAt\n") || !strings.Contains(PRQuery, "comments(first: 100) {\n            pageInfo { hasNextPage }") {
		t.Fatal("PRQuery does not read lastEditedAt and each review's inline comments")
	}
	for name, body := range map[string]string{
		"another page of inline comments": strings.Replace(pr, `"comments": {
        "pageInfo": {
         "hasNextPage": false`, `"comments": {
        "pageInfo": {
         "hasNextPage": true`, 1),
		"no inline comment connection": strings.Replace(pr, `"comments": {
        "pageInfo"`, `"notComments": {
        "pageInfo"`, 1),
		"a lastEditedAt that is not a string": strings.Replace(pr, `"lastEditedAt": null`, `"lastEditedAt": 7`, 1),
		"an empty lastEditedAt":               strings.Replace(pr, `"lastEditedAt": null`, `"lastEditedAt": ""`, 1),
	} {
		if body == pr {
			t.Fatalf("%s: the fixture edit did not apply", name)
		}
		if _, err := DecodePR([]byte(body)); !errors.Is(err, ErrResponse) {
			t.Errorf("%s: %v, want ErrResponse", name, err)
		}
	}
}

// TestEvaluateUnreadFactsRefuse pins that a fact the reads could not gather
// (too many directories to read modes from, say) refuses.
func TestEvaluateUnreadFactsRefuse(t *testing.T) {
	f := passingFacts(t)
	f.Unread = []string{"file modes: the changes span 101 directory listings, more than the 100 one read makes"}
	wantRefusal(t, evalManual(f), "not read: file modes: the changes span 101 directory listings")
}
