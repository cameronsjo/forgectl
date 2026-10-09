package merge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Land is the one merge path (atelier P4, T10.4): `surface merge` (ByCLI)
// and the drain's autopilot (ByDrain) both call it. It reads the facts
// fresh with no cache, asks Evaluate, takes the subject from the PR title,
// reads the PR again and aborts on any change, writes the audit attempt
// line, runs `gh pr merge --squash --match-head-commit` with a body it
// writes, and confirms the merge commit is on the default branch. Every
// refusal and every outcome is one audit line; repeated refusals are
// limited by AppendAudit.

// Outcome results.
const (
	// LandMerged: merged, and the merge commit is on the default branch.
	LandMerged = "merged"
	// LandWouldMerge: a dry run that would merge.
	LandWouldMerge = "would-merge"
	// LandRefused: the policy, the subject or the re-read refused.
	LandRefused = "refused"
	// LandUnreadable: GitHub could not be read; nothing was decided.
	LandUnreadable = "unreadable"
	// LandFailed: the merge was not made (gh failed, or the attempt line
	// could not be written first).
	LandFailed = "merge-failed"
	// LandUnconfirmed: GitHub says merged, but the merge commit could not be
	// shown on the default branch.
	LandUnconfirmed = "merged-unconfirmed"
)

// Outcome is what Land did.
type Outcome struct {
	Result      string   `json:"result"`
	PR          int      `json:"pr"`
	URL         string   `json:"url"`
	Head        string   `json:"head"`
	Reasons     []string `json:"reasons"`
	MergeCommit string   `json:"merge_commit"`
	// AuditLine is the hash of the attempt line the squash body carries.
	AuditLine string `json:"audit_line"`
	// AuditNote says an audit line could not be written, or a refusal
	// repeated an earlier one and was not written again.
	AuditNote string `json:"audit_note,omitempty"`
	// Err is the read or merge failure behind LandUnreadable, LandFailed or
	// LandUnconfirmed.
	Err error `json:"-"`
}

// Lander is Land's I/O. Each field is required.
type Lander struct {
	// Read reads the facts with no cache (Reader.Read).
	Read func(ctx context.Context, row Row) (Snapshot, error)
	// Recheck reads the PR and the named required checks again
	// (Reader.Recheck).
	Recheck func(ctx context.Context, f Facts, checks []string) ([]string, error)
	// Landed reads what the merge left (Reader.Landed).
	Landed func(ctx context.Context, f Facts) (Landing, error)
	// Merge runs gh with args from a neutral working directory, through the
	// host-pinned runner.
	Merge func(ctx context.Context, args []string) error
	// Audit appends what fn returns for the whole audit file
	// (worker.MergeAudit.Append).
	Audit func(fn func(existing []byte) ([]byte, error)) error
	Now   func() time.Time
	// Sleep waits between landing reads, returning early when ctx ends.
	Sleep func(ctx context.Context, d time.Duration)
}

// landingTries and landingWait bound the post-merge confirmation: GitHub can
// take a moment to name the merge commit and move the default branch.
const (
	landingTries = 3
	landingWait  = 2 * time.Second
)

// maxGHError caps a gh error kept in an audit line or a reason.
const maxGHError = 300

// MergeArgs is the gh argv for a squash merge of f's PR bound to its head:
// the repository named host-qualified, never --admin, never --auto.
func MergeArgs(f Facts, subject, body string) []string {
	return []string{"pr", "merge", strconv.Itoa(f.PR.Number), "-R", Host + "/" + f.Repository.NameWithOwner,
		"--squash", "--match-head-commit", f.PR.HeadRefOid, "--subject", subject, "--body", body}
}

