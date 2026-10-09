package merge

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// GitHub identities the policy matches by numeric id, never by login.
const (
	// CodeRabbitUserID is coderabbitai[bot]'s user id.
	CodeRabbitUserID int64 = 136622811
	// GitHubActionsAppID is the GitHub Actions app's id; a required check
	// run must come from it.
	GitHubActionsAppID int64 = 15368
)

// Result is a verdict's outcome.
type Result string

const (
	// Pass: every predicate holds at the head.
	Pass Result = "pass"
	// Refuse: at least one predicate does not; Reasons says which.
	Refuse Result = "refuse"
	// Off: the policy is off (mode off, or the caller needs auto and the
	// mode is manual); nothing was evaluated.
	Off Result = "off"
)

// Verdict is Evaluate's answer. Reasons are readable sentences, each naming
// the expected and the seen value; a pass has none.
type Verdict struct {
	Result  Result   `json:"verdict"`
	Reasons []string `json:"reasons"`
}

// Actor is a GitHub actor as GraphQL gives it. DatabaseID is 0 when the
// response carried none (a deleted account, a mannequin).
type Actor struct {
	Typename   string `json:"__typename"`
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
}

// Row is the worker's ledger row and queue state, as recorded at launch.
type Row struct {
	Name       string
	Branch     string
	BranchFrom string
	LaunchID   string
	// Stage is the ledger row's stage.
	Stage string
	// Base is the commit the worker's branch started at.
	Base         string
	GitHubRepo   string
	GitHubRepoID int64
	// QueueLaunchID and QueueState are the queue row of the same name: the
	// drain's claim. Both are empty for a CLI launch with no queue row.
	QueueLaunchID string
	QueueState    string
}

// Repository is the row's repository as GitHub names it now.
type Repository struct {
	NameWithOwner string
	DatabaseID    int64
	DefaultBranch string
}

// PullRequest is the PR's state, read in one query bound to HeadRefOid.
type PullRequest struct {
	Number int
	URL    string
	Title  string
	// Body is the PR description; only the @coderabbitai mention rule
	// reads it.
	Body              string
	State             string
	IsDraft           bool
	IsCrossRepository bool
	HeadRepoID        int64
	BaseRepoID        int64
	Author            Actor
	BaseRefName       string
	BaseRefOid        string
	HeadRefName       string
	HeadRefOid        string
	Mergeable         string
	MergeStateStatus  string
	ChangedFiles      int
}

// CheckRun is one check run at the head, with the suite it belongs to.
type CheckRun struct {
	DatabaseID int64
	Name       string
	Status     string
	Conclusion string
	StartedAt  string
	// AppID is the suite's app.
	AppID int64
	// HasWorkflowRun is false for a run whose suite has no workflow run
	// (a third-party app's run).
	HasWorkflowRun bool
	// WorkflowPath is the workflow's resourcePath
	// ("/<owner>/<repo>/actions/workflows/<file>"), and Event the run's
	// trigger.
	WorkflowPath string
	Event        string
	// SuiteBranch is the suite's head branch, and SuitePRs the open PRs
	// GitHub matches the suite to (its matchingPullRequests). A run counts
	// for a PR only when both name it.
	SuiteBranch string
	SuitePRs    []int
}

// Review is one pull request review.
type Review struct {
	Author      Actor
	State       string
	SubmittedAt string
	URL         string
	Body        string
	// CommitOID is the commit the review was submitted at.
	CommitOID string
}

// Comment is a PR or review-thread comment.
type Comment struct {
	Author Actor
	Body   string
}

// Thread is a review thread.
type Thread struct {
	IsResolved bool
	// ResolvedBy is nil when no one, or no readable account, resolved it.
	ResolvedBy *Actor
	Comments   []Comment
}

// File is one changed file, from the compare of base and head, with its
// modes from the git trees at base (for PreviousPath on a rename) and head.
// A mode is "" where the file does not exist on that side.
type File struct {
	Path         string
	PreviousPath string
	Status       string
	BaseMode     string
	HeadMode     string
}

// Ancestry statuses, as GitHub's compare API reports them.
const (
	CompareAhead     = "ahead"
	CompareIdentical = "identical"
	// CompareNotFound is not GitHub's: the compare answered 404, so one of
	// its commits is not on GitHub.
	CompareNotFound = "not-found"
)

