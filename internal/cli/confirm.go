package cli

import "github.com/charmbracelet/huh"

// confirm shows a yes/no prompt for destructive actions. It returns the user's
// choice; an error means the prompt couldn't run (e.g. no tty) or was aborted.
// Callers pass --yes to skip it entirely.
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
