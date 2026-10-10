package merge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestSubject(t *testing.T) {
	for _, ok := range []string{
		"fix(upgrade): say when brew's update lock is held",
		"feat: add a thing",
		"docs(herdr): merge and audit",
		"refactor(surface-merge): split reads",
		"test: cover the closers",
		"chore: bump",
		"fix: " + strings.Repeat("x", 72),
		"fix: skip the ci step when offline",
		"docs: describe [skip] markers",
	} {
		if got, err := Subject(ok); err != nil || got != ok {
			t.Errorf("Subject(%q) = %q, %v; want it back", ok, got, err)
		}
	}
	for name, bad := range map[string]string{
		"breaking bang":         "feat!: drop the flag",
		"breaking bang, scoped": "feat(x)!: drop the flag",
		"unknown type":          "perf: faster",
		"ci type":               "ci: change the workflow",
		"uppercase scope":       "fix(Upgrade): x",
		"scope with a slash":    "fix(a/b): x",
		"no space":              "fix:x",
		"empty description":     "fix: ",
		"73 characters":         "fix: " + strings.Repeat("x", 73),
		"non-ASCII":             "fix: café",
		"a newline":             "fix: one\nBREAKING CHANGE: two",
		"a tab":                 "fix: one\ttwo",
		"no type":               "Say when the lock is held",
		"a closing reference":   "fix: closes #12",
		"a bare reference":      "fix: #12 crash on start",
		"a GH- reference":       "fix: resolve GH-12",
		"a cross-repo ref":      "fix: fixes cameronsjo/forgectl#12",
		"a leading space":       " fix: x",
	} {
		if _, err := Subject(bad); !errors.Is(err, ErrSubject) {
			t.Errorf("%s: Subject(%q) = %v, want ErrSubject", name, bad, err)
		}
	}
	// Titles that pass the shape but hold a link or a CI-skip directive
	// refuse for that reason (T10.4 security review).
	for _, c := range []struct{ title, want string }{
		{"fix: closes https://github.com/cameronsjo/forgectl/issues/12", "mentions github.com"},
		{"fix: see github.com/cameronsjo/forgectl/pull/12", "mentions github.com"},
		{"fix: see www.github.com/o/r/pulls/3", "mentions github.com"},
		{"fix: closes HTTP://GITHUB.COM/O/R/ISSUES/1", "mentions github.com"},
		{"fix: closes github.com:443/o/r/issues/1", "mentions github.com"},
		{"fix: closes github.com/o/r/issues/%31%32", "mentions github.com"},
		{"docs: link the github.com docs", "mentions github.com"},
		{"docs: fix typo [skip ci]", "CI-skip directive"},
		{"docs: fix typo [ci skip]", "CI-skip directive"},
		{"docs: fix typo [no ci]", "CI-skip directive"},
		{"docs: fix typo [skip actions]", "CI-skip directive"},
		{"docs: fix typo [actions skip]", "CI-skip directive"},
		{"docs: fix typo [ Skip  CI ]", "CI-skip directive"},
		{"docs: fix typo skip-checks: true", "CI-skip directive"},
	} {
		if _, err := Subject(c.title); !errors.Is(err, ErrSubject) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Subject(%q) = %v, want ErrSubject naming %q", c.title, err, c.want)
		}
	}
}

func TestBodyCarriesNoPRText(t *testing.T) {
	f := passingFacts(t)
	s := goodSettings()
	in := BodyInput{By: ByDrain, AuditHash: strings.Repeat("a", 64), PolicyHash: PolicyHash(s), PRURL: f.PR.URL, Head: f.PR.HeadRefOid,
		Checks: ChecksSeen(f, s), Markers: MarkerEvidence(f, s)}
	b := Body(in)
	for _, want := range []string{
		"Audit-line: sha256:" + strings.Repeat("a", 64) + "\n",
		"Policy: sha256:" + PolicyHash(s) + "\n",
		"Head: " + head1204 + "\n",
		"Pull-request: https://github.com/cameronsjo/forgectl/pull/1204\n",
		"Check: build-test run ",
		"Review: polish head=" + head1204 + " crit=0 imp=0 ",
		"Review: cadence-forge-security-reviewer head=" + head1204,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("body lacks %q:\n%s", want, b)
		}
	}
	if !strings.HasSuffix(b, "\n\nMerged-By: forgectl-drain\n") {
		t.Fatalf("the trailer is not the last paragraph:\n%s", b)
	}
	if !strings.HasSuffix(Body(BodyInput{By: ByCLI}), "\n\nMerged-By: forgectl-cli\n") {
		t.Fatal("the cli trailer")
	}
	// The title is PR text: it never reaches the body.
	if strings.Contains(b, f.PR.Title) {
		t.Fatalf("the body copies the PR title:\n%s", b)
	}
	// A link that is not a plain github.com URL is withheld.
	in.PRURL = "https://evil.example/x\nCloses #1"
	in.Markers = []Evidence{{Reviewer: "polish", URL: "https://github.com/x\nFixes #2", Head: head1204}}
	b = Body(in)
	if strings.Contains(b, "evil") || strings.Contains(b, "Closes") || strings.Contains(b, "Fixes") || !strings.Contains(b, "(link withheld)") {
		t.Fatalf("an unsafe link reached the body:\n%s", b)
	}
}