// Facts is everything read for one worker's PR at one head SHA.
type Facts struct {
	Row        Row
	Repository Repository
	// OperatorID is the numeric id of the account gh is authenticated as.
	OperatorID int64
	PR         PullRequest
	Checks     []CheckRun
	Reviews    []Review
	Threads    []Thread
	// Comments are the PR's conversation comments.
	Comments []Comment
	Files    []File
	// BaseAncestry is compare(Row.Base...PR.BaseRefOid).status, and
	// HeadAncestry compare(Row.Base...PR.HeadRefOid).status.
	BaseAncestry string
	HeadAncestry string
	// Unread names facts the reads could not gather (too many directories
	// to read modes from, say). Each one refuses.
	Unread []string
}

// Policy is the resolved [surface.merge] the verdict is for. ForDrain is the
// drain's autopilot asking, which needs mode auto; status and `surface
// merge` need manual or auto.
type Policy struct {
	Settings config.MergeSettings
	ForDrain bool
}

// markerPattern is a cadence-review marker: the whole first line of a
// review body, no byte-order mark, no leading space.
var markerPattern = regexp.MustCompile(`^<!-- cadence-review: ([a-z0-9-]{1,40}) head=([0-9a-f]{40}) crit=(0|[1-9][0-9]{0,3}) imp=(0|[1-9][0-9]{0,3}) -->$`)

// Marker is a parsed cadence-review marker.
type Marker struct {
	Reviewer string
	Head     string
	Crit     int
	Imp      int
}

// ParseMarker reads a marker from a review body's first line.
func ParseMarker(body string) (Marker, bool) {
	line, _, _ := strings.Cut(body, "\n")
	m := markerPattern.FindStringSubmatch(line)
	if m == nil {
		return Marker{}, false
	}
	crit, err1 := strconv.Atoi(m[3])
	imp, err2 := strconv.Atoi(m[4])
	if err1 != nil || err2 != nil {
		return Marker{}, false
	}
	return Marker{Reviewer: m[1], Head: m[2], Crit: crit, Imp: imp}, true
}

// Evaluate is the merge policy: pass only when every predicate holds at
// one head SHA. It is pure; the caller reads Facts fresh. Every failing
// predicate adds a reason, so one verdict shows all of them.
func Evaluate(f Facts, p Policy) Verdict {
	s := p.Settings
	switch {
	case s.Mode == config.MergeOff:
		reason := s.OffReason
		if reason == "" {
			reason = "the merge policy is off"
		}
		return Verdict{Result: Off, Reasons: []string{reason}}
	case p.ForDrain && s.Mode != config.MergeAuto:
		return Verdict{Result: Off, Reasons: []string{fmt.Sprintf("the drain merges only with [surface.merge] mode \"auto\"; the mode is %q", s.Mode)}}
	case s.Mode != config.MergeManual && s.Mode != config.MergeAuto:
		return Verdict{Result: Off, Reasons: []string{fmt.Sprintf("[surface.merge] mode is %q, expected manual or auto", s.Mode)}}
	}
	var reasons []string
	add := func(format string, a ...any) { reasons = append(reasons, fmt.Sprintf(format, a...)) }
	repo, repoOK := checkRepository(f, s, add)
	checkRow(f, add)
	checkPR(f, add)
	if repoOK {
		checkChecks(f, repo, add)
		checkPaths(f, repo, add)
	}
	checkApprovers(f, s, add)
	checkOpenFindings(f, s, add)
	for _, u := range f.Unread {
		add("not read: %s", u)
	}
	if len(reasons) > 0 {
		return Verdict{Result: Refuse, Reasons: reasons}
	}
	return Verdict{Result: Pass, Reasons: []string{}}
}

type addFunc func(format string, a ...any)

