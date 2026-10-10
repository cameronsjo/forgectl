package merge

import (
	"fmt"
	"html"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// GitHubActionsAppID is the GitHub Actions app's id, matched by number,
// never by name; a required check run must come from it.
const GitHubActionsAppID int64 = 15368

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
	// StartedAt is the ledger row's started_at: SelectPR ignores a PR
	// created before it, which belongs to an earlier launch of the name.
	StartedAt time.Time
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
	Number            int
	URL               string
	Title             string
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
	// LastEditedAt is when the body was last edited, "" when it never was.
	// EditUnread is true when the response did not carry the field at all,
	// so whether it was edited is not known.
	LastEditedAt string
	EditUnread   bool
}

// Comment is a PR conversation comment, or an inline review comment. Only
// checkOpenFindings reads it: a comment is never a passing marker, but one
// holding a marker can refuse.
type Comment struct {
	Author    Actor
	Body      string
	URL       string
	CreatedAt string
	// LastEditedAt and EditUnread are as on Review.
	LastEditedAt string
	EditUnread   bool
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
	// Comments are the PR's conversation comments, and ReviewComments its
	// inline review comments (on a diff line), from every review.
	Comments       []Comment
	ReviewComments []Comment
	Files          []File
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
		add("[surface.merge] approvers is empty, expected cadence-review")
		return
	}
	var why []string
	for _, a := range s.Approvers {
		var r []string
		switch a {
		case config.MergeApproverCadenceReview:
			r = cadenceReview(f, s)
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
	// edited is true when the review was edited after it was posted, or
	// when the read could not say: such a marker never passes.
	edited bool
}

// countedReview reports a review the cadence-review approver reads: a User
// review by s.MarkerAuthorID in state COMMENTED or APPROVED.
func countedReview(r Review, s config.MergeSettings) bool {
	return r.Author.Typename == "User" && r.Author.DatabaseID == s.MarkerAuthorID && r.Author.DatabaseID != 0 &&
		(r.State == "COMMENTED" || r.State == "APPROVED")
}

// markersOf collects the strict cadence-review markers by s.MarkerAuthorID,
// per reviewer, for the cadence-review approver. A marker review whose
// submittedAt does not parse is not counted here; checkOpenFindings refuses
// it. An edited one is counted, so it can be the latest, but never passes.
func markersOf(f Facts, s config.MergeSettings) map[string][]marker {
	byReviewer := map[string][]marker{}
	for i, r := range f.Reviews {
		if !countedReview(r, s) {
			continue
		}
		m, ok := ParseMarker(r.Body)
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			continue
		}
		byReviewer[m.Reviewer] = append(byReviewer[m.Reviewer], marker{Marker: m, at: at, commit: r.CommitOID, index: i,
			edited: r.LastEditedAt != "" || r.EditUnread})
	}
	return byReviewer
}

// passingAt reports a marker that clears the head: never edited, it names
// head, was posted at head, and reports no Critical or Important finding.
func passingAt(m marker, head string) bool {
	return !m.edited && m.Head == head && m.commit == head && m.Crit == 0 && m.Imp == 0
}

// looseMarker reads the reviewer name a mention of the marker claims (the
// token after it), in a body scanText has normalized: any case, any run of
// space, backslash, underscore or hyphen (ASCII or Unicode) between
// "cadence" and "review", and space before the colon. Whether a body
// mentions the marker at all is markerSkeleton's to say, which errs wider:
// a mention this cannot read a name from refuses outright.
var looseMarker = regexp.MustCompile(`(?i)cadence[\s\\_\-\x{2010}-\x{2015}\x{2212}]*review\s*:[ \t]*(\S*)`)

// markerSkeleton is the marker's word as skeleton keeps it. A body whose
// skeleton holds it mentions the marker, however it is styled: markdown
// emphasis, code spans or strike-through, any punctuation between or after
// the words, look-alike letters, or none of these.
const markerSkeleton = "cadencereview"

