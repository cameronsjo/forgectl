package workstate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// ForQueueRow maps a drain queue row. A queue state that is terminal for the
// queue is not always an end of the work: reported is delivered and failed is
// blocked, because someone still has to verify or decide.
func ForQueueRow(r worker.QueueRow) Work {
	switch r.State {
	case worker.QueueQueued:
		return Work{Status: Queued}
	case worker.QueueClaimed, worker.QueueLaunched:
		return Work{Status: Working}
	case worker.QueueNeedsYou:
		return Work{Status: Blocked, On: Operator, Reason: "worker at a prompt or idle with no report"}
	case worker.QueueReported:
		if r.PRClosedAt != nil {
			return Work{Status: Delivered, On: Operator, Reason: "PR closed unmerged; the drain closes the worker after a grace period"}
		}
		return Work{Status: Delivered, On: verifierFor(r.Batch), Reason: "review the report and PR"}
	case worker.QueueFailed:
		return Work{Status: Blocked, On: Operator, Reason: "failed: retry or drop"}
	case worker.QueueClosed:
		if n, ok := drain.MergedPR(r.LastError); ok {
			return Work{Status: Resolved, Evidence: fmt.Sprintf("merged PR #%d", n)}
		}
		if r.PRClosedAt != nil {
			return Work{Status: Dropped, Reason: "PR closed unmerged"}
		}
		return Work{Status: Dropped, Reason: "closed with no merge recorded"}
	case worker.QueueExpired:
		return Work{Status: Expired, Reason: "sat queued too long and was never launched"}
	}
	return Work{Status: Stalled, Reason: "unknown queue state " + string(r.State)}
}

// operatorPauses are the pause kinds the drain cannot clear by itself: each
// needs a fix and the next drain start. The others retry every tick.
var operatorPauses = []drain.PauseKind{drain.PauseConfig, drain.PauseGitHubAuth, drain.PauseLaunchConfig}

// ForDrain maps the drain process. status is drain.Effective's value,
// pauseReason the joined "kind: reason; …" text drain.json records, and
// waiting the rows that are queued or live. A stopped drain with nothing
// waiting makes no claim: the zero Work, which callers omit.
func ForDrain(status, pauseReason string, waiting int) (Work, bool) {
	switch status {
	case drain.StatusRunning:
		return Work{Status: Working}, true
	case drain.StatusPaused:
		kinds := pauseKinds(pauseReason)
		on := System
		for _, k := range kinds {
			// An unknown kind is a newer binary's: the operator reads it.
			if slices.Contains(operatorPauses, drain.PauseKind(k)) || !knownPause(k) {
				on = Operator
			}
		}
		if len(kinds) == 0 {
			on = Operator
		}
		reason := "paused"
		if len(kinds) > 0 {
			reason = "paused: " + strings.Join(kinds, ", ")
		}
		return Work{Status: Blocked, On: on, Reason: reason}, true
	case drain.StatusStale:
		return Work{Status: Stalled, Reason: "the drain's last tick is stale"}, true
	case drain.StatusStopped:
		if waiting > 0 {
			return Work{Status: Blocked, On: Operator, Reason: "drain stopped with rows waiting"}, true
		}
		return Work{}, false
	}
	return Work{Status: Stalled, Reason: "unknown drain status " + status}, true
}

func knownPause(k string) bool {
	switch drain.PauseKind(k) {
	case drain.PauseConfig, drain.PauseHerdr, drain.PauseGitHubAuth, drain.PauseLaunchConfig, drain.PauseGitHub:
		return true
	}
	return false
}

// pauseKinds reads the kind prefixes out of drain.Pauses.Reason's
// "kind: reason; kind: reason" form. A reason may itself hold "; ", so only
// a part that starts with a word and ": " counts as a new kind.
func pauseKinds(reason string) []string {
	var kinds []string
	for part := range strings.SplitSeq(reason, "; ") {
		k, _, ok := strings.Cut(part, ": ")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// ForDeskItem maps a desk item. A done item ended its run, but only exit 0
// met its goal: any other exit leaves it blocked on the operator until they
// rerun or prune it.
func ForDeskItem(it desk.Item) Work {
	switch it.State {
	case desk.StateWaiting:
		return Work{Status: Blocked, On: Operator, Reason: "approve or skip"}
	case desk.StateRefused:
		reason := "refused"
		if it.Refusal != "" {
			reason = "refused: " + it.Refusal
		}
		return Work{Status: Blocked, On: Operator, Reason: reason}
	case desk.StateHeld:
		// It turns waiting on its own once what holds it clears.
		reason := "held"
		if it.Hold != "" {
			reason = "held: " + it.Hold
		}
		return Work{Status: Queued, Reason: reason}
	case desk.StateRunning:
		return Work{Status: Working}
	case desk.StateLost:
		return Work{Status: Stalled, Reason: "run lost: its owner died before it ended"}
	case desk.StateDone:
		switch {
		case it.ExitCode == nil:
			return Work{Status: Blocked, On: Operator, Reason: "ended with no exit code recorded"}
		case *it.ExitCode == 0:
			return Work{Status: Resolved, Evidence: "exit 0"}
		default:
			return Work{Status: Blocked, On: Operator, Reason: fmt.Sprintf("failed: exit %d", *it.ExitCode)}
		}
	case desk.StateSkipped:
		reason := "skipped"
		if it.Meta.SkipReason != "" {
			reason = "skipped: " + it.Meta.SkipReason
		}
		return Work{Status: Dropped, Reason: reason}
	}
	return Work{Status: Stalled, Reason: "unknown desk state " + string(it.State)}
}