// checkRepository is predicate 2. It returns the repository's policy entry
// when there is one.
func checkRepository(f Facts, s config.MergeSettings, add addFunc) (config.MergeRepo, bool) {
	row, live := f.Row, f.Repository
	if row.GitHubRepo == "" || row.GitHubRepoID == 0 {
		add("the worker's ledger row records no GitHub repository (expected the nameWithOwner and id read at launch; the launch predates the record, or origin is not on github.com)")
		return config.MergeRepo{}, false
	}
	ok := true
	if live.DatabaseID != row.GitHubRepoID {
		add("the repository's id on GitHub is %d, expected %d, the id recorded at launch", live.DatabaseID, row.GitHubRepoID)
		ok = false
	}
	if !strings.EqualFold(live.NameWithOwner, row.GitHubRepo) {
		add("the repository is %q on GitHub now, expected %q, the name recorded at launch (renamed or transferred since)", live.NameWithOwner, row.GitHubRepo)
		ok = false
	}
	for _, name := range []string{row.GitHubRepo, live.NameWithOwner} {
		if r, hit := refusedRepoFor(name, row.GitHubRepoID); hit {
			add("%s is never merged by forgectl, whatever the config says: it installs live from its default branch or holds machine configuration (built-in refusal %s, id %d)", row.GitHubRepo, r.name, r.id)
			return config.MergeRepo{}, false
		}
	}
	if r, hit := refusedRepoFor("", live.DatabaseID); hit {
		add("%s is never merged by forgectl, whatever the config says (built-in refusal %s, id %d)", live.NameWithOwner, r.name, r.id)
		return config.MergeRepo{}, false
	}
	entry, listed := s.Repo(row.GitHubRepo)
	if !listed {
		names := make([]string, 0, len(s.Repos))
		for _, r := range s.Repos {
			names = append(names, r.Name)
		}
		add("%s is not on [surface.merge] repos (expected one of %s)", row.GitHubRepo, quoteList(names))
		return config.MergeRepo{}, false
	}
	return entry, ok
}

// checkRow is predicate 3.
func checkRow(f Facts, add addFunc) {
	row := f.Row
	switch {
	case row.LaunchID == "":
		add("the worker's ledger row has no launch_id, expected the drain's claim (a launch by hand is never merged by policy)")
	case row.LaunchID != row.QueueLaunchID:
		add("the worker's ledger row has launch_id %q, expected %q, the queue row's claim", row.LaunchID, row.QueueLaunchID)
	}
	if want := "worker/" + row.Name; row.Branch != want {
		add("the worker's branch is %q, expected %q", row.Branch, want)
	}
	if row.BranchFrom != "new" {
		add("the worker's branch came from %q, expected \"new\": only a branch the drain created is merged", nonEmpty(row.BranchFrom, "nothing recorded"))
	}
	if row.Stage != "launched" {
		add("the worker's ledger stage is %q, expected \"launched\"", row.Stage)
	}
	if row.QueueState != "launched" && row.QueueState != "reported" {
		add("the worker's queue state is %q, expected \"launched\" or \"reported\"", nonEmpty(row.QueueState, "no queue row"))
	}
	if !isSHA(row.Base) {
		add("the worker's recorded base is %q, expected a 40-character commit id", row.Base)
	}
	if f.PR.HeadRefName != row.Branch {
		add("the PR's head branch is %q, expected the worker's %q", f.PR.HeadRefName, row.Branch)
	}
	if f.BaseAncestry == CompareNotFound || f.HeadAncestry == CompareNotFound {
		add("the worker's recorded base %s is not on GitHub (the compare answered 404), expected a commit the PR's base and head descend from", short(row.Base))
		return
	}
	if f.BaseAncestry != CompareAhead && f.BaseAncestry != CompareIdentical {
		add("the worker's base %s is not an ancestor of the PR's base %s (compare status %q, expected \"ahead\" or \"identical\")", short(row.Base), short(f.PR.BaseRefOid), f.BaseAncestry)
	}
	if f.HeadAncestry != CompareAhead && f.HeadAncestry != CompareIdentical {
		add("the PR's head %s does not descend from the worker's base %s (compare status %q, expected \"ahead\" or \"identical\")", short(f.PR.HeadRefOid), short(row.Base), f.HeadAncestry)
	}
}

