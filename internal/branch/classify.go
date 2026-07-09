// Test plan for classify.go
//
//   - Protected (default) branch always classifies Blocked, regardless of any
//     other signal.
//   - An open PR ALWAYS blocks — even a merged-on-server, worktree-attached
//     branch is Blocked if a PR is still open against it (gotcha #2 wins over
//     gotcha #1/#3).
//   - MergedOnServer + no worktree -> SafeToDelete.
//   - MergedOnServer + worktree attached -> SafeToDelete (WorktreePath carried
//     through so Prune knows to remove the worktree before the branch), never
//     NeedsAttention — the worktree-then-branch order is Prune's job, not a
//     reason to punt to a human.
//   - MergedLocally=true but MergedOnServer=false must NEVER classify
//     SafeToDelete — Classify does not even read MergedLocally as a delete
//     signal (gotcha #1/#5): it exists on Info purely as a reported, informational
//     data point.
//   - UpstreamGone with no MergedOnServer confirmation -> NeedsAttention, not
//     SafeToDelete — a gone upstream is not proof of a merge (could be a
//     force-push, a closed-without-merge PR, or a deleted-then-recreated
//     branch).
//   - A branch with none of the above (no PR record at all, upstream still
//     live) -> Blocked, "appears active" — never silently dropped from the
//     report.
package branch

import "fmt"

// Group is the bucket a Classify decision lands in — the three sections of
// `forgectl branch`'s report.
type Group string

const (
	// SafeToDelete is server-confirmed merged (never local-ancestry-only, see
	// gotcha #1), not backing an open PR, and not the protected default branch.
	SafeToDelete Group = "safe-to-delete"
	// Blocked is a branch this tool must never delete: the protected default
	// branch, or one an open PR is based on (gotcha #2).
	Blocked Group = "blocked"
	// NeedsAttention is ambiguous — a human should look before deleting: an
	// upstream-gone branch with no server-confirmed merge.
	NeedsAttention Group = "needs-attention"
)

// Info is one branch's classification input — the metadata Enumerate gathers
// from git/gh before Classify (a pure function of this struct) decides its
// Group. Field names deliberately spell out WHICH signal source each merge
// flag came from, since conflating them is exactly gotcha #1.
type Info struct {
	// Name is the branch's short name (e.g. "feat/foo"), never a full ref.
	Name string

	LocalExists  bool
	RemoteExists bool

	// UpstreamGone reports `git for-each-ref`'s upstream:track showing
	// "[gone]" — the local branch's remote-tracking ref was deleted. This is
	// NOT proof of a merge (a closed-without-merge PR looks identical).
	UpstreamGone bool

	// MergedLocally is `git branch --merged <default>`'s verdict: true only if
	// the branch's tip is a literal ancestor of the default branch. A
	// squash-merged branch is NEVER an ancestor, so this is false for the
	// exact case gotcha #1 warns about. Classify does not use this field to
	// decide SafeToDelete — it is carried on Info purely so the report/tests
	// can show local-vs-server disagreement.
	MergedLocally bool

	// MergedOnServer is true when a `gh pr list --state merged` row's
	// headRefName matches this branch — the ONLY signal Classify trusts for
	// SafeToDelete (gotcha #1, #5).
	MergedOnServer bool

	// OpenPRNumber is nonzero when an OPEN PR's headRefName matches this
	// branch — deleting the branch would break that PR (gotcha #2).
	OpenPRNumber int

	// WorktreePath is nonempty when this branch is checked out in a worktree
	// (from `git worktree list --porcelain`). Prune must remove the worktree
	// before deleting the branch (gotcha #3) — Classify does not downgrade
	// such a branch to NeedsAttention just because a worktree is attached.
	WorktreePath string

	// Protected marks the repo's default branch (or any branch the caller has
	// otherwise pinned) — always Blocked.
	Protected bool
}

// Classification is one branch's Classify verdict plus the human-readable
// reason driving it — Report groups these three ways.
type Classification struct {
	Info   Info
	Group  Group
	Reason string
}

// Report is the grouped, dry-run-friendly output of classifying every
// enumerated branch.
type Report struct {
	SafeToDelete   []Classification
	Blocked        []Classification
	NeedsAttention []Classification
}

// Classify is pure: given everything Enumerate learned about a branch, it
// decides the Group and a one-line reason. See the package-level "Test plan"
// comment above for every case this function must get right.
func Classify(info Info) Classification {
	if info.Protected {
		return Classification{Info: info, Group: Blocked, Reason: "protected default branch"}
	}
	if info.OpenPRNumber > 0 {
		return Classification{
			Info:   info,
			Group:  Blocked,
			Reason: fmt.Sprintf("open PR #%d is based on this branch — deleting it would break the PR", info.OpenPRNumber),
		}
	}
	if info.MergedOnServer {
		if info.WorktreePath != "" {
			return Classification{
				Info:  info,
				Group: SafeToDelete,
				Reason: fmt.Sprintf(
					"merged (server-confirmed via gh pr list --state merged); checked out at %s — the worktree will be removed before the branch is deleted",
					info.WorktreePath,
				),
			}
		}
		return Classification{Info: info, Group: SafeToDelete, Reason: "merged (server-confirmed via gh pr list --state merged)"}
	}
	if info.UpstreamGone {
		return Classification{
			Info:  info,
			Group: NeedsAttention,
			Reason: "upstream ref is gone but no merged PR was found server-side (local `--merged` ancestry is not trusted alone — " +
				"a squash merge is never a literal ancestor); verify manually before deleting",
		}
	}
	return Classification{Info: info, Group: Blocked, Reason: "no open or merged PR found; branch appears active"}
}

// ClassifyAll runs Classify over every info and groups the results into a
// Report.
func ClassifyAll(infos []Info) Report {
	var report Report
	for _, info := range infos {
		c := Classify(info)
		switch c.Group {
		case SafeToDelete:
			report.SafeToDelete = append(report.SafeToDelete, c)
		case NeedsAttention:
			report.NeedsAttention = append(report.NeedsAttention, c)
		default:
			report.Blocked = append(report.Blocked, c)
		}
	}
	return report
}