// scanText is a body as looseMarker and skeleton read it: invisible,
// zero-width, bidi and control characters (other than space, tab and line
// ends) and invalid UTF-8 removed, NFKC-normalized (so full-width letters
// and compatibility hyphens and spaces fold to ASCII), HTML entities
// decoded, and then removed and normalized again, since an entity can spell
// a hidden character; then Cyrillic, Greek and small-capital letters that
// look like Latin ones folded onto them (foldConfusables).
func scanText(body string) string {
	once := func(s string) string { return norm.NFKC.String(stripHidden(s)) }
	return foldConfusables(once(html.UnescapeString(once(body))))
}

// confusables maps letters that look like Latin ones onto those letters,
// case kept: Cyrillic, Greek, and small capitals NFKC leaves alone.
var confusables = map[rune]rune{
	// Cyrillic.
	'а': 'a', 'в': 'b', 'г': 'r', 'ԁ': 'd', 'е': 'e', 'һ': 'h', 'і': 'i', 'ј': 'j', 'к': 'k', 'ӏ': 'l', 'м': 'm', 'н': 'h',
	'о': 'o', 'р': 'p', 'ԛ': 'q', 'с': 'c', 'ѕ': 's', 'т': 't', 'у': 'y', 'ѵ': 'v', 'ԝ': 'w', 'х': 'x',
	'А': 'A', 'В': 'B', 'Е': 'E', 'Һ': 'H', 'І': 'I', 'Ј': 'J', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P',
	'С': 'C', 'Ѕ': 'S', 'Т': 'T', 'У': 'Y', 'Х': 'X',
	// Greek.
	'α': 'a', 'ε': 'e', 'η': 'n', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P',
	'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
	// Latin small capitals and dotless i.
	'ᴀ': 'a', 'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ɪ': 'i', 'ı': 'i', 'ɴ': 'n', 'ᴏ': 'o', 'ʀ': 'r', 'ᴠ': 'v', 'ᴡ': 'w',
}

// foldConfusables replaces each look-alike letter in s (confusables).
func foldConfusables(s string) string {
	return strings.Map(func(r rune) rune {
		if l, ok := confusables[r]; ok {
			return l
		}
		return r
	}, s)
}

// skeleton is s lowercased with everything but a-z and 0-9 dropped, so
// "**Cadence-Review**:" and "cadence.review" both read "cadencereview".
func skeleton(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(s))
}

func stripHidden(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			return r
		case r == utf8.RuneError || termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r):
			return -1
		}
		return r
	}, s)
}

// reviewerName is a reviewer name a strict marker can carry.
var reviewerName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// maxQuotedLine caps a body line quoted in a reason.
const maxQuotedLine = 120

// Post kinds a marker can appear in.
const (
	postReview       = "pull request review"
	postConversation = "conversation comment"
	postInline       = "inline review comment"
)

// markerPost is a review, conversation comment or inline review comment by
// marker_author_id whose body mentions the marker's word.
type markerPost struct {
	// kind is postReview, postConversation or postInline, and where names
	// the post for a reason: "review <url>" or "<kind> <url>".
	kind, where string
	// stampField is the timestamp's GraphQL name, and stamp its value.
	stampField, stamp string
	// edited is the post's lastEditedAt ("" when never edited), and
	// editUnread says the read did not carry it.
	edited     string
	editUnread bool
	body       string
	// count is how many times the body's skeleton holds markerSkeleton,
	// and mentions the names looseMarker reads, at most one per count.
	count    int
	mentions [][]string
	// review is nil for a comment.
	review *Review
}

// mentionsOf reports how many times body mentions the marker (its skeleton
// holding markerSkeleton) and the names looseMarker reads from it; 0 is no
// mention.
func mentionsOf(body string) (int, [][]string) {
	text := scanText(body)
	n := strings.Count(skeleton(text), markerSkeleton)
	if n == 0 {
		return 0, nil
	}
	return n, looseMarker.FindAllStringSubmatch(text, -1)
}