// checkPR is predicate 4, with the discovery filter repeated.
func checkPR(f Facts, add addFunc) {
	pr := f.PR
	if pr.State != "OPEN" {
		add("the PR is %s, expected OPEN", pr.State)
	}
	if pr.IsDraft {
		add("the PR is a draft, expected ready for review")
	}
	if pr.BaseRefName != f.Repository.DefaultBranch || pr.BaseRefName == "" {
		add("the PR targets %q, expected the default branch %q", pr.BaseRefName, f.Repository.DefaultBranch)
	}
	if pr.Mergeable != "MERGEABLE" {
		add("the PR's mergeable state is %s, expected MERGEABLE", nonEmpty(pr.Mergeable, "unknown"))
	}
	if pr.MergeStateStatus != "CLEAN" && pr.MergeStateStatus != "HAS_HOOKS" {
		add("the PR's merge state is %s, expected CLEAN or HAS_HOOKS", nonEmpty(pr.MergeStateStatus, "unknown"))
	}
	if pr.IsCrossRepository || pr.HeadRepoID != f.Repository.DatabaseID || pr.BaseRepoID != f.Repository.DatabaseID {
		add("the PR's head repository is %d (cross-repository %v), expected the base repository %d", pr.HeadRepoID, pr.IsCrossRepository, f.Repository.DatabaseID)
	}
	if f.OperatorID == 0 || pr.Author.DatabaseID != f.OperatorID {
		add("the PR's author is %s, expected the operator's id %d", who(pr.Author), f.OperatorID)
	}
	if !isSHA(pr.HeadRefOid) {
		add("the PR's head is %q, expected a 40-character commit id", pr.HeadRefOid)
	}
}

// checkChecks is predicate 5. Only runs whose suite GitHub ties to this PR
// count (the suite's head branch is the PR's, and its matching open PRs
// include this one), so a run at the same commit for another PR cannot
// stand in. Of those, for each required name: a run from another app or
// workflow file, or with no workflow run, refuses; a run from the pinned
// file on any other event (push, workflow_dispatch, schedule) never counts
// and refuses unless it is SUCCESS; among the runs from the pinned file on a
// pull_request event, any still running refuses, any that concluded other
// than SUCCESS refuses, and at least one SUCCESS is needed.
func checkChecks(f Facts, repo config.MergeRepo, add addFunc) {
	want := "/" + f.Repository.NameWithOwner + "/actions/workflows/" + path.Base(repo.Workflow)
	tied := func(r CheckRun) bool {
		return r.SuiteBranch != "" && r.SuiteBranch == f.PR.HeadRefName && slices.Contains(r.SuitePRs, f.PR.Number)
	}
	for _, name := range repo.RequiredChecks {
		var pinned []CheckRun
		for _, r := range f.Checks {
			if r.Name != name || !tied(r) {
				continue
			}
			switch {
			case !r.HasWorkflowRun:
				add("check %q has a run (id %d) with no workflow run behind it, expected only runs from %s", name, r.DatabaseID, repo.Workflow)
			case r.AppID != GitHubActionsAppID:
				add("check %q has a run (id %d) from app %d, expected GitHub Actions (%d)", name, r.DatabaseID, r.AppID, GitHubActionsAppID)
			case r.WorkflowPath != want:
				add("check %q has a run (id %d) from workflow %s, expected only %s", name, r.DatabaseID, r.WorkflowPath, want)
			case r.Event == "pull_request":
				pinned = append(pinned, r)
			case r.Status != "COMPLETED" || r.Conclusion != "SUCCESS":
				// A run of the pinned file on another event (push,
				// workflow_dispatch, schedule) never counts, but one that is
				// not SUCCESS still refuses.
				add("check %q has a run (id %d) from %s on event %q that is %s/%s, expected every run of the pinned workflow at the head to be COMPLETED/SUCCESS",
					name, r.DatabaseID, repo.Workflow, r.Event, nonEmpty(r.Status, "unknown"), nonEmpty(r.Conclusion, "none"))
			}
		}
		if len(pinned) == 0 {
			add("check %q has no run at the head tied to PR #%d from %s on a pull_request event", name, f.PR.Number, repo.Workflow)
			continue
		}
		passed := 0
		for _, r := range pinned {
			switch {
			case r.Status != "COMPLETED":
				add("check %q is still running: run id %d is %s, expected COMPLETED/SUCCESS", name, r.DatabaseID, nonEmpty(r.Status, "unknown"))
			case r.Conclusion != "SUCCESS":
				add("check %q has a run (id %d) that concluded %s, expected every run at the head to be SUCCESS", name, r.DatabaseID, nonEmpty(r.Conclusion, "none"))
			default:
				passed++
			}
		}
		if passed == 0 {
			add("check %q has no successful run at the head, expected at least one SUCCESS", name)
		}
	}
}

