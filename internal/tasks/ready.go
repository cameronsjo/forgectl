package tasks

import "sort"

// Ready returns the open (not Done) tasks among all that carry no active
// "blocked" relation, ranked by Vikunja's own Position field — the `bd
// ready` semantic, reimplemented on Vikunja's relation graph rather than a
// bespoke dependency store. Pure: no I/O, no network, no clock.
func Ready(all []Task) []Task {
	out := make([]Task, 0, len(all))
	for _, t := range all {
		if t.Done {
			continue
		}
		if IsReady(t) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}
