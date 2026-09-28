package cli

import (
	"fmt"
	"io"

	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
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
func reportUsageStats(out io.Writer, enabled bool, marks theme.Marks) bool {
	return collectUsageStats(enabled, func(state doctor.State, detail string) {
		_, _ = fmt.Fprintf(out, "%s %s\n", doctorMark(state, marks), detail)
	})
}

// collectUsageStats is reportUsageStats without a writer: each line goes to
// emit as a state and its text, so `launch doctor --json` gets the same
// checks the terminal prints without parsing them back out of text.
func collectUsageStats(enabled bool, emit func(doctor.State, string)) bool {
	if !enabled {
		emit(doctor.StateWarn, "usage statistics: off (local-only opt-in; enable with [launch] usage_stats = true)")
		return true
	}

	status, err := launch.InspectUsage()
	if err != nil {
		emit(doctor.StateFail, "usage statistics: state path unusable: "+termsafe.SafeLine(err.Error()))
		return false
	}
	// Emitted before the verdict lines, and before any refusal return, because
	// a store can be narrowed on the leaf and still refused on a file below it.
	reportUsageNarrowing(status.Narrowed, emit)
	if status.Refusal != nil {
		emit(doctor.StateFail, fmt.Sprintf("usage statistics: on, but the store at %s was refused: %s",
			termsafe.QuotePath(status.Paths.Leaf), termsafe.SafeLine(status.Refusal.Error())))
		return false
	}
	if !status.DataPresent {
		emit(doctor.StateOK, "usage statistics: on, nothing recorded yet → "+termsafe.QuotePath(status.Paths.Data))
		return true
	}
	emit(doctor.StateOK, fmt.Sprintf("usage statistics: on → %s (read it with `forgectl launch stats`)",
		termsafe.QuotePath(status.Paths.Data)))
	return true
}

// reportUsageNarrowing names every path this inspection tightened.
//
// It warns rather than failing: by the time the line prints, the permissions
// are already back to the store's own, so the store itself is healthy and a
// failing exit code would report a problem that no longer exists. What the
// operator needs is the fact that something had widened it — one line per
// path, so a store widened before doctor ran is still legible afterwards.
func reportUsageNarrowing(narrowed []string, emit func(doctor.State, string)) {
	for _, path := range narrowed {
		emit(doctor.StateWarn, fmt.Sprintf("usage statistics: %s was more permissive than forgectl's own mode and has been tightened",
			termsafe.QuotePath(path)))
	}
}
