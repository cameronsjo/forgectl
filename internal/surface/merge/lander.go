package merge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Land is the one merge path (atelier P4, T10.4): `surface merge` (ByCLI)
// and the drain's autopilot (ByDrain) both call it. It reads the facts
// fresh with no cache, asks Evaluate, takes the subject from the PR title,
// asks a person at a terminal (surface merge only, Lander.Confirm), reads
// the PR again and aborts on any change, writes the audit attempt
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
	// LandFailed: the merge was not made (gh failed and GitHub says the PR
	// is not merged, or the attempt line could not be written first).
	LandFailed = "merge-failed"
	// LandUnconfirmed: the merge could not be shown to be this attempt's on
	// the default branch: GitHub could not be read after gh reported
	// success, does not say merged yet (a merge queue), says merged at
	// another head, or the merge commit is not shown on the default branch.
	LandUnconfirmed = "merged-unconfirmed"
	// LandMergedElsewhere: GitHub says the PR merged, but its merge commit's
	// message does not carry this attempt's Audit-line: something else
	// merged it.
	LandMergedElsewhere = "merged-elsewhere"
	// LandUnknown: gh failed, and GitHub could not be read after it, so
	// whether the PR merged is not known.
	LandUnknown = "merge-unknown"
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
	// AuditErr is the error behind an AuditNote that says a line could not
	// be written; nil for a repeated refusal, which is not an error.
	AuditErr error `json:"-"`
	// Err is the read or merge failure behind LandUnreadable, LandFailed or
	// LandUnconfirmed.
	Err error `json:"-"`
}

// Lander is Land's I/O. Each field is required, except Confirm for the
// drain (see Confirm).
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
	// AuditCap is the most bytes the audit file may hold
	// (worker.MaxMergeAuditBytes): an attempt line is written only with room
	// for it and AuditOutcomeReserve after it.
	AuditCap int
	Now      func() time.Time
	// Sleep waits between landing reads, returning early when ctx ends.
	Sleep func(ctx context.Context, d time.Duration)
	// Confirm asks a person at a terminal to approve a merge `surface merge`
	// (ByCLI) is about to make, and returns nil only when they typed "yes".
	// Land calls it for every ByCLI merge that is not a dry run, after the
	// verdict passes and the subject is composed, before the re-read; nil
	// refuses that merge. The drain (ByDrain) never calls it: the autopilot
	// is the one unattended merge path.
	Confirm func(ctx context.Context, c Confirmation) error
}

// Confirmation is what a person is shown before `surface merge` merges.
type Confirmation struct {
	Repo    string
	PR      int
	URL     string
	Head    string
	Subject string
	// Checks and Markers are the verdict's evidence: the required check runs
	// counted and each required reviewer's latest marker.
	Checks  []CheckSeen
	Markers []Evidence
}

// ErrNoConfirmer reports a `surface merge` Lander with no Confirm.
var ErrNoConfirmer = errors.New("merge: surface merge has no way to ask a person at a terminal; nothing was merged")

// landingTries and landingWait bound the post-merge confirmation: GitHub can
// take a moment to name the merge commit and move the default branch.
// confirmTimeout caps the confirmation, which runs on a context of its own
// so a merge whose context ended (a deadline that killed gh after GitHub
// merged) is still read back.
const (
	landingTries   = 3
	landingWait    = 2 * time.Second
	confirmTimeout = 30 * time.Second
)

// maxGHError caps a gh error kept in an audit line or a reason.
const maxGHError = 300

// AuditOutcomeReserve is the room, beyond a second copy of the attempt
// line's own size, an attempt line leaves in the audit file for its outcome
// line: the outcome carries the same checks and markers as the attempt plus
// its reasons and result, so a merge that runs has room to record how it
// ended. The outcome line may use it; nothing else checks it.
const AuditOutcomeReserve = 8 << 10

