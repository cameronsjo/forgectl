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
// not colour doctor's exit code. Inspection creates nothing: doctor never makes
// the leaf, the lock, or the data file, and never repairs a store it refuses.
// It does tighten a mode broader than the store's own, because it reaches the
// store through the same safe opener the writer uses — so every path it
// tightened is named on its own line. A doctor that silently corrected a
// widened store would destroy the evidence an operator called it to see.
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
	// Printed before the verdict lines, and before any refusal return, because
	// a store can be narrowed on the leaf and still refused on a file below it.
	reportUsageNarrowing(out, status.Narrowed)
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

// reportUsageNarrowing names every path this inspection tightened.
//
// It warns rather than failing: by the time the line prints, the permissions
// are already back to the store's own, so the store itself is healthy and a
// failing exit code would report a problem that no longer exists. What the
// operator needs is the fact that something had widened it — one line per
// path, so a store widened before doctor ran is still legible afterwards.
func reportUsageNarrowing(out io.Writer, narrowed []string) {
	for _, path := range narrowed {
		fmt.Fprintf(out, "%s usage statistics: %s was more permissive than forgectl's own mode and has been tightened\n",
			launchWarnMark, termsafe.QuotePath(path))
	}
}
