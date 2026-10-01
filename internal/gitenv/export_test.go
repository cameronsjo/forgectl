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

// SetUnfilteredDeadline shortens RunUnfiltered's deadline for t.
func SetUnfilteredDeadline(t interface{ Cleanup(func()) }, d time.Duration) {
	old := unfilteredDeadline
	unfilteredDeadline = d
	t.Cleanup(func() { unfilteredDeadline = old })
}
