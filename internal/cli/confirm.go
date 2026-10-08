package cli

import (
	"charm.land/huh/v2"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// confirm shows a yes/no prompt for destructive actions. It returns the
// user's choice; an error means the prompt couldn't run (e.g. no tty) or was
// aborted. NOT every caller offers a way to skip it: tmux_kill and update
// each gate this behind their own --yes flag, but clean has no such flag —
// every one of its destructive passes always confirms first.
func confirm(th theme.Theme, prompt string) (bool, error) {
	ok := false
	err := confirmForm(th, prompt, &ok).Run()
	return ok, err
}

// confirmForm builds the prompt's form. Split from confirm so a test can feed
// it keys without a tty. Esc and Ctrl+C both cancel (keymap.Cancel), and the
// description says so, because huh's own help line lists only the toggle and
// submit keys.
func confirmForm(th theme.Theme, prompt string, ok *bool) *huh.Form {
	return keymap.Suspendable(huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(prompt).
			Description("esc to cancel").
			Affirmative("Yes").
			Negative("No").
			Value(ok),
	))).WithKeyMap(keymap.Cancel()).WithShowHelp(true).WithTheme(th.Huh())
}

// confirmFn is confirm, exposed as a package-level var so tests can
// substitute a fake — huh.NewConfirm().Run() requires a real tty, which is
// exactly what a test doesn't have. clean.go's three call sites and
// pr_findings.go's cleanup go through this var so far — branch.go and
// tmux_kill.go still call confirm() directly, and their own apply⇒confirm⇒delete paths
// are exactly as untestable as clean's was. Migrating them is a real
// follow-up (a five-file refactor, not this fix), but it's out of scope
// for forgectl#165 — this var exists to make clean_test.go's
// apply⇒confirm⇒prune tests possible (item 3), not to be a universal seam
// yet.
var confirmFn = confirm