// allowedStatus are the file statuses the policy judges.
var allowedStatus = []string{"added", "modified", "removed", "renamed"}

// checkPaths is predicate 6.
func checkPaths(f Facts, repo config.MergeRepo, add addFunc) {
	if len(f.Files) != f.PR.ChangedFiles {
		add("the file list has %d entries, expected the PR's changedFiles %d (GitHub's compare lists at most %d files, so a larger PR never passes)", len(f.Files), f.PR.ChangedFiles, MaxCompareFiles)
	}
	if len(f.Files) == 0 {
		add("the PR changes no files, expected at least one")
	}
	for _, file := range f.Files {
		names := []string{file.Path}
		if file.Status == "renamed" {
			names = append(names, file.PreviousPath)
		}
		bad := false
		for _, n := range names {
			if err := config.CheckChangedPath(n); err != nil {
				add("path %q is refused: %v", n, err)
				bad = true
			}
		}
		if bad {
			continue
		}
		if !slices.Contains(allowedStatus, file.Status) {
			add("path %q has status %q, expected added, modified, removed or renamed", file.Path, file.Status)
			continue
		}
		checkModes(file, add)
		removedTest := strings.HasSuffix(file.Path, "_test.go") && file.Status == "removed" ||
			strings.HasSuffix(file.PreviousPath, "_test.go") && file.Status == "renamed" && file.PreviousPath != file.Path
		if removedTest {
			add("path %q removes a test file (%s), which is refused", file.Path, nonEmpty(file.PreviousPath, file.Path))
		}
		for _, n := range names {
			if why := builtinRefusal(n); why != "" {
				add("path %q is refused whatever the config says: %s", n, why)
				continue
			}
			if !slices.ContainsFunc(repo.Paths, func(g string) bool { return config.MatchMergeGlob(g, n) }) {
				add("path %q matches none of [surface.merge.paths] for %s (expected one of %s)", n, repo.Name, quoteList(repo.Paths))
			}
		}
	}
}

// regularFileMode is the only git mode a merged file may have.
const regularFileMode = "100644"

func checkModes(file File, add addFunc) {
	switch file.Status {
	case "added":
		if file.HeadMode != regularFileMode {
			add("path %q is added with mode %s, expected %s", file.Path, nonEmpty(file.HeadMode, "none"), regularFileMode)
		}
	case "removed":
		if file.BaseMode != regularFileMode {
			add("path %q is removed with base mode %s, expected %s", file.Path, nonEmpty(file.BaseMode, "none"), regularFileMode)
		}
	default:
		if file.BaseMode != regularFileMode || file.HeadMode != regularFileMode {
			add("path %q has mode %s at base and %s at head, expected %s at both", file.Path, nonEmpty(file.BaseMode, "none"), nonEmpty(file.HeadMode, "none"), regularFileMode)
		}
	}
}

// checkApprovers is predicate 7: at least one configured approver passes.
// Open cadence-review findings refuse on their own (checkOpenFindings),
// whichever approver passes.
func checkApprovers(f Facts, s config.MergeSettings, add addFunc) {
	if len(s.Approvers) == 0 {
		add("[surface.merge] approvers is empty, expected cadence-review or coderabbit")
		return
	}
	var why []string
	for _, a := range s.Approvers {
		var r []string
		switch a {
		case config.MergeApproverCadenceReview:
			r = cadenceReview(f, s)
		case config.MergeApproverCodeRabbit:
			r = codeRabbit(f)
		default:
			r = []string{fmt.Sprintf("approver %q is not implemented", a)}
		}
		if len(r) == 0 {
			return
		}
		why = append(why, r...)
	}
	add("no approver passed the head: %s", strings.Join(why, "; "))
}

