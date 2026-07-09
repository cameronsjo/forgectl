package pr

// State is a PR's review disposition as the discovery layer sees it. It is the
// single vocabulary both the picker's skip and the dashboards' dim speak, so
// the two can never drift.
type State int

const (
	// StateNeedsReview is the default: no reviewed mark at or after the PR's
	// latest activity.
	StateNeedsReview State = iota
	// StateReviewed means the local store holds a mark at or after the PR's
	// latest activity (a fresh push moves it back to StateNeedsReview).
	StateReviewed
)

// ApprovalState is the one shared authority for "is this PR done?". It is thin
// today — a delegate to the reviewed store's timestamp comparison — but it is
// deliberately the sole call site both the picker's skip and the dashboards'
// dim invoke, so a later fold (e.g. GitHub's reviewDecision) lands in exactly
// one place. A nil store means nothing is reviewed.
func ApprovalState(pr PR, store *ReviewedStore) State {
	if store != nil && store.IsReviewed(pr.Ref, pr.UpdatedAt) {
		return StateReviewed
	}
	return StateNeedsReview
}

// Dimmed reports whether pr should render dimmed / be skipped — true iff
// ApprovalState is StateReviewed. Both the picker's launch skip and the
// dashboards' row dimming call this, so their verdicts are identical by
// construction.
func Dimmed(pr PR, store *ReviewedStore) bool {
	return ApprovalState(pr, store) == StateReviewed
}