func TestPolicyHash(t *testing.T) {
	a := goodSettings()
	b := goodSettings()
	b.OffReason = "ignored"
	b.Repos[0].Name = "CameronSjo/Forgectl"
	if PolicyHash(a) != PolicyHash(b) {
		t.Fatal("OffReason or a repository name's case changed the hash")
	}
	changes := map[string]func(s *config.MergeSettings){
		"mode":      func(s *config.MergeSettings) { s.Mode = "auto" },
		"reviewers": func(s *config.MergeSettings) { s.RequiredReviewers = s.RequiredReviewers[:1] },
		"paths":     func(s *config.MergeSettings) { s.Repos[0].Paths = []string{"docs/**"} },
		"checks":    func(s *config.MergeSettings) { s.Repos[0].RequiredChecks = []string{"lint"} },
		"author":    func(s *config.MergeSettings) { s.MarkerAuthorID = 1 },
	}
	for name, mutate := range changes {
		c := goodSettings()
		mutate(&c)
		if PolicyHash(c) == PolicyHash(a) {
			t.Errorf("%s: the hash did not change", name)
		}
	}
	if len(PolicyHash(a)) != 64 {
		t.Fatalf("hash %q", PolicyHash(a))
	}
}

func TestMoved(t *testing.T) {
	f := passingFacts(t)
	same := PRRead{Repository: f.Repository, PR: f.PR, Reviews: f.Reviews, Comments: f.Comments, ReviewComments: f.ReviewComments}
	if why := Moved(f, same); len(why) != 0 {
		t.Fatalf("an unchanged read: %q", why)
	}
	cases := map[string]struct {
		mutate func(*PRRead)
		want   string
	}{
		"head":         {func(p *PRRead) { p.PR.HeadRefOid = base1204 }, "the head is a398e7258d0a, it was 3afe70e8bff8"},
		"base branch":  {func(p *PRRead) { p.PR.BaseRefName = "release" }, `the base branch is "release"`},
		"draft":        {func(p *PRRead) { p.PR.IsDraft = true }, "the draft flag is true"},
		"state":        {func(p *PRRead) { p.PR.State = "CLOSED" }, "the state is CLOSED, it was OPEN"},
		"a new review": {func(p *PRRead) { p.Reviews = append(p.Reviews, Review{Body: "x"}) }, "the reviews changed"},
		"an edited review": {func(p *PRRead) {
			p.Reviews = append([]Review(nil), p.Reviews...)
			p.Reviews[0].LastEditedAt = "2026-10-09T20:00:00Z"
		}, "the reviews changed"},
		"a new comment":        {func(p *PRRead) { p.Comments = append(p.Comments, Comment{Body: "x"}) }, "the conversation comments changed"},
		"a new inline comment": {func(p *PRRead) { p.ReviewComments = append(p.ReviewComments, Comment{Body: "x"}) }, "the inline review comments changed"},
		"another repository":   {func(p *PRRead) { p.Repository.DatabaseID = 7 }, "the repository id is 7"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := same
			p.Reviews = append([]Review(nil), same.Reviews...)
			c.mutate(&p)
			why := Moved(f, p)
			if !strings.Contains(strings.Join(why, "; "), c.want) {
				t.Fatalf("%q; want one naming %q", why, c.want)
			}
		})
	}
}

