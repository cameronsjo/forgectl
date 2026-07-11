package review

import (
	"context"
	"fmt"
	"log/slog"
)

// Source enumerates open work items from one tracker host. Items returns the
// rows, per-query degradation notes, and a fatal error only when the source
// produced nothing usable at all. This interface is the multi-host seam: the
// GitHub source ships now, a Gitea source implements the same contract later.
type Source interface {
	Name() string
	Items(ctx context.Context) ([]Item, []string, error)
}

// sourceResult carries one source's outcome across the Aggregate fan-out
// channel (the Inventory model: buffered channel, degrade-to-note, fixed
// receive loop).
type sourceResult struct {
	name  string
	items []Item
	notes []string
	err   error
}

// Aggregate fans the sources out concurrently and merges their items: deduped
// by Key (sources own disjoint hosts, so a collision is a same-item re-read
// and last-write-wins is safe), sorted deterministically. A failed source
// degrades to a note; Aggregate errors only when EVERY source failed —
// all-degraded means there is no data to render, and rendering an empty
// dashboard as though the inventory were clear would be a lie.
func Aggregate(ctx context.Context, sources ...Source) ([]Item, []string, error) {
	if len(sources) == 0 {
		return nil, nil, fmt.Errorf("review: no sources configured")
	}

	ch := make(chan sourceResult, len(sources))
	for _, src := range sources {
		src := src
		go func() {
			items, notes, err := src.Items(ctx)
			ch <- sourceResult{src.Name(), items, notes, err}
		}()
	}

	var notes []string
	failed := 0
	byKey := make(map[string]Item)
	for range sources {
		res := <-ch
		notes = append(notes, res.notes...)
		if res.err != nil {
			slog.Warn("Review source degraded.", "source", res.name, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: %v", res.name, res.err))
			failed++
			continue
		}
		for _, it := range res.items {
			byKey[it.Key()] = it
		}
	}
	if failed == len(sources) {
		return nil, notes, fmt.Errorf("review: every source failed")
	}

	out := make([]Item, 0, len(byKey))
	for _, it := range byKey {
		out = append(out, it)
	}
	SortItems(out)
	slog.Info("Successfully aggregated review inventory.", "items", len(out), "sources", len(sources), "degraded_sources", failed, "notes", len(notes))
	return out, notes, nil
}
