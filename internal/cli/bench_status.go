package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/module"
)

// newBenchStatusCmd builds `forgectl bench status [--json]`. The Claude-callable
// contract mirrors `projects list`: `--json` emits the raw report to stdout; the
// human card also goes to stdout (it is the payload, not a diagnostic). It is a
// read-only health card — a down component is reported, never a non-zero exit.
func newBenchStatusCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Health card across hearth, chronicle, and flux",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := bench.Status(cmd.Context(), deps.Cfg, deps.Runner, bench.NewHTTPProber())
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			renderBenchReport(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	return cmd
}

// renderBenchReport writes the human health card: one glyph-led line per
// component plus its indented probe details.
func renderBenchReport(out io.Writer, r bench.Report) {
	for _, c := range []bench.Component{r.Hearth, r.Chronicle, r.Flux} {
		fmt.Fprintf(out, "%s %s — %s\n", benchGlyph(c.State), c.Name, c.Reason)
		for _, d := range c.Details {
			fmt.Fprintf(out, "    %s\n", d)
		}
	}
}

// benchGlyph maps a component State onto the launch_doctor glyph vocabulary:
// ✓ healthy, ! needs attention (degraded or not-yet-configured), ✗ down.
func benchGlyph(s bench.State) string {
	switch s {
	case bench.StateOK:
		return launchOKMark
	case bench.StateUnavailable:
		return launchFailMark
	default: // degraded, not-configured
		return launchWarnMark
	}
}
