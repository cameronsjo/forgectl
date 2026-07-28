package cli

import "github.com/charmbracelet/huh"

// confirm shows a yes/no prompt for destructive actions. It returns the
// user's choice; an error means the prompt couldn't run (e.g. no tty) or was
// aborted. NOT every caller offers a way to skip it: tmux_kill and update
// each gate this behind their own --yes flag, but clean has no such flag —
// every one of its destructive passes always confirms first.
func confirm(prompt string) (bool, error) {
	ok := false
	err := huh.NewConfirm().
		Title(prompt).
		Affirmative("Yes").
		Negative("No").
		Value(&ok).
		Run()
	return ok, err
}

// confirmFn is confirm, exposed as a package-level var so tests can
// substitute a fake — huh.NewConfirm().Run() requires a real tty, which is
// exactly what a test doesn't have. Production callers go through this var
// (never confirm directly) so the fix-round apply⇒confirm⇒prune tests in
// clean_test.go can pin that a Yes reaches the prune commands and a No
// never does (forgectl#165 item 3 — previously untestable, and untested).
var confirmFn = confirm