// marker is a parsed marker with the review it came from.
type marker struct {
	Marker
	at     time.Time
	commit string
	index  int
}

// markersOf collects the cadence-review markers by s.MarkerAuthorID, per
// reviewer. A marker review whose submittedAt does not parse is not counted
// and comes back as a reason: it may hold a finding.
func markersOf(f Facts, s config.MergeSettings) (map[string][]marker, []string) {
	var bad []string
	byReviewer := map[string][]marker{}
	for i, r := range f.Reviews {
		if r.Author.Typename != "User" || r.Author.DatabaseID != s.MarkerAuthorID || r.Author.DatabaseID == 0 {
			continue
		}
		if r.State != "COMMENTED" && r.State != "APPROVED" {
			continue
		}
		m, ok := ParseMarker(r.Body)
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			bad = append(bad, fmt.Sprintf("cadence-review: a %s marker has submittedAt %q, expected a timestamp", m.Reviewer, r.SubmittedAt))
			continue
		}
		byReviewer[m.Reviewer] = append(byReviewer[m.Reviewer], marker{Marker: m, at: at, commit: r.CommitOID, index: i})
	}
	return byReviewer, bad
}

// passingAt reports a marker that clears the head: it names head, was posted
// at head, and reports no Critical or Important finding.
func passingAt(m marker, head string) bool {
	return m.Head == head && m.commit == head && m.Crit == 0 && m.Imp == 0
}

// checkOpenFindings refuses a reviewer whose latest marker with a Critical
// or Important finding, at any commit, has no later passing marker at the
// head. It runs under every approver set (ADR-0011, 2026-10-09 amendment,
// decision 1): a passing CodeRabbit review never silences an open finding.
func checkOpenFindings(f Facts, s config.MergeSettings, add addFunc) {
	if s.MarkerAuthorID <= 0 {
		add("[surface.merge] marker_author_id is not set, expected the operator's id: open cadence-review findings cannot be read without it")
		return
	}
	head := f.PR.HeadRefOid
	byReviewer, bad := markersOf(f, s)
	for _, b := range bad {
		add("%s", b)
	}
	reviewers := make([]string, 0, len(byReviewer))
	for name := range byReviewer {
		reviewers = append(reviewers, name)
	}
	sort.Strings(reviewers)
	for _, name := range reviewers {
		ms := byReviewer[name]
		var failedAt time.Time
		var failed *marker
		for i := range ms {
			if (ms[i].Crit > 0 || ms[i].Imp > 0) && (failed == nil || !ms[i].at.Before(failedAt)) {
				failed, failedAt = &ms[i], ms[i].at
			}
		}
		if failed == nil {
			continue
		}
		cleared := slices.ContainsFunc(ms, func(m marker) bool { return passingAt(m, head) && m.at.After(failedAt) })
		if !cleared {
			add("open finding: %s reported crit=%d imp=%d at head %s (%s), expected a later passing marker at head %s; this refuses under every approver",
				name, failed.Crit, failed.Imp, short(failed.Head), failedAt.UTC().Format(time.RFC3339), short(head))
		}
	}
}

// cadenceReview returns why the cadence-review approver does not pass, or
// nothing when it does: each required reviewer's latest marker must pass at
// the head. Open findings from any reviewer are checkOpenFindings'.
func cadenceReview(f Facts, s config.MergeSettings) []string {
	var why []string
	head := f.PR.HeadRefOid
	byReviewer, _ := markersOf(f, s)
	for _, name := range s.RequiredReviewers {
		ms := byReviewer[name]
		if len(ms) == 0 {
			why = append(why, fmt.Sprintf("cadence-review: no %s marker by user %d, expected one at head %s", name, s.MarkerAuthorID, short(head)))
			continue
		}
		last := latestMarker(ms)
		if !passingAt(last, head) {
			why = append(why, fmt.Sprintf("cadence-review: %s's latest marker is head=%s (review commit %s) crit=%d imp=%d, expected head=%s crit=0 imp=0", name, short(last.Head), short(last.commit), last.Crit, last.Imp, short(head)))
		}
	}
	if len(s.RequiredReviewers) == 0 {
		why = append(why, "cadence-review: [surface.merge] required_reviewers is empty, expected at least one reviewer name")
	}
	return why
}