// markerPosts lists the reviews (any state, any commit), conversation
// comments and inline review comments by s.MarkerAuthorID that mention the
// marker's word (mentionsOf).
func markerPosts(f Facts, s config.MergeSettings) []markerPost {
	var out []markerPost
	for i := range f.Reviews {
		r := &f.Reviews[i]
		if r.Author.DatabaseID != s.MarkerAuthorID {
			continue
		}
		if n, m := mentionsOf(r.Body); n > 0 {
			out = append(out, markerPost{kind: postReview, where: "review " + nonEmpty(r.URL, "with no URL"), stampField: "submittedAt", stamp: r.SubmittedAt,
				edited: r.LastEditedAt, editUnread: r.EditUnread, body: r.Body, count: n, mentions: m, review: r})
		}
	}
	for _, list := range []struct {
		kind     string
		comments []Comment
	}{{postConversation, f.Comments}, {postInline, f.ReviewComments}} {
		for _, c := range list.comments {
			if c.Author.DatabaseID != s.MarkerAuthorID {
				continue
			}
			if n, m := mentionsOf(c.Body); n > 0 {
				out = append(out, markerPost{kind: list.kind, where: list.kind + " " + nonEmpty(c.URL, "with no URL"), stampField: "createdAt", stamp: c.CreatedAt,
					edited: c.LastEditedAt, editUnread: c.EditUnread, body: c.Body, count: n, mentions: m})
			}
		}
	}
	return out
}

// strictPass returns the marker when p is a review that passes at head: a
// counted review, never edited, whose body mentions the marker's word once,
// in a first line that is an exact marker naming head, posted at head, with
// crit=0 imp=0.
func strictPass(p markerPost, head string, s config.MergeSettings) (Marker, bool) {
	if p.review == nil || p.edited != "" || p.editUnread || !countedReview(*p.review, s) || p.count != 1 || len(p.mentions) != 1 {
		return Marker{}, false
	}
	m, ok := ParseMarker(p.body)
	if !ok || m.Crit != 0 || m.Imp != 0 || m.Head != head || p.review.CommitOID != head {
		return Marker{}, false
	}
	return m, true
}

// whyNotPassing says why p is not a passing marker at head.
func whyNotPassing(p markerPost, head string) string {
	var why []string
	if p.review == nil {
		why = append(why, "a "+p.kind+" never counts as a passing marker")
	} else {
		r := p.review
		if r.Author.Typename != "User" {
			why = append(why, fmt.Sprintf("its author is a %s, expected a User", nonEmpty(r.Author.Typename, "unknown actor")))
		}
		if r.State != "COMMENTED" && r.State != "APPROVED" {
			why = append(why, fmt.Sprintf("its state is %s, expected COMMENTED or APPROVED", nonEmpty(r.State, "unknown")))
		}
	}
	if p.edited != "" {
		why = append(why, fmt.Sprintf("it was edited at %s; an edited post never counts as a passing marker", p.edited))
	}
	if p.count > 1 {
		why = append(why, fmt.Sprintf("it mentions cadence-review %d times, expected one marker", p.count))
	}
	m, ok := ParseMarker(p.body)
	switch {
	case !ok:
		line, _, _ := strings.Cut(p.body, "\n")
		if len(line) > maxQuotedLine {
			line = line[:maxQuotedLine] + "..."
		}
		why = append(why, fmt.Sprintf("its first line %s is not an exact marker", strconv.Quote(line)))
	case m.Crit > 0 || m.Imp > 0:
		why = append(why, fmt.Sprintf("%s reported crit=%d imp=%d at head %s", m.Reviewer, m.Crit, m.Imp, short(m.Head)))
	case p.review != nil && (m.Head != head || p.review.CommitOID != head):
		why = append(why, fmt.Sprintf("it names head %s at review commit %s, expected %s for both", short(m.Head), short(p.review.CommitOID), short(head)))
	}
	return strings.Join(why, "; ")
}

