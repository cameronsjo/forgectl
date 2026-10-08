package review

// The two release-pr stall rules that watch the release machinery itself, not
// the nightly beat:
//
//   - no-release-pr: releasable commits have waited past ReleasablePRWindow
//     and no release PR is open.
//   - release-workflow-stuck: a run of the release-PR workflow has sat in
//     waiting, queued, or pending past StuckRunWindow.
//
// Everything here is pure; collection (releases_github.go) only fills the
// facts these functions read.

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	// ReleasablePRWindow is how long releasable commits may sit on the branch
	// with no open release PR before the radar calls it a stall.
	ReleasablePRWindow = 24 * time.Hour
	// StuckRunWindow is how long a release-PR workflow run may stay waiting,
	// queued, or pending. A healthy run starts within minutes.
	StuckRunWindow = time.Hour
)

// Stall reasons. Each stall string for these two rules starts with its name,
// so the table's stall line and --json carry the reason verbatim.
const (
	StallNoReleasePR          = "no-release-pr"
	StallReleaseWorkflowStuck = "release-workflow-stuck"
)

// stuckStatuses are the run statuses that mean "has not started".
var stuckStatuses = map[string]bool{"waiting": true, "queued": true, "pending": true}

// DefaultReleaseWorkflow is the workflow that opens the release PR when the
// registry entry sets no release_workflow. cadence-hooks prepares its release
// in prepare-release.yml; every other release-pr repo uses release-please.
func DefaultReleaseWorkflow(repo string) string {
	if repo == "cadence-hooks" {
		return ".github/workflows/prepare-release.yml"
	}
	return ".github/workflows/release-please.yml"
}

// ReleaseWorkflowFile returns the entry's release-PR workflow path and
// whether the registry named it (an unnamed default that does not exist in the
// repo is not an error; a named one is).
func ReleaseWorkflowFile(e RegistryEntry) (file string, explicit bool) {
	if e.ReleaseWorkflow != "" {
		return e.ReleaseWorkflow, true
	}
	return DefaultReleaseWorkflow(e.Repo), false
}

// CommitFact is one unreleased commit, reduced to what the releasable rule
// reads.
type CommitFact struct {
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	// BreakingFooter is true when the message body carries a
	// `BREAKING CHANGE:` (or `BREAKING-CHANGE:`) footer.
	BreakingFooter bool `json:"breaking_footer,omitempty"`
}

var (
	reConventional   = regexp.MustCompile(`^([A-Za-z]+)(\([^)]*\))?(!)?: `)
	reBreakingFooter = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE: `)
	releasableTypes  = map[string]bool{"feat": true, "fix": true, "perf": true}
)

// NewCommitFact reduces a full commit message to a CommitFact.
func NewCommitFact(message string, at time.Time) CommitFact {
	subject, body, _ := strings.Cut(message, "\n")
	const maxSubject = 200
	if len(subject) > maxSubject {
		subject = subject[:maxSubject]
	}
	return CommitFact{At: at, Subject: strings.TrimSpace(subject), BreakingFooter: reBreakingFooter.MatchString(body)}
}

// Releasable reports whether the commit would make release-please open a
// release PR: type feat, fix, or perf, a `!` breaking marker on any type, or
// a BREAKING CHANGE footer. chore, ci, docs, test, refactor, style, and build
// alone never do.
func (c CommitFact) Releasable() bool {
	if c.BreakingFooter {
		return true
	}
	m := reConventional.FindStringSubmatch(c.Subject)
	if m == nil {
		return false
	}
	return m[3] == "!" || releasableTypes[strings.ToLower(m[1])]
}

// releasableSummary counts the releasable commits and returns the oldest one's
// time. ok is false when there are none.
func releasableSummary(commits []CommitFact) (n int, oldest time.Time, ok bool) {
	for _, c := range commits {
		if !c.Releasable() {
			continue
		}
		if n == 0 || c.At.Before(oldest) {
			oldest = c.At
		}
		n++
	}
	return n, oldest, n > 0
}

// noReleasePRStall fires when releasable commits are unreleased, no release PR
// is open, and the oldest releasable commit is more than ReleasablePRWindow
// old.
func noReleasePRStall(f RepoFacts, now time.Time) []string {
	if len(f.ReleasePRs) > 0 {
		return nil
	}
	n, oldest, ok := releasableSummary(f.UnreleasedCommits)
	// A commit merged after the cut can carry an older committer date (a
	// rebase or sync keeps the original); nothing is due before the last cut.
	if f.LastRelease != nil && oldest.Before(f.LastRelease.At) {
		oldest = f.LastRelease.At
	}
	if !ok || now.Sub(oldest) <= ReleasablePRWindow {
		return nil
	}
	return []string{fmt.Sprintf("%s: %d releasable commit(s) since %s, oldest %s ago, and no open release PR",
		StallNoReleasePR, n, orNone(lastTag(f)), Age(now, oldest))}
}

// releaseWorkflowStall fires for the oldest run of the release-PR workflow
// that has stayed waiting, queued, or pending past StuckRunWindow.
func releaseWorkflowStall(workflow string, runs []RunFact, now time.Time) []string {
	var oldest *RunFact
	for i := range runs {
		r := &runs[i]
		if !stuckStatuses[r.Status] || now.Sub(r.CreatedAt) <= StuckRunWindow {
			continue
		}
		if oldest == nil || r.CreatedAt.Before(oldest.CreatedAt) {
			oldest = r
		}
	}
	if oldest == nil {
		return nil
	}
	return []string{fmt.Sprintf("%s: %s run %d has been %s for %s",
		StallReleaseWorkflowStuck, path.Base(workflow), oldest.ID, oldest.Status, Age(now, oldest.CreatedAt))}
}

func lastTag(f RepoFacts) string {
	if f.LastRelease == nil {
		return ""
	}
	return f.LastRelease.Tag
}
