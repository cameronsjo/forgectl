package launch

import (
	"os"

	"github.com/charmbracelet/huh"
	"golang.org/x/term"
)

// Choice is the resolved outcome of the interview.
type Choice struct {
	Model string
	Mode  SessionMode
}

// Interview presents the Model and Session selects, defaulted to the resolved
// profile, and returns the user's choice. It requires a TTY (huh renders an
// interactive form); callers should fall back to defaults when stdin is not a
// terminal (see IsInteractiveTTY).
func Interview(p Profile) (Choice, error) {
	model := p.Model
	session := "new"

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Model").
				Description("Resolved from your project profile — change it for this launch").
				Options(huh.NewOptions(modelChoices(p.Model)...)...).
				Value(&model),
			huh.NewSelect[string]().
				Title("Session").
				Options(
					huh.NewOption("New", "new"),
					huh.NewOption("Resume", "resume"),
					huh.NewOption("Fork (resume into a new session id)", "fork"),
				).
				Value(&session),
		),
	).WithTheme(huh.ThemeCharm())

	if err := form.Run(); err != nil {
		return Choice{}, err
	}
	return Choice{Model: model, Mode: sessionMode(session)}, nil
}

// IsInteractiveTTY reports whether both stdin and stdout are terminals — the
// gate for showing the interview instead of launching with resolved defaults.
func IsInteractiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// modelChoices returns the selectable models, defaulted to the standard aliases.
// A non-standard resolved model is prepended so it stays selectable.
func modelChoices(resolved string) []string {
	base := []string{"opus", "sonnet", "haiku"}
	if resolved == "" {
		return base
	}
	for _, m := range base {
		if m == resolved {
			return base
		}
	}
	return append([]string{resolved}, base...)
}

func sessionMode(s string) SessionMode {
	switch s {
	case "resume":
		return Resume
	case "fork":
		return Fork
	default:
		return New
	}
}