// latestMarker is the marker submitted last; within one second, the later
// review in the list.
func latestMarker(ms []marker) marker {
	last := ms[0]
	for _, m := range ms[1:] {
		if m.at.After(last.at) || (m.at.Equal(last.at) && m.index > last.index) {
			last = m
		}
	}
	return last
}

// codeRabbitStatusLine marks a completed CodeRabbit review body, as the
// captured cameronsjo/forgectl#1195 review carries it.
const codeRabbitStatusLine = "<!-- This is an auto-generated comment by CodeRabbit for review status -->"

var (
	codeRabbitFirstLine = regexp.MustCompile(`^\*\*Actionable comments posted: (0|[1-9][0-9]{0,3})\*\*$`)
	codeRabbitRange     = regexp.MustCompile(`(?m)^Reviewing files that changed from the base of the PR and between [0-9a-f]{40} and ([0-9a-f]{40})\.$`)
)

// CompletedCodeRabbitReview reports whether body has the shape of a
// completed CodeRabbit review of head: the "Actionable comments posted"
// first line, the review-status marker, and the commit range ending at
// head.
func CompletedCodeRabbitReview(body, head string) bool {
	first, _, _ := strings.Cut(body, "\n")
	if !codeRabbitFirstLine.MatchString(first) || !strings.Contains(body, codeRabbitStatusLine) {
		return false
	}
	m := codeRabbitRange.FindAllStringSubmatch(body, -1)
	return len(m) == 1 && m[0][1] == head
}

func isCodeRabbit(a Actor) bool { return a.DatabaseID == CodeRabbitUserID }

// codeRabbit returns why the coderabbit approver does not pass, or nothing
// when it does.
func codeRabbit(f Facts) []string {
	var why []string
	head := f.PR.HeadRefOid
	if !slices.ContainsFunc(f.Reviews, func(r Review) bool {
		return isCodeRabbit(r.Author) && r.Author.Typename == "Bot" && r.CommitOID == head &&
			(r.State == "COMMENTED" || r.State == "APPROVED") && CompletedCodeRabbitReview(r.Body, head)
	}) {
		why = append(why, fmt.Sprintf("coderabbit: no completed review by user %d at head %s (a commit status such as \"Review rate limited\" never counts)", CodeRabbitUserID, short(head)))
	}
	for _, t := range f.Threads {
		if len(t.Comments) == 0 || !isCodeRabbit(t.Comments[0].Author) {
			continue
		}
		switch {
		case !t.IsResolved:
			why = append(why, "coderabbit: a CodeRabbit review thread is unresolved, expected every bot thread resolved by the bot")
		case t.ResolvedBy == nil || !isCodeRabbit(*t.ResolvedBy):
			by := "an unreadable account"
			if t.ResolvedBy != nil {
				by = who(*t.ResolvedBy)
			}
			why = append(why, fmt.Sprintf("coderabbit: a CodeRabbit thread was resolved by %s, expected the bot (%d)", by, CodeRabbitUserID))
		}
	}
	mention := func(c Comment) bool {
		return !isCodeRabbit(c.Author) && strings.Contains(strings.ToLower(c.Body), "@coderabbitai")
	}
	all := append(slices.Clone(f.Comments), Comment{Author: f.PR.Author, Body: f.PR.Body})
	for _, r := range f.Reviews {
		all = append(all, Comment{Author: r.Author, Body: r.Body})
	}
	for _, t := range f.Threads {
		all = append(all, t.Comments...)
	}
	for _, c := range all {
		if mention(c) {
			why = append(why, fmt.Sprintf("coderabbit: %s mentioned @coderabbitai, expected no non-bot mention", who(c.Author)))
			break
		}
	}
	return why
}

func who(a Actor) string {
	if a.Login == "" {
		return fmt.Sprintf("an account with id %d", a.DatabaseID)
	}
	return fmt.Sprintf("%s %q (id %d)", strings.ToLower(nonEmpty(a.Typename, "actor")), a.Login, a.DatabaseID)
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// short is a commit id cut to 12 characters for a reason sentence.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return nonEmpty(sha, "none")
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func quoteList(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return strings.Join(q, ", ")
}
