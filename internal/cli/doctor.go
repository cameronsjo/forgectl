package cli

import (
	"fmt"
	"io"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// doctorSkipMark renders a StateSkip check — mirrors launch_doctor.go's
// launchOKMark/launchWarnMark/launchFailMark (same package, reused as-is for
// ok/warn/fail here) with one addition: skip needs its own neutral glyph,
// since "not configured" is deliberately distinct from both "ok" and "warn".
var doctorSkipMark = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("-")

// doctorModule declares the ecosystem-health-check extension (ADR-0005): no
// config section of its own — every check reads a section another module
// already owns (bench, launch, update).
var doctorModule = module.Manifest{
	Name:      "doctor",
	Tier:      module.TierExtension,
	ConfigKey: "",
	New:       newDoctorCmd,
}

// newDoctorCmd builds `forgectl doctor` over the registry Deps.
func newDoctorCmd(deps module.Deps) *cobra.Command {
	return newDoctorCmdForDeps(doctor.NewDeps(deps.Cfg, deps.Runner))
}

// newDoctorCmdForDeps builds the command over an already-constructed
// doctor.Deps — split out so tests can inject fakes (mirrors
// newUpdateCmdForClient) without going through the full module.Deps wiring.
func newDoctorCmdForDeps(d doctor.Deps) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the whole workbench is wired: claude, tmux, ghostty, gh, config, the local bench, and forgectl itself",
		Long: `doctor is forgectl's ecosystem health check. It orchestrates the checks
each domain already knows how to make — it never reimplements one:

  claude          claude on PATH (or [launch.defaults].binary_path / $FORGECTL_CLAUDE_BIN)
  config          config.toml present and parseable
  log path        the resolved log path's parent directory exists or can be created
  tmux/ghostty/cmux   present on PATH
  gh              installed and authenticated
  hearth/chronicle        the local bench's health (forgectl bench status)
  trust store     the workflow-blessing trust store, if you use blessed workflows
  forgectl version   the Homebrew tap's reachability + forgectl's own currency

Every check runs independently — one failing check never hides another. An
unconfigured optional integration (no bench dir, no trust store) reports "-"
(skipped), not a failure.

Exit codes: 0 every check ok (or skipped), 1 at least one check failed.`,
		Args: cobra.NoArgs,
		// SilenceUsage/SilenceErrors mirror update.go's identical setting —
		// --json is spec'd to emit ONLY the machine-readable report on
		// stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := doctor.Run(cmd.Context(), d)
			if asJSON {
				// Raw stdout, not colorOut: --json is a machine-readable
				// payload and must never pass through a colour writer, the
				// same split pr_prs.go and review_list.go make.
				if err := writeDoctorJSON(cmd.OutOrStdout(), report); err != nil {
					return WithExitCode(err, 2)
				}
			} else {
				if err := printDoctorReport(colorOut(cmd), report); err != nil {
					return WithExitCode(err, 2)
				}
			}
			if !report.Healthy() {
				return WithExitCode(fmt.Errorf("doctor found problems"), 1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable report to stdout")
	return cmd
}

// doctorMark returns the glyph for one Check's State — StateOK/Warn/Fail
// reuse launch_doctor.go's existing marks (same package) so the two doctor
// surfaces render identically; StateSkip gets its own neutral glyph.
func doctorMark(s doctor.State) string {
	switch s {
	case doctor.StateOK:
		return launchOKMark
	case doctor.StateWarn:
		return launchWarnMark
	case doctor.StateFail:
		return launchFailMark
	default: // doctor.StateSkip
		return doctorSkipMark
	}
}

// printDoctorReport writes one line per Check — glyph, name, detail — with
// the hint (when present) on an indented line underneath, mirroring
// launch_doctor.go's own per-line shape.
//
// Detail and Hint carry captured output from external commands (brew's
// outdated line, a CommandError's stderr) — an untrusted tap or a
// compromised gh session could embed a newline/CR or an ANSI CSI sequence
// there to forge an additional well-formed check line, or overwrite a real
// failure's line via a cursor-control escape. SafeLine visibly escapes both
// classes at this exact display boundary — the one place every check's output
// converges before reaching a terminal. The --json path needs no such
// treatment: encoding/json already escapes control bytes.
func printDoctorReport(out io.Writer, report doctor.Report) error {
	for _, c := range report.Checks {
		if _, err := fmt.Fprintf(out, "%s %-18s %s\n", doctorMark(c.State), c.Name, termsafe.SafeLine(c.Detail)); err != nil {
			return err
		}
		if c.Hint != "" {
			if _, err := fmt.Fprintf(out, "  %s\n", termsafe.SafeLine(c.Hint)); err != nil {
				return err
			}
		}
	}
	return nil
}

// doctorCheckJSON is one Check's --json wire shape.
type doctorCheckJSON struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// doctorReportJSON is `doctor --json`'s stdout wire shape.
type doctorReportJSON struct {
	Checks  []doctorCheckJSON `json:"checks"`
	Healthy bool              `json:"healthy"`
}

// writeDoctorJSON encodes report as doctorReportJSON to out.
func writeDoctorJSON(out io.Writer, report doctor.Report) error {
	j := doctorReportJSON{Checks: make([]doctorCheckJSON, 0, len(report.Checks)), Healthy: report.Healthy()}
	for _, c := range report.Checks {
		j.Checks = append(j.Checks, doctorCheckJSON{Name: c.Name, State: string(c.State), Detail: c.Detail, Hint: c.Hint})
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(j)
}