// Land runs one merge attempt for row under s, as by.
func (l Lander) Land(ctx context.Context, s config.MergeSettings, by By, row Row, dryRun bool) Outcome {
	snap, err := l.Read(ctx, row)
	if err != nil {
		return Outcome{Result: LandUnreadable, Reasons: []string{"GitHub could not be read: " + termsafe.SafeLineMax(err.Error(), maxGHError)}, Err: err}
	}
	f := snap.Facts
	base := l.auditBase(s, by, f)
	out := Outcome{PR: f.PR.Number, URL: f.PR.URL, Head: f.PR.HeadRefOid}
	refuse := func(reasons []string) Outcome {
		out.Result, out.Reasons = LandRefused, reasons
		if dryRun {
			return out
		}
		line := base
		line.Result, line.Reasons = AuditRefused, reasons
		if _, written, err := l.record(line); err != nil {
			out.AuditNote = "the refusal could not be audited: " + err.Error()
		} else if !written {
			out.AuditNote = "the same refusal is already in the audit file"
		}
		return out
	}
	if !snap.HasPR {
		return refuse([]string{"no PR: " + snap.NoPR})
	}
	if v := Evaluate(f, Policy{Settings: s, ForDrain: by == ByDrain}); v.Result != Pass {
		return refuse(v.Reasons)
	}
	subject, err := Subject(f.PR.Title)
	if err != nil {
		return refuse([]string{err.Error()})
	}
	repo, _ := s.Repo(f.Row.GitHubRepo) // Evaluate passed, so the repository is listed
	moved, err := l.Recheck(ctx, f, repo.RequiredChecks)
	if err != nil {
		out.Result, out.Err = LandUnreadable, err
		out.Reasons = []string{"the PR could not be read again before the merge: " + termsafe.SafeLineMax(err.Error(), maxGHError)}
		return out
	}
	if len(moved) > 0 {
		reasons := make([]string, 0, len(moved))
		for _, m := range moved {
			reasons = append(reasons, "the PR changed between the verdict and the merge: "+m)
		}
		return refuse(reasons)
	}
	if dryRun {
		out.Result, out.Reasons = LandWouldMerge, []string{}
		return out
	}
	attempt := base
	attempt.Result, attempt.Reasons = AuditMerging, []string{}
	hash, _, err := l.record(attempt)
	if err != nil {
		out.Result, out.Err = LandFailed, err
		out.Reasons = []string{"nothing was merged: the audit attempt line could not be written: " + err.Error()}
		return out
	}
	out.AuditLine = hash
	body := Body(BodyInput{By: by, AuditHash: hash, PolicyHash: base.PolicyHash, PRURL: f.PR.URL, Head: f.PR.HeadRefOid, Checks: base.Checks, Markers: base.Markers})
	mergeErr := l.Merge(ctx, MergeArgs(f, subject, body))
	landing, landErr := l.confirm(ctx, f, mergeErr == nil)
	outcome := base
	outcome.Attempt = hash
	switch {
	case mergeErr != nil && (landErr != nil || !landing.Merged):
		why := "gh pr merge failed: " + termsafe.SafeLineMax(mergeErr.Error(), maxGHError)
		if landErr != nil {
			why += "; the PR's state could not be read after it: " + termsafe.SafeLineMax(landErr.Error(), maxGHError)
		}
		out.Result, out.Err, out.Reasons = LandFailed, mergeErr, []string{why}
		outcome.Result = AuditFailed
	case landErr != nil:
		out.Result, out.Err = LandUnconfirmed, landErr
		out.Reasons = []string{"merged, but GitHub could not be read to confirm the merge commit: " + termsafe.SafeLineMax(landErr.Error(), maxGHError)}
		outcome.Result = AuditUnconfirmed
	case landing.HeadRefOid != f.PR.HeadRefOid:
		out.Result, out.MergeCommit = LandUnconfirmed, landing.MergeCommit
		out.Reasons = []string{fmt.Sprintf("GitHub says the PR merged at head %s, expected %s", short(landing.HeadRefOid), short(f.PR.HeadRefOid))}
		outcome.Result = AuditUnconfirmed
	case !landing.OnDefault:
		out.Result, out.MergeCommit = LandUnconfirmed, landing.MergeCommit
		out.Reasons = []string{fmt.Sprintf("the merge commit %s is not shown on %s (head %s, compare status %q)",
			short(landing.MergeCommit), nonEmpty(landing.DefaultBranch, "the default branch"), short(landing.DefaultHead), landing.Ancestry)}
		outcome.Result = AuditUnconfirmed
	default:
		out.Result, out.MergeCommit, out.Reasons = LandMerged, landing.MergeCommit, []string{}
		outcome.Result = AuditMerged
	}
	if mergeErr != nil && out.Result != LandFailed {
		// gh reported a failure, yet GitHub says it merged (a timeout after
		// the merge, say): the record keeps both.
		out.Reasons = append(out.Reasons, "gh pr merge reported: "+termsafe.SafeLineMax(mergeErr.Error(), maxGHError))
	}
	outcome.MergeCommit, outcome.Reasons = out.MergeCommit, out.Reasons
	if _, _, err := l.record(outcome); err != nil {
		out.AuditNote = "the outcome line could not be audited: " + err.Error()
	}
	return out
}

// confirm reads what the merge left, again while GitHub has not yet named
// a merge commit on the default branch. When gh failed, one read decides.
func (l Lander) confirm(ctx context.Context, f Facts, merged bool) (Landing, error) {
	tries := landingTries
	if !merged {
		tries = 1
	}
	var landing Landing
	var err error
	for i := range tries {
		if i > 0 {
			l.Sleep(ctx, landingWait)
		}
		landing, err = l.Landed(ctx, f)
		if err == nil && landing.Merged && landing.OnDefault {
			return landing, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return landing, err
}

// auditBase is the audit line every record for f starts from.
func (l Lander) auditBase(s config.MergeSettings, by By, f Facts) AuditLine {
	repo := f.Repository.NameWithOwner
	if repo == "" {
		repo = f.Row.GitHubRepo
	}
	return AuditLine{
		Time: l.Now().UTC().Format(time.RFC3339), Actor: by, Repo: repo, RepoID: f.Row.GitHubRepoID, PR: f.PR.Number, Head: f.PR.HeadRefOid,
		PolicyHash: PolicyHash(s), Checks: ChecksSeen(f, s), Markers: MarkerEvidence(f, s),
	}
}

// ErrAuditUnwritten reports an audit store that took no line.
var ErrAuditUnwritten = errors.New("merge: the audit store wrote no line")

// record appends line to the audit file, chained onto it, and returns the
// line's hash and whether it was written (a repeated refusal is not).
func (l Lander) record(line AuditLine) (hash string, written bool, err error) {
	err = l.Audit(func(existing []byte) ([]byte, error) {
		data, h, write, err := AppendAudit(existing, line)
		if err != nil || !write {
			return nil, err
		}
		hash, written = h, true
		return data, nil
	})
	if err == nil && line.Result != AuditRefused && !written {
		err = ErrAuditUnwritten
	}
	return hash, written, err
}
