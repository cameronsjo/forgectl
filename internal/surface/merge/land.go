package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

// The pure half of a merge (atelier P4, T10.4): the subject forgectl takes
// from the PR title, the body it writes, the policy hash, the evidence it
// records, and what changed between the verdict and the merge. `surface
// merge` and the drain's autopilot both use these; the reads and the merge
// call are in internal/cli.

// subjectPattern is the only PR title shape a merge takes as its subject: a
// conventional commit type from a fixed set, an optional lowercase scope, no
// "!", and up to 72 printable ASCII characters.
var subjectPattern = regexp.MustCompile(`^(fix|feat|docs|refactor|test|chore)(\([a-z0-9-]+\))?: [ -~]{1,72}$`)

// subjectIssueRef is an issue reference a subject may not hold: GitHub reads
// "fix: #12" or "fix(x): closes #12" in a commit on the default branch as
// closing #12, and a subject is the one piece of PR text a merge copies.
var subjectIssueRef = regexp.MustCompile(`(?i)#[0-9]|\bgh-[0-9]`)

// ErrSubject reports a PR title a merge will not take as its subject.
var ErrSubject = fmt.Errorf("merge: the PR title is not a subject forgectl merges with")

// Subject returns the squash commit's subject: the PR title, only when it
// matches subjectPattern and holds no issue reference.
func Subject(title string) (string, error) {
	if !subjectPattern.MatchString(title) {
		return "", fmt.Errorf("%w: %q does not match %s (a type of fix, feat, docs, refactor, test or chore, an optional lowercase scope, no \"!\", \": \", then 1-72 printable ASCII characters)",
			ErrSubject, title, subjectPattern.String())
	}
	if subjectIssueRef.MatchString(title) {
		return "", fmt.Errorf("%w: %q holds an issue reference (#N or GH-N), which a commit on the default branch could read as closing it", ErrSubject, title)
	}
	return title, nil
}

// By names who merges: the CLI or the drain's autopilot.
type By string

const (
	// ByCLI is `forgectl surface merge`.
	ByCLI By = "cli"
	// ByDrain is the drain's autopilot step.
	ByDrain By = "drain"
)

// Trailer is the body's last line.
func (b By) Trailer() string { return "Merged-By: forgectl-" + string(b) }

// CheckSeen is one required-check run the verdict counted or judged.
type CheckSeen struct {
	Name       string `json:"name"`
	RunID      int64  `json:"run_id"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Evidence is one required reviewer's latest marker.
type Evidence struct {
	Reviewer string `json:"reviewer"`
	URL      string `json:"url"`
	Head     string `json:"head"`
	Crit     int    `json:"crit"`
	Imp      int    `json:"imp"`
}

// ChecksSeen lists the runs of each required check at the head that GitHub
// ties to the PR, in the repository's required-check order, then by run id.
func ChecksSeen(f Facts, s config.MergeSettings) []CheckSeen {
	repo, ok := s.Repo(f.Row.GitHubRepo)
	if !ok {
		return []CheckSeen{}
	}
	out := []CheckSeen{}
	for _, name := range repo.RequiredChecks {
		var runs []CheckSeen
		for _, r := range f.Checks {
			if r.Name != name || r.SuiteBranch != f.PR.HeadRefName || !slices.Contains(r.SuitePRs, f.PR.Number) {
				continue
			}
			runs = append(runs, CheckSeen{Name: r.Name, RunID: r.DatabaseID, Event: r.Event, Status: r.Status, Conclusion: r.Conclusion})
		}
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].RunID < runs[j].RunID })
		out = append(out, runs...)
	}
	return out
}

// MarkerEvidence lists each required reviewer's latest strict marker by
// marker_author_id, in required_reviewers order. A reviewer with none is
// left out.
func MarkerEvidence(f Facts, s config.MergeSettings) []Evidence {
	out := []Evidence{}
	byReviewer := markersOf(f, s)
	for _, name := range s.RequiredReviewers {
		ms := byReviewer[name]
		if len(ms) == 0 {
			continue
		}
		last := latestMarker(ms)
		out = append(out, Evidence{Reviewer: name, URL: f.Reviews[last.index].URL, Head: last.Head, Crit: last.Crit, Imp: last.Imp})
	}
	return out
}

// PolicyHash is the SHA-256 of the canonical JSON of the resolved policy
// (OffReason left out; repositories in their configured order, names
// lowercased), so an audit line names the policy it was decided under.
func PolicyHash(s config.MergeSettings) string {
	type repo struct {
		Name           string   `json:"name"`
		Workflow       string   `json:"workflow"`
		RequiredChecks []string `json:"required_checks"`
		Paths          []string `json:"paths"`
	}
	canon := struct {
		Mode              string   `json:"mode"`
		Machine           string   `json:"machine"`
		Approvers         []string `json:"approvers"`
		MarkerAuthorID    int64    `json:"marker_author_id"`
		RequiredReviewers []string `json:"required_reviewers"`
		Method            string   `json:"method"`
		Repos             []repo   `json:"repos"`
	}{Mode: string(s.Mode), Machine: s.Machine, Approvers: nonNil(s.Approvers), MarkerAuthorID: s.MarkerAuthorID,
		RequiredReviewers: nonNil(s.RequiredReviewers), Method: s.Method, Repos: []repo{}}
	for _, r := range s.Repos {
		canon.Repos = append(canon.Repos, repo{Name: strings.ToLower(r.Name), Workflow: r.Workflow, RequiredChecks: nonNil(r.RequiredChecks), Paths: nonNil(r.Paths)})
	}
	// termsafe:allow-raw-json hashed only, never written to a terminal
	data, err := json.Marshal(canon)
	if err != nil {
		// A struct of strings and ints always encodes; a failure is a bug,
		// and a hash that names no policy must not match any real one.
		return "unencodable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// githubURL is a URL a merge body may carry: github.com, and only characters
// a review or PR link holds.
var githubURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._/-]+(#[A-Za-z0-9_-]+)?$`)

