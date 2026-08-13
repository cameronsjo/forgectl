package cli

import (
	"fmt"
	"io"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// reportUsageStats prints the launch-statistics line and reports whether the
// store is healthy.
//
// Disabled is healthy — off is the default and a legitimate choice, so it must
// not colour doctor's exit code. Inspection is strictly read-only: doctor never
// creates the leaf, the lock, or the data file, and never repairs one it
// refuses. A doctor that half-fixed a store would destroy the evidence an
// operator called it to see.
func reportUsageStats(out io.Writer, enabled bool) bool {
	if !enabled {
		fmt.Fprintf(out, "%s usage statistics: off (local-only opt-in; enable with [launch] usage_stats = true)\n", launchWarnMark)
		return true
	}

	status, err := launch.InspectUsage()
	if err != nil {
		fmt.Fprintf(out, "%s usage statistics: state path unusable: %s\n", launchFailMark, termsafe.SafeLine(err.Error()))
		return false
	}
	if status.Refusal != nil {
		fmt.Fprintf(out, "%s usage statistics: on, but the store at %s was refused: %s\n",
			launchFailMark, termsafe.QuotePath(status.Paths.Leaf), termsafe.SafeLine(status.Refusal.Error()))
		return false
	}
	if !status.DataPresent {
		fmt.Fprintf(out, "%s usage statistics: on, nothing recorded yet → %s\n",
			launchOKMark, termsafe.QuotePath(status.Paths.Data))
		return true
	}
	fmt.Fprintf(out, "%s usage statistics: on → %s (read it with `forgectl launch stats`)\n",
		launchOKMark, termsafe.QuotePath(status.Paths.Data))
	return true
}
