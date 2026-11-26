package workerwatch

import (
	"slices"
	"time"
)

// Stream event types emitted by `forgectl surface watch --stream`.
// Every event carries strictly typed fields (names, addresses, states, PR
// numbers, check rollups, report filenames) and never raw pane text or
// external prose, so an agent subscribed via Monitor cannot be steered by
// untrusted terminal or PR content.
const (
	EventHCAvailable    = "watch.hc_available"
	EventHCUnavailable  = "watch.hc_unavailable"
	EventWorkerAppeared = "worker.appeared"
	EventWorkerGone     = "worker.gone"
	EventStatusChanged  = "worker.status_changed"
	EventReportComplete = "worker.report_complete"
	EventPRChanged      = "worker.pr_changed"
)

// StreamEvent is one NDJSON line written by `forgectl surface watch --stream`.
type StreamEvent struct {
	Time     time.Time `json:"ts"`
	Type     string    `json:"type"`
	Worker   string    `json:"worker,omitempty"`
	Address  string    `json:"address,omitempty"`
	State    string    `json:"state,omitempty"`
	Status   string    `json:"status,omitempty"`
	Prev     string    `json:"prev,omitempty"`
	NeedsYou bool      `json:"needs_you,omitempty"`
	Report   string    `json:"report,omitempty"`
	Repo     string    `json:"repo,omitempty"`
	PR       int       `json:"pr,omitempty"`
	PRState  string    `json:"pr_state,omitempty"`
	Checks   string    `json:"checks,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// Diff compares two consecutive snapshots and returns the typed transition
// events between prev and cur in deterministic order. Transient hc read
// errors on an individual worker (HCError != "" and not Gone) do not emit
// status flaps.
func Diff(prev, cur Snapshot) []StreamEvent {
	var out []StreamEvent
	ts := cur.Taken

	switch {
	case prev.HCUnavailable == "" && cur.HCUnavailable != "":
		out = append(out, StreamEvent{
			Time:   ts,
			Type:   EventHCUnavailable,
			Reason: cur.HCUnavailable,
		})
	case prev.HCUnavailable != "" && cur.HCUnavailable == "":
		out = append(out, StreamEvent{
			Time: ts,
			Type: EventHCAvailable,
		})
	}

	prevByName := make(map[string]Worker, len(prev.Workers))
	for _, w := range prev.Workers {
		prevByName[w.Name] = w
	}
	curByName := make(map[string]Worker, len(cur.Workers))
	for _, w := range cur.Workers {
		curByName[w.Name] = w
	}

	for _, curW := range cur.Workers {
		prevW, existed := prevByName[curW.Name]
		wasLive := existed && !prevW.Gone()
		isLive := !curW.Gone()

		if !wasLive && isLive {
			out = append(out, StreamEvent{
				Time:     ts,
				Type:     EventWorkerAppeared,
				Worker:   curW.Name,
				Address:  curW.Address,
				State:    curW.State,
				Status:   curW.Status,
				NeedsYou: curW.NeedsYou(),
			})
			continue
		}
		if wasLive && !isLive {
			out = append(out, StreamEvent{
				Time:    ts,
				Type:    EventWorkerGone,
				Worker:  curW.Name,
				Address: prevW.Address,
				Prev:    stateStatus(prevW),
			})
			continue
		}
		if !isLive {
			continue
		}

		// Both prev and cur have the worker present (not gone).
		if prevW.HCError == "" && curW.HCError == "" {
			prevKey := stateStatus(prevW)
			curKey := stateStatus(curW)
			if prevKey != curKey {
				out = append(out, StreamEvent{
					Time:     ts,
					Type:     EventStatusChanged,
					Worker:   curW.Name,
					Address:  curW.Address,
					State:    curW.State,
					Status:   curW.Status,
					Prev:     prevKey,
					NeedsYou: curW.NeedsYou(),
				})
			}
		}

		for _, name := range newlyCompletedReports(prevW.Reports, curW.Reports) {
			out = append(out, StreamEvent{
				Time:    ts,
				Type:    EventReportComplete,
				Worker:  curW.Name,
				Address: curW.Address,
				Report:  name,
			})
		}

		if curW.PR != nil && prChanged(prevW.PR, curW.PR) {
			out = append(out, StreamEvent{
				Time:    ts,
				Type:    EventPRChanged,
				Worker:  curW.Name,
				Address: curW.Address,
				Repo:    curW.PR.Repo,
				PR:      curW.PR.Number,
				PRState: curW.PR.State,
				Checks:  curW.PR.Checks,
			})
		}
	}

	// Workers present in prev (not gone) that dropped out of cur altogether.
	for _, prevW := range prev.Workers {
		if prevW.Gone() {
			continue
		}
		if _, ok := curByName[prevW.Name]; !ok {
			out = append(out, StreamEvent{
				Time:    ts,
				Type:    EventWorkerGone,
				Worker:  prevW.Name,
				Address: prevW.Address,
				Prev:    stateStatus(prevW),
			})
		}
	}

	return out
}

func stateStatus(w Worker) string {
	return w.State + "/" + w.Status
}

func newlyCompletedReports(prev, cur Reports) []string {
	donePrev := make(map[string]bool, len(prev.Files))
	for _, f := range prev.Files {
		if f.Complete {
			donePrev[f.Name] = true
		}
	}
	var added []string
	for _, f := range cur.Files {
		if f.Complete && !donePrev[f.Name] {
			added = append(added, f.Name)
		}
	}
	slices.Sort(added)
	return added
}

func prChanged(prev, cur *PR) bool {
	if prev == nil {
		return cur != nil
	}
	if cur == nil {
		return false
	}
	if prev.Repo != cur.Repo || prev.Number != cur.Number || prev.State != cur.State ||
		prev.Draft != cur.Draft || prev.Checks != cur.Checks {
		return true
	}
	return reviewSignature(prev.Review) != reviewSignature(cur.Review)
}

type reviewSig struct {
	present bool
	atHead  int
	crit    int
	imp     int
}

func reviewSignature(r *Review) reviewSig {
	if r == nil {
		return reviewSig{}
	}
	return reviewSig{present: true, atHead: r.AtHead, crit: r.Crit, imp: r.Imp}
}