// BodyInput is what Body writes: nothing in it is PR text.
type BodyInput struct {
	By         By
	AuditHash  string
	PolicyHash string
	PRURL      string
	Head       string
	Checks     []CheckSeen
	Markers    []Evidence
}

// Body is the squash commit's body, written by forgectl alone: the audit
// line's hash, the policy hash, the head, the checks and markers counted,
// and the Merged-By trailer as the last paragraph. No PR text is copied, so
// no closing keyword, BREAKING CHANGE or Release-As line reaches the default
// branch through it. A URL that is not a plain github.com link is left out.
func Body(in BodyInput) string {
	var b strings.Builder
	b.WriteString("Merged by forgectl under the [surface.merge] policy (ADR-0011).\n\n")
	fmt.Fprintf(&b, "Audit-line: sha256:%s\n", in.AuditHash)
	fmt.Fprintf(&b, "Policy: sha256:%s\n", in.PolicyHash)
	fmt.Fprintf(&b, "Head: %s\n", in.Head)
	if githubURL.MatchString(in.PRURL) {
		fmt.Fprintf(&b, "Pull-request: %s\n", in.PRURL)
	}
	for _, c := range in.Checks {
		fmt.Fprintf(&b, "Check: %s run %d %s %s/%s\n", c.Name, c.RunID, c.Event, c.Status, c.Conclusion)
	}
	for _, m := range in.Markers {
		url := m.URL
		if !githubURL.MatchString(url) {
			url = "(link withheld)"
		}
		fmt.Fprintf(&b, "Review: %s head=%s crit=%d imp=%d %s\n", m.Reviewer, m.Head, m.Crit, m.Imp, url)
	}
	b.WriteString("\n" + in.By.Trailer() + "\n")
	return b.String()
}

// Moved compares the facts a verdict passed on with a fresh PR read made
// just before the merge, and names every change: the head, the base branch,
// the draft flag, the state, or any review, conversation comment or inline
// review comment (added, removed or edited). Any change aborts the merge.
func Moved(before Facts, after PRRead) []string {
	var why []string
	a, b := before.PR, after.PR
	if after.Repository.DatabaseID != before.Repository.DatabaseID {
		why = append(why, fmt.Sprintf("the repository id is %d, it was %d", after.Repository.DatabaseID, before.Repository.DatabaseID))
	}
	if b.Number != a.Number {
		why = append(why, fmt.Sprintf("the PR read is #%d, the verdict was for #%d", b.Number, a.Number))
	}
	if b.HeadRefOid != a.HeadRefOid {
		why = append(why, fmt.Sprintf("the head is %s, it was %s", short(b.HeadRefOid), short(a.HeadRefOid)))
	}
	if b.BaseRefName != a.BaseRefName {
		why = append(why, fmt.Sprintf("the base branch is %q, it was %q", b.BaseRefName, a.BaseRefName))
	}
	if b.IsDraft != a.IsDraft {
		why = append(why, fmt.Sprintf("the draft flag is %v, it was %v", b.IsDraft, a.IsDraft))
	}
	if b.State != a.State {
		why = append(why, fmt.Sprintf("the state is %s, it was %s", b.State, a.State))
	}
	if !slices.Equal(after.Reviews, before.Reviews) {
		why = append(why, fmt.Sprintf("the reviews changed (%d now, %d before)", len(after.Reviews), len(before.Reviews)))
	}
	if !slices.Equal(after.Comments, before.Comments) {
		why = append(why, fmt.Sprintf("the conversation comments changed (%d now, %d before)", len(after.Comments), len(before.Comments)))
	}
	if !slices.Equal(after.ReviewComments, before.ReviewComments) {
		why = append(why, fmt.Sprintf("the inline review comments changed (%d now, %d before)", len(after.ReviewComments), len(before.ReviewComments)))
	}
	return why
}
