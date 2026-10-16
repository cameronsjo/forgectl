package sweep

import (
	"encoding/json"
	"fmt"
)

// SchemaVersion is the ledger version. Version 1 is the dashboard.json the
// steward mod read before; version 2 adds prs, opinion actions and worker
// panes. A v1 reader ignores the fields it does not know.
const SchemaVersion = 2

// PR states, as the attention queue reads them.
const (
	StateBlocked     = "blocked"
	StateMarkerClean = "marker-clean"
	StateWaiting     = "waiting"
	StateDraft       = "draft"
)

// Opinion actions.
const (
	ActionReview  = "review"
	ActionUnblock = "unblock"
	ActionNone    = "none"
)

// Opinion tones, urgent first.
const (
	ToneAct  = "act"
	ToneWarn = "warn"
	ToneNote = "note"
	ToneGood = "good"
)

// Worker phases, in order.
const (
	PhaseLaunched = "launched"
	PhasePlan     = "plan"
	PhaseGo       = "go"
	PhaseReport   = "report"
)

// Ledger is sweep.json.
type Ledger struct {
	SchemaVersion   int         `json:"schemaVersion"`
	GeneratedAt     string      `json:"generatedAt"`
	Merged          int         `json:"merged"`
	DependencyBumps int         `json:"dependencyBumps"`
	ReposTouched    int         `json:"reposTouched"`
	SweepRepos      int         `json:"sweepRepos"`
	OpenPRs         int         `json:"openPrs"`
	Filed           int         `json:"filed"`
	Opinions        []Opinion   `json:"opinions"`
	Workers         []Worker    `json:"workers"`
	TopRepos        []RepoCount `json:"topRepos"`
	Recent          []Recent    `json:"recent"`
	PRs             []PR        `json:"prs"`
	Sources         Sources     `json:"sources"`
}

// Opinion is one computed observation. Action says what the operator can
// start from it; Target says on what.
type Opinion struct {
	ID     string  `json:"id"`
	Tone   string  `json:"tone"`
	Text   string  `json:"text"`
	Action string  `json:"action"`
	Target *Target `json:"target,omitempty"`
}

// Target is a PR (Repo and Number) or a worker (Worker).
type Target struct {
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	Worker string `json:"worker,omitempty"`
}

// Worker is one worker's row. Repos are short names (the v1 field); PRs are
// its open PRs by owner/repo and number.
type Worker struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Phase     string   `json:"phase"`
	Repos     []string `json:"repos"`
	Pane      string   `json:"pane,omitempty"`
	PRs       []Ref    `json:"prs"`
	BlockedBy string   `json:"blockedBy,omitempty"`
}

// Ref names a PR.
type Ref struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// PR is one open PR in a swept repository.
type PR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Reason string `json:"reason"`
	Worker string `json:"worker"`
}

// RepoCount is a repository and its merge count; it marshals as the pair
// [name, count] the v1 ledger used.
type RepoCount struct {
	Repo  string
	Count int
}

// MarshalJSON writes [repo, count].
func (r RepoCount) MarshalJSON() ([]byte, error) {
	// termsafe:allow-raw-json one repository name and a count; the name is a validated GitHub name
	return json.Marshal([]any{r.Repo, r.Count})
}

// UnmarshalJSON reads [repo, count].
func (r *RepoCount) UnmarshalJSON(data []byte) error {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(data, &pair); err != nil {
		return fmt.Errorf("topRepos entry is not [repo, count]: %w", err)
	}
	if err := json.Unmarshal(pair[0], &r.Repo); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &r.Count)
}

// Recent is one recent merge (the v1 field set).
type Recent struct {
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	ClosedAt string `json:"closedAt"`
}

// Sources says how each input read went: "ok", "absent", "error",
// "unavailable" (herdr could not be asked) or "truncated" (GitHub returned a
// full page limit).
type Sources struct {
	GitHub string `json:"github"`
	Herdr  string `json:"herdr"`
	Desk   string `json:"desk"`
	Briefs string `json:"briefs"`
}