// checkOpenFindings fails closed on cadence-review markers. Every review
// (any state, any commit), conversation comment and inline review comment by
// marker_author_id whose body mentions the marker's word in any styling
// (mentionsOf) and that is not a strict passing marker at the head (strictPass)
// is an open finding for each reviewer it names, unless a strict passing
// marker at the head by the same reviewer was submitted later (a marker in
// the same second is not later). A post counts at its last edit when it was
// edited, and an edited post is never a strict pass, so an edit can neither
// hide a finding behind an earlier pass nor turn a finding into a pass. Each
// finding not cleared adds its own reason. One with a mention no reviewer
// name can be read from (looseMarker), one that names a reviewer that does
// not parse, or one whose timestamp or edit time does not, refuses
// outright: no later marker can be shown to clear it. It runs outside the
// approver loop, so it refuses under every approver set (ADR-0011,
// 2026-10-09 amendment, decision 1).
func checkOpenFindings(f Facts, s config.MergeSettings, add addFunc) {
	if s.MarkerAuthorID <= 0 {
		add("[surface.merge] marker_author_id is not set, expected the operator's id: open cadence-review findings cannot be read without it")
		return
	}
	head := f.PR.HeadRefOid
	type openItem struct {
		at                   time.Time
		reviewer, where, why string
	}
	passes := map[string][]time.Time{}
	var open []openItem
	for _, p := range markerPosts(f, s) {
		at, err := time.Parse(time.RFC3339, p.stamp)
		if err != nil {
			add("open finding: %s mentions cadence-review: and has %s %q, expected a timestamp, so no later passing marker can be shown to clear it; this refuses under every approver", p.where, p.stampField, p.stamp)
			continue
		}
		if p.editUnread {
			add("open finding: %s mentions cadence-review: and the read did not say whether it was edited (no lastEditedAt), so no later passing marker can be shown to clear it; this refuses under every approver", p.where)
			continue
		}
		if p.edited != "" {
			edit, err := time.Parse(time.RFC3339, p.edited)
			if err != nil {
				add("open finding: %s mentions cadence-review: and has lastEditedAt %q, expected a timestamp, so no later passing marker can be shown to clear it; this refuses under every approver", p.where, p.edited)
				continue
			}
			if edit.After(at) {
				at = edit // an edited post counts at its edit
			}
		}
		if m, ok := strictPass(p, head, s); ok {
			passes[m.Reviewer] = append(passes[m.Reviewer], at)
			continue
		}
		why := whyNotPassing(p, head)
		if len(p.mentions) == 0 || p.count > len(p.mentions) {
			add("open finding: %s mentions cadence-review %d time(s) in a form no reviewer name can be read from (%d read), so no marker can clear it: %s; this refuses under every approver",
				p.where, p.count, len(p.mentions), why)
			continue
		}
		var names []string
		bad, badName := false, ""
		for _, mention := range p.mentions {
			if !reviewerName.MatchString(mention[1]) {
				bad, badName = true, mention[1]
				break
			}
			if !slices.Contains(names, mention[1]) {
				names = append(names, mention[1])
			}
		}
		if bad {
			add("open finding: %s mentions cadence-review: with reviewer name %q, which does not parse (expected 1-40 characters of a-z, 0-9 and '-'), so no marker can clear it: %s; this refuses under every approver", p.where, badName, why)
			continue
		}
		for _, n := range names {
			open = append(open, openItem{at: at, reviewer: n, where: p.where, why: why})
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		if open[i].reviewer != open[j].reviewer {
			return open[i].reviewer < open[j].reviewer
		}
		return open[i].at.Before(open[j].at)
	})
	for _, it := range open {
		if slices.ContainsFunc(passes[it.reviewer], func(t time.Time) bool { return t.After(it.at) }) {
			continue
		}
		add("open finding: %s (%s, %s), expected a later passing marker by %s at head %s; this refuses under every approver",
			it.why, it.where, it.at.UTC().Format(time.RFC3339), it.reviewer, short(head))
	}
}

// cadenceReview returns why the cadence-review approver does not pass, or
// nothing when it does: each required reviewer's latest marker must pass at
// the head. Open findings from any reviewer are checkOpenFindings'.
func cadenceReview(f Facts, s config.MergeSettings) []string {
	var why []string
	head := f.PR.HeadRefOid
	byReviewer := markersOf(f, s)
	for _, name := range s.RequiredReviewers {
		ms := byReviewer[name]
		if len(ms) == 0 {
			why = append(why, fmt.Sprintf("cadence-review: no %s marker by user %d, expected one at head %s", name, s.MarkerAuthorID, short(head)))
			continue
		}
		last := latestMarker(ms)
		if !passingAt(last, head) {
			edited := ""
			if last.edited {
				edited = ", edited after it was posted (an edited marker never passes)"
			}
			why = append(why, fmt.Sprintf("cadence-review: %s's latest marker is head=%s (review commit %s) crit=%d imp=%d%s, expected head=%s crit=0 imp=0", name, short(last.Head), short(last.commit), last.Crit, last.Imp, edited, short(head)))
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
