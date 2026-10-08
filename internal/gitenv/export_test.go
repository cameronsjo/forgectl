package gitenv

import "time"

// ErrGitfileNotRegular lets the external tests tell the not-a-regular-file
// refusal from the others.
var ErrGitfileNotRegular = errGitfileNotRegular

// ErrHeadNotRegular and ErrUnfilteredDeadline let the external tests tell
// those refusals from the others.
var (
	ErrHeadNotRegular     = errHeadNotRegular
	ErrUnfilteredDeadline = errUnfilteredDeadline
)

// SetRepoDeadline shortens repoDeadline for t.
func SetRepoDeadline(t interface{ Cleanup(func()) }, d time.Duration) {
	old := repoDeadline
	repoDeadline = d
	t.Cleanup(func() { repoDeadline = old })
}