// ErrAuditNoRoom reports an audit file with no room for an attempt line and
// AuditOutcomeReserve: the merge is refused.
var ErrAuditNoRoom = errors.New("merge: merge-audit.jsonl has no room for this merge's attempt line and the reserve for its outcome line; rotate it by hand (see docs/herdr.md)")

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
			out.AuditNote, out.AuditErr = "the refusal could not be audited: "+err.Error(), err
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
	if by == ByCLI && !dryRun {
		// A person at a terminal approves every merge by hand; the re-read
		// below still runs after their answer.
		err := ErrNoConfirmer
		if l.Confirm != nil {
			err = l.Confirm(ctx, Confirmation{Repo: base.Repo, PR: f.PR.Number, URL: f.PR.URL, Head: f.PR.HeadRefOid, Subject: subject,
				Checks: base.Checks, Markers: base.Markers})
		}
		if err != nil {
			return refuse([]string{"not confirmed at a terminal: " + termsafe.SafeLineMax(err.Error(), maxGHError)})
		}
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
	if errors.Is(err, ErrAuditNoRoom) {
		return refuse([]string{"the audit file is full: " + err.Error()})
	}
	if err != nil {
		out.Result, out.Err = LandFailed, err
		out.Reasons = []string{"nothing was merged: the audit attempt line could not be written: " + err.Error()}
		return out
	}
	out.AuditLine = hash
	body := Body(BodyInput{By: by, AuditHash: hash, PolicyHash: base.PolicyHash, PRURL: f.PR.URL, Head: f.PR.HeadRefOid, Checks: base.Checks, Markers: base.Markers})
	mergeErr := l.Merge(ctx, MergeArgs(f, subject, body))
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), confirmTimeout)
	landing, landErr := l.confirm(cctx, f, mergeErr == nil)
	cancel()
	outcome := base
	outcome.Attempt = hash
	ghFailed := func() string { return "gh pr merge failed: " + termsafe.SafeLineMax(mergeErr.Error(), maxGHError) }
	merged := landErr == nil && landing.Merged && landing.State == "MERGED"
	switch {
	case landErr != nil && mergeErr != nil:
		out.Result, out.Err = LandUnknown, mergeErr
		out.Reasons = []string{ghFailed() + "; GitHub could not be read after it: " + termsafe.SafeLineMax(landErr.Error(), maxGHError) +
			"; the PR may have merged: check it by hand"}
		outcome.Result = AuditUnknown
	case landErr != nil:
		out.Result, out.Err = LandUnconfirmed, landErr
		out.Reasons = []string{"gh pr merge reported success, but GitHub could not be read to confirm the merge: " + termsafe.SafeLineMax(landErr.Error(), maxGHError)}
		outcome.Result = AuditUnconfirmed
	case !merged && mergeErr != nil:
		out.Result, out.Err, out.Reasons = LandFailed, mergeErr, []string{ghFailed() + "; GitHub says the PR is " + nonEmpty(landing.State, "not merged")}
		outcome.Result = AuditFailed
	case !merged:
		out.Result = LandUnconfirmed
		out.Reasons = []string{"gh pr merge reported success, but GitHub does not say the PR merged (state " + nonEmpty(landing.State, "unknown") +
			"); a merge queue, if the branch has one, merges it later with a message of its own"}
		outcome.Result = AuditUnconfirmed
	case !CarriesAuditLine(landing.MergeMessage, hash):
		out.Result, out.MergeCommit = LandMergedElsewhere, landing.MergeCommit
		out.Reasons = []string{fmt.Sprintf("GitHub says the PR merged as %s, but that commit's message does not carry this attempt's Audit-line: sha256:%s; something else merged it",
			short(landing.MergeCommit), short(hash))}
		outcome.Result = AuditMergedElsewhere
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
	if mergeErr != nil && out.Result != LandFailed && out.Result != LandUnknown {
		// gh reported a failure, yet GitHub says it merged (a timeout after
		// the merge, say): the record keeps both.
		out.Reasons = append(out.Reasons, "gh pr merge reported: "+termsafe.SafeLineMax(mergeErr.Error(), maxGHError))
	}
	outcome.MergeCommit, outcome.Reasons = out.MergeCommit, out.Reasons
	if _, _, err := l.record(outcome); err != nil {
		out.AuditNote, out.AuditErr = "the outcome line could not be audited: "+err.Error(), err
	}
	return out
}

// CarriesAuditLine reports whether a merge commit's message holds the line
// "Audit-line: sha256:<hash>": the body this attempt wrote, so the merge is
// this attempt's and not another's.
func CarriesAuditLine(message, hash string) bool {
	if hash == "" {
		return false
	}
	want := "Audit-line: sha256:" + hash
	for _, line := range strings.Split(message, "\n") {
		if strings.TrimSuffix(line, "\r") == want {
			return true
		}
	}
	return false
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
		Time: l.Now().UTC().Format(time.RFC3339), Actor: by, Worker: f.Row.Name, Repo: repo, RepoID: f.Row.GitHubRepoID, PR: f.PR.Number, Head: f.PR.HeadRefOid,
		PolicyHash: PolicyHash(s), Checks: ChecksSeen(f, s), Markers: MarkerEvidence(f, s),
	}
}

// ErrAuditUnwritten reports an audit store that took no line.
var ErrAuditUnwritten = errors.New("merge: the audit store wrote no line")

// record appends line to the audit file, chained onto it, and returns the
// line's hash and whether it was written (a repeated refusal is not). An
// attempt line needs room for itself, an outcome line as large again, and
// AuditOutcomeReserve under AuditCap, or it is ErrAuditNoRoom.
func (l Lander) record(line AuditLine) (hash string, written bool, err error) {
	err = l.Audit(func(existing []byte) ([]byte, error) {
		data, h, write, err := AppendAudit(existing, line)
		if err != nil || !write {
			return nil, err
		}
		if line.Result == AuditMerging && (l.AuditCap <= 0 || len(existing)+2*len(data)+AuditOutcomeReserve > l.AuditCap) {
			return nil, ErrAuditNoRoom
		}
		hash, written = h, true
		return data, nil
	})
	if err == nil && line.Result != AuditRefused && !written {
		err = ErrAuditUnwritten
	}
	return hash, written, err
}
