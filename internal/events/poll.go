// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package events

import (
	"context"
	"time"
)

// PollResult is one poll: how many PRs were read, how many could not be,
// and the events the reads made.
type PollResult struct {
	Read    int     `json:"read"`
	Skipped int     `json:"skipped"`
	Events  []Event `json:"events"`
}

// Poll reads every watched PR once and records what changed. The reads run
// with no lock held, so a slow GitHub never blocks a session's watch or a
// reader; Record takes the lock once, at the end. A PR that cannot be read
// is skipped and keeps its last snapshot.
func Poll(ctx context.Context, st *Store, f Fetcher, now func() time.Time) (PollResult, error) {
	w, err := st.Watched()
	if err != nil {
		return PollResult{}, err
	}
	var res PollResult
	var snaps []PRState
	for _, ref := range w.Refs() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		snap, err := f.Fetch(ctx, ref)
		if err != nil {
			res.Skipped++
			continue
		}
		res.Read++
		snaps = append(snaps, snap)
	}
	if len(snaps) == 0 {
		return res, nil
	}
	evs, err := st.Record(now(), snaps)
	res.Events = evs
	return res, err
}
