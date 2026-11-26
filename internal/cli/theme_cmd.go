package cli

import (
	"fmt"
	"os"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// resolveTheme builds the Theme every module receives.
//
// It never fails the run. A bad [theme] is already reported by doctor and
// launch doctor through config.ValidatePath; refusing to start over a colour
// would trade a cosmetic problem for an unusable binary, so a rejected section
// warns once on stderr and falls back to the default.
//
// Mode resolution takes NO probe here. This runs on the plain-print path,
// where the destination is usually a pipe and an OSC 11 query would either go
// unanswered or land as stray bytes in the output. Surfaces that can safely
// ask — the Bubble Tea program, a huh form — probe themselves and rebuild via
// Theme.WithDark.
func resolveTheme(cfg config.Config) theme.Theme {
	opts, err := theme.FromConfig(cfg.Theme)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forgectl: "+safeText("[theme] ignored: "+err.Error()))
		return theme.Default()
	}
	return theme.New(opts, theme.Detect(opts.Mode, theme.Env{}, nil))
}
