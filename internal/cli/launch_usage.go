package cli

import (
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
)

// The seams the launch and resume paths call through. Tests swap them with
// t.Cleanup and never run in parallel, because they are package-level.
var (
	recordLaunchUsage = launch.RecordUsage
	execHarness       = launch.Exec
	usageNow          = time.Now
)

// recordUsageSilently is the only way the launch and resume paths touch usage
// statistics, and its whole job is to swallow the result.
//
// The silence is a contract, not laziness. Every byte those paths write is
// either the operator's own passthrough output or a warning they already
// depended on; `forgectl launch agents --help` must stay byte-identical
// whether collection is on, off, or failing. A recorder error printed to
// stderr would break that, and one routed to slog would break it too the
// moment log_file is "-" — which is exactly how a debugging session is
// configured. `launch stats` and `launch doctor` are where an operator asks
// what the store is doing.
func recordUsageSilently(enabled bool, ev launch.UsageEventV1) {
	_ = recordLaunchUsage(enabled, ev) //nolint:errcheck // silence is the contract; see above
}

// launchUsageClassification derives the two behavioral fields from the branch
// forgectl itself chose, never from the harness arguments.
//
// A native `codex resume …` or `claude --resume …` arriving through the opaque
// passthrough stays "unknown": forgectl did not build those arguments and has
// no business parsing them. Guessing would put an argv-derived value in a row
// that promises to carry none, and it would be wrong the first time a harness
// renamed a flag.
func launchUsageClassification(args []string) (sessionMode, posture string) {
	switch {
	case len(args) == 0:
		return launch.UsageSessionNew, launch.UsagePostureDefault
	case args[0] == "agents":
		return launch.UsageSessionUnknown, launch.UsagePostureAgents
	default:
		return launch.UsageSessionUnknown, launch.UsagePostureBuilder
	}
}

// newLaunchUsageEvent assembles a complete row. Only the resolved harness and
// model travel with it — never the profile match, the effort, the permission
// posture, the working directory, or anything derived from them.
func newLaunchUsageEvent(harness, model, sessionMode, posture string) launch.UsageEventV1 {
	return launch.UsageEventV1{
		SchemaVersion: launch.UsageSchemaVersion,
		TS:            launch.UsageTimestamp(usageNow()),
		Event:         launch.UsageEventExecAttempt,
		Harness:       harness,
		Model:         model,
		SessionMode:   sessionMode,
		Posture:       posture,
	}
}