// TestRecheckAndLanded drives the two merge-time reads against the fixtures.
func TestRecheckAndLanded(t *testing.T) {
	ctx := context.Background()
	snap, err := Reader{GH: fixtureGH{t: t}.runner()}.Read(ctx, row1204())
	if err != nil {
		t.Fatal(err)
	}
	required := []string{"build-test", "lint", "macos-test"}
	why, err := Reader{GH: fixtureGH{t: t}.runner()}.Recheck(ctx, snap.Facts, required)
	if err != nil || len(why) != 0 {
		t.Fatalf("recheck of the same fixture: %q, %v", why, err)
	}
	// A required check re-run at the head between the verdict and the merge
	// (T10.4 security review): running again, or failed, refuses.
	checks := string(readFixture(t, "checks_1204.json"))
	for name, edit := range map[string]func(string) string{
		"re-run, in progress": func(c string) string {
			return strings.Replace(c, `"name": "lint",
          "status": "COMPLETED",
          "conclusion": "SUCCESS"`, `"name": "lint",
          "status": "IN_PROGRESS",
          "conclusion": null`, 1)
		},
		"failed": func(c string) string {
			return strings.Replace(c, `"name": "build-test",
          "status": "COMPLETED",
          "conclusion": "SUCCESS"`, `"name": "build-test",
          "status": "COMPLETED",
          "conclusion": "FAILURE"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := edit(checks)
			if changed == checks {
				t.Fatal("the fixture edit did not apply")
			}
			why, err := Reader{GH: fixtureGH{t: t, override: map[string]string{"checkSuites(first: 50)": changed}}.runner()}.Recheck(ctx, snap.Facts, required)
			if err != nil || len(why) != 1 || !strings.Contains(why[0], "the runs of required check") {
				t.Fatalf("recheck after a check changed: %q, %v", why, err)
			}
		})
	}
	// A run of a check outside the required set may change.
	unrequired := strings.Replace(checks, `"name": "govulncheck",
          "status": "COMPLETED",
          "conclusion": "SUCCESS"`, `"name": "govulncheck",
          "status": "COMPLETED",
          "conclusion": "FAILURE"`, 1)
	if unrequired == checks {
		t.Fatal("the govulncheck edit did not apply")
	}
	if why, err := (Reader{GH: fixtureGH{t: t, override: map[string]string{"checkSuites(first: 50)": unrequired}}.runner()}).Recheck(ctx, snap.Facts, required); err != nil || len(why) != 0 {
		t.Fatalf("a change outside the required checks: %q, %v", why, err)
	}
	if _, err := (Reader{GH: fixtureGH{t: t, failOn: "checkSuites(first"}.runner()}).Recheck(ctx, snap.Facts, required); !errors.Is(err, ErrRead) {
		t.Fatalf("a failed checks re-read: %v, want ErrRead", err)
	}
	moved := strings.Replace(string(readFixture(t, "pr_1204.json")), `"isDraft": false`, `"isDraft": true`, 1)
	why, err = Reader{GH: fixtureGH{t: t, override: map[string]string{"reviews(first: 100)": moved}}.runner()}.Recheck(ctx, snap.Facts, required)
	if err != nil || len(why) != 1 || !strings.Contains(why[0], "draft") {
		t.Fatalf("recheck of a PR turned draft: %q, %v", why, err)
	}
	if _, err := (Reader{GH: fixtureGH{t: t, failOn: "reviews(first"}.runner()}).Recheck(ctx, snap.Facts, required); !errors.Is(err, ErrRead) {
		t.Fatalf("a failed recheck: %v, want ErrRead", err)
	}

	const mergeCommit = "1111111111111111111111111111111111111111"
	const mainHead = "2222222222222222222222222222222222222222"
	landed := `{"data":{"repository":{"databaseId":1252924951,"nameWithOwner":"cameronsjo/forgectl","defaultBranchRef":{"name":"main","target":{"oid":"` + mainHead + `"}},
	  "pullRequest":{"number":1204,"state":"MERGED","merged":true,"headRefOid":"` + head1204 + `","mergeCommit":{"oid":"` + mergeCommit + `","message":"fix: x\n\nAudit-line: sha256:abc\n"}}}}}`
	gh := fixtureGH{t: t, override: map[string]string{"mergeCommit { oid message }": landed, "compare/" + mergeCommit + "..." + mainHead: `{"status":"ahead"}`}}
	l, err := Reader{GH: gh.runner()}.Landed(ctx, snap.Facts)
	if err != nil || !l.Merged || !l.OnDefault || l.MergeCommit != mergeCommit || l.DefaultBranch != "main" || l.MergeMessage != "fix: x\n\nAudit-line: sha256:abc\n" {
		t.Fatalf("landed: %+v, %v", l, err)
	}
	gh.override["compare/"+mergeCommit+"..."+mainHead] = `{"status":"diverged"}`
	if l, err := (Reader{GH: gh.runner()}).Landed(ctx, snap.Facts); err != nil || l.OnDefault || l.Ancestry != "diverged" {
		t.Fatalf("a merge commit off the default branch: %+v, %v", l, err)
	}
	open := strings.Replace(strings.Replace(landed, `"MERGED","merged":true`, `"OPEN","merged":false`, 1), `{"oid":"`+mergeCommit+`","message":"fix: x\n\nAudit-line: sha256:abc\n"}`, "null", 1)
	if l, err := (Reader{GH: fixtureGH{t: t, override: map[string]string{"mergeCommit { oid message }": open}}.runner()}).Landed(ctx, snap.Facts); err != nil || l.Merged || l.OnDefault {
		t.Fatalf("an unmerged PR: %+v, %v", l, err)
	}
	other := strings.Replace(landed, `"databaseId":1252924951`, `"databaseId":7`, 1)
	if _, err := (Reader{GH: fixtureGH{t: t, override: map[string]string{"mergeCommit { oid message }": other}}.runner()}).Landed(ctx, snap.Facts); !errors.Is(err, ErrRead) {
		t.Fatalf("another repository: %v, want ErrRead", err)
	}
}
