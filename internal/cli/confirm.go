package cli

import (
	"charm.land/huh/v2"

	"github.com/cameronsjo/forgectl/internal/keymap"
)

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
		WithTheme(keymap.DarkCharm()).
		Run()
	return ok, err
}

// confirmFn is confirm, exposed as a package-level var so tests can
// substitute a fake — huh.NewConfirm().Run() requires a real tty, which is
// exactly what a test doesn't have. Only clean.go's three call sites go
// through this var so far — branch.go, pr_findings.go, and tmux_kill.go
// still call confirm() directly, and their own apply⇒confirm⇒delete paths
// are exactly as untestable as clean's was. Migrating them is a real
// follow-up (a five-file refactor, not this fix), but it's out of scope
// for forgectl#165 — this var exists to make clean_test.go's
// apply⇒confirm⇒prune tests possible (item 3), not to be a universal seam
// yet.
var confirmFn = confirm
