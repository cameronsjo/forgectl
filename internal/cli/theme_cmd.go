package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// themeModule declares the theme extension (ADR-0005). It exists to CLAIM the
// [theme] config section as much as to render anything: the registry requires
// every struct-kind section to be owned by exactly one module, and the
// alternative — a second `sharedSections` exception — was considered and
// declined, because a section nobody owns is a section nobody can show you.
//
// The verbs are that ownership made useful. `theme show` answers "what colour
// is this role, and where did it come from"; `theme preview` answers "what
// does that actually look like".
var themeModule = module.Manifest{
	Name:      "theme",
	Tier:      module.TierExtension,
	ConfigKey: "theme",
	New:       newThemeCmd,
}

func newThemeCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "theme",
		Short: "Inspect the resolved colour theme",
		Long: `theme reports the colours every styled surface in forgectl draws from.

  forgectl theme show          resolved hex per role, with provenance
  forgectl theme show --json   the same, machine-readable
  forgectl theme preview       render each role so you can see it

Colours come from the Artificer terminal palette — the same source as the
ghostty, tmux and gitmux themes — and are overridable per role in the [theme]
section of config.toml.`,
	}
	cmd.AddCommand(newThemeShowCmd(deps.Theme), newThemePreviewCmd(deps.Theme))
	return cmd
}

// themeRoleJSON is one role's wire shape for `theme show --json`.
type themeRoleJSON struct {
	Role       string  `json:"role"`
	Hex        string  `json:"hex"`
	Provenance string  `json:"provenance"`
	Contrast   float64 `json:"contrast"`
	BelowAA    bool    `json:"below_aa"`
}

// themeShowJSON is `theme show --json`'s stdout wire shape.
type themeShowJSON struct {
	Preset   string          `json:"preset"`
	Mode     string          `json:"mode"`
	Dark     bool            `json:"dark"`
	Roles    []themeRoleJSON `json:"roles"`
	Warnings []string        `json:"warnings"`
}

func newThemeShowCmd(th theme.Theme) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the resolved colour for every role",
		Long: `show prints each role's resolved hex, where that value came from, and its
contrast against the surface it is actually drawn on.

Contrast is the WCAG 2.x ratio. Most roles are measured against the background;
the on-fill roles are measured against their fill, because that is where they
are drawn — onaccent sits on accentfill, never on the page.

A role is flagged "below AA" only when it is under 4.5:1 AND carries text. A
fill or a raised surface is not text and is owed no such floor, so it is never
flagged: the ratio a colour must clear follows how it is used, not its hue.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return writeThemeJSON(cmd.OutOrStdout(), th)
			}
			return printThemeShow(colorOut(cmd), th)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable report to stdout")
	return cmd
}

// themeRows builds the per-role report both output paths render.
func themeRows(th theme.Theme) []themeRoleJSON {
	names := theme.RoleNames()
	rows := make([]themeRoleJSON, 0, len(names))
	for i, name := range names {
		r := theme.Role(i)
		ratio := th.Contrast(r)
		rows = append(rows, themeRoleJSON{
			Role:       name,
			Hex:        th.Hex(r),
			Provenance: th.Provenance(r),
			Contrast:   ratio,
			BelowAA:    theme.CarriesText(r) && ratio < 4.5,
		})
	}
	return rows
}

func writeThemeJSON(out io.Writer, th theme.Theme) error {
	rep := themeShowJSON{
		Preset:   th.PresetName(),
		Mode:     th.Mode().String(),
		Dark:     th.IsDark(),
		Roles:    themeRows(th),
		Warnings: th.Warnings(),
	}
	if rep.Warnings == nil {
		rep.Warnings = []string{}
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func printThemeShow(out io.Writer, th theme.Theme) error {
	mode := "light"
	if th.IsDark() {
		mode = "dark"
	}
	if _, err := fmt.Fprintf(out, "preset %s · mode %s (resolved %s)\n\n", th.PresetName(), th.Mode(), mode); err != nil {
		return err
	}
	styles := th.Styles()
	for _, row := range themeRows(th) {
		flag := ""
		if row.BelowAA {
			flag = styles.Muted.Render("  below AA as text")
		}
		line := fmt.Sprintf("%-14s %-9s %-9s %5.2f:1", row.Role, row.Hex, row.Provenance, row.Contrast)
		if _, err := fmt.Fprintln(out, styles.Fg.Render(line)+flag); err != nil {
			return err
		}
	}
	for _, w := range th.Warnings() {
		if _, err := fmt.Fprintf(out, "\n%s %s\n", th.Marks().Warn, termsafe.SafeLine(w)); err != nil {
			return err
		}
	}
	return nil
}

func newThemePreviewCmd(th theme.Theme) *cobra.Command {
	return &cobra.Command{
		Use:   "preview",
		Short: "Render every role so you can see it",
		Long: `preview renders a swatch and a sample line for every role.

It deliberately breaks the design system's two-colour rule: a swatch sheet's
whole job is to show every colour at once, which is the one context where that
rule does not apply.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printThemePreview(colorOut(cmd), th)
		},
	}
}

func printThemePreview(out io.Writer, th theme.Theme) error {
	styles := th.Styles()
	marks := th.Marks()

	if _, err := fmt.Fprintf(out, "%s  %s\n\n",
		styles.Brand.Render("forgectl"), styles.Muted.Render("theme preview · "+th.PresetName())); err != nil {
		return err
	}

	names := theme.RoleNames()
	for i, name := range names {
		r := theme.Role(i)
		swatch := th.Style(r).Render("████")
		if _, err := fmt.Fprintf(out, "%s  %-14s %s\n", swatch, name, styles.Muted.Render(th.Hex(r))); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(out, "\n%s ok   %s warn   %s fail   %s skip\n",
		marks.OK, marks.Warn, marks.Fail, marks.Skip); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "\n%s  %s  %s\n",
		styles.Selected.Render("▌selected row"),
		styles.Steel.Render("a/session/path"),
		styles.Meta.Render("2 windows · metadata"))
	return err
}

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
		fmt.Fprintln(os.Stderr, "forgectl: "+termsafe.SafeLine("[theme] ignored: "+err.Error()))
		return theme.Default()
	}
	return theme.New(opts, theme.Detect(opts.Mode, theme.Env{}, nil))
}
