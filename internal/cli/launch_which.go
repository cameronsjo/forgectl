package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func newLaunchWhichCmd(boundary *config.LegacyMigrationBoundary, cfg config.Config, th theme.Theme) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "which",
		Short: "Print the resolved launch profile for the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return termsafe.Error(fmt.Errorf("determine working directory: %w", err))
			}
			effLaunch, notice, effFrom := autoMigrateOrWarnLegacyLaunch(boundary, cfg)
			if notice != "" && !asJSON {
				fmt.Fprintln(cmd.ErrOrStderr(), "forgectl: "+termsafe.SafeLine(notice))
			}
			cfg.Launch = effLaunch
			lc, src := resolveLaunchConfig(boundary, cfg, effFrom)
			profile := launch.Resolve(lc, cwd)
			if asJSON {
				return writeLaunchWhichJSON(cmd.OutOrStdout(), profile, cwd, src)
			}
			out := th.Writer(cmd.OutOrStdout(), os.Environ())
			printLaunchProfile(out, th, profile, cwd, src)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"directory":...,"config":...,"matched":...,"harness":...,"model":...,"effort":...,"permission_mode":...,"allow_danger":...,"env_keys":[...],"add_dir":[...]} to stdout`)
	return cmd
}

// launchWhichJSON is the --json wire shape for `launch which`. Env is
// represented only by its sorted key NAMES — the same key-names-only rule
// printLaunchProfile's terminal row applies, because `which` output (human or
// machine) is the kind of thing pasted into an issue or an agent transcript,
// and a configured env value is exactly where a secret lives.
type launchWhichJSON struct {
	Directory      string   `json:"directory"`
	Config         string   `json:"config"`
	Matched        string   `json:"matched"`
	Harness        string   `json:"harness"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	PermissionMode string   `json:"permission_mode"`
	AllowDanger    bool     `json:"allow_danger"`
	EnvKeys        []string `json:"env_keys"`
	AddDir         []string `json:"add_dir"`
}

// buildLaunchWhichJSON converts a resolved profile into the --json wire
// shape. Slice fields are never nil so the encoder emits [] rather than null
// for a profile with no env or no add-dir entries.
func buildLaunchWhichJSON(p launch.Profile, cwd, confPath string) launchWhichJSON {
	envKeys := launch.SortedEnvKeys(p.Env)
	if envKeys == nil {
		envKeys = []string{}
	}
	addDir := p.AddDir
	if addDir == nil {
		addDir = []string{}
	}
	return launchWhichJSON{
		Directory:      cwd,
		Config:         confPath,
		Matched:        p.Match,
		Harness:        p.Harness,
		Model:          p.Model,
		Effort:         p.Effort,
		PermissionMode: p.PermissionMode,
		AllowDanger:    p.AllowDanger,
		EnvKeys:        envKeys,
		AddDir:         addDir,
	}
}

// writeLaunchWhichJSON encodes the profile through the sanctioned termsafe
// seam. Nothing is written before a marshal error, so a failing writer or
// encoder never leaves a partial document on stdout.
func writeLaunchWhichJSON(w io.Writer, p launch.Profile, cwd, confPath string) error {
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(buildLaunchWhichJSON(p, cwd, confPath))
}

func printLaunchProfile(w io.Writer, th theme.Theme, p launch.Profile, cwd, confPath string) {
	styles := th.Styles()
	labelStyle := styles.Muted.Width(14)
	valueStyle := styles.Fg
	titleStyle := styles.Accent
	dimStyle := styles.Muted.Italic(true)

	row := func(label, value string) {
		_, _ = fmt.Fprintln(w, renderSafe(labelStyle.Render, label)+renderSafe(valueStyle.Render, value))
	}
	rowDim := func(label, value string) {
		_, _ = fmt.Fprintln(w, renderSafe(labelStyle.Render, label)+renderSafe(dimStyle.Render, value))
	}

	_, _ = fmt.Fprintln(w, titleStyle.Render("launch profile")+renderSafe(dimStyle.Render, "  "+cwd))
	row("config", confPath)

	matched := p.Match
	if matched == "" {
		rowDim("matched", "(defaults only)")
	} else {
		row("matched", matched)
	}
	row("harness", p.Harness)
	if p.Harness == "pi" && p.Provider != "" {
		row("provider", p.Provider)
	}
	row("model", p.Model)
	switch p.Harness {
	case "codex":
		row("approval", p.ApprovalPolicy)
		row("sandbox", p.Sandbox)
	case "claude":
		// Omitted rather than shown empty when no level resolved: an absent
		// --effort means Claude Code's own default applies, which is a
		// different statement from "effort is blank". Matches the conditional
		// env/add-dir rows below.
		if p.Effort != "" {
			row("effort", p.Effort)
		}
		row("permission", p.PermissionMode)
		row("allow danger", fmt.Sprintf("%t", p.AllowDanger))
	}

	// Env renders its sorted KEY NAMES only, never its values: this is arbitrary
	// environment injected into the launched harness, so it is where an
	// ANTHROPIC_API_KEY or GH_TOKEN sits, and `which` output is the kind of
	// thing pasted into an issue or a terminal share. The key names carry the
	// signal an operator needs here — which variables the profile injects.
	// Same policy, same rendering as the sibling surface: leafValue's
	// reflect.Map arm in internal/cli/config_cmd.go.
	if len(p.Env) > 0 {
		row("env", redactedMapDisplay(launch.SortedEnvKeys(p.Env)))
	}
	for i, d := range p.AddDir {
		label := ""
		if i == 0 {
			label = "add-dir"
		}
		row(label, d)
	}
}

// renderSafe establishes the ordering invariant for styled terminal output:
// untrusted text is escaped first, then the trusted renderer may add ANSI.
func renderSafe(render func(...string) string, untrusted string) string {
	return render(termsafe.SafeLine(untrusted))
}
