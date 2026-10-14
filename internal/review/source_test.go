package review

// Test plan for source.go
//
// Aggregate (Classification: concurrent fan-in on the Inventory model)
//   [x] Happy: items merge across sources, deduped by Key, sorted
//   [x] Unhappy: one failed source degrades to a note, rows survive
//   [x] Unhappy: EVERY source failed → error (all-degraded is not an empty
//       inventory)
//   [x] Boundary: zero sources → error
//   [x] Determinism: notes in source order, not arrival order (#1018)

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeSource is a canned Source for aggregation tests.
type fakeSource struct {
	name  string
	items []Item
	notes []string
	err   error
}

func (f fakeSource) Name() string { return f.name }
func (f fakeSource) Items(context.Context) ([]Item, []string, error) {
	return f.items, f.notes, f.err
}

func TestAggregate_MergesDedupesSorts(t *testing.T) {
	a := fakeSource{name: "a", items: []Item{testItem(KindPR, "zeta", 1), testItem(KindIssue, "alpha", 2)}}
	b := fakeSource{name: "b", items: []Item{testItem(KindIssue, "alpha", 2)}} // dup of a's second

	items, notes, err := Aggregate(context.Background(), a, b)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %v", notes)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (deduped): %+v", len(items), items)
	}
	if items[0].Key() != "github.com/cameronsjo/alpha#2" || items[1].Key() != "github.com/cameronsjo/zeta#1" {
		t.Errorf("wrong order: %s, %s", items[0].Key(), items[1].Key())
	}
}

func TestAggregate_FailedSourceDegradesToNote(t *testing.T) {
	ok := fakeSource{name: "ok", items: []Item{testItem(KindIssue, "alpha", 1)}}
	bad := fakeSource{name: "bad", err: errors.New("boom")}

	items, notes, err := Aggregate(context.Background(), ok, bad)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("healthy source's rows must survive; got %d", len(items))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "bad") {
		t.Errorf("want one 'bad' degradation note, got %v", notes)
	}
}

func TestAggregate_AllSourcesFailed(t *testing.T) {
	bad1 := fakeSource{name: "b1", err: errors.New("boom")}
	bad2 := fakeSource{name: "b2", err: errors.New("boom")}
	if _, _, err := Aggregate(context.Background(), bad1, bad2); err == nil {
		t.Error("every source failing must be an error, not an empty inventory")
	}
}

func TestAggregate_NoSources(t *testing.T) {
	if _, _, err := Aggregate(context.Background()); err == nil {
		t.Error("zero sources must be an error")
	}
}

// gatedSource is a fakeSource that answers only once gate is closed, and
// closes done when it has answered.
type gatedSource struct {
	fakeSource
	gate <-chan struct{}
	done chan<- struct{}
}

func (g gatedSource) Items(ctx context.Context) ([]Item, []string, error) {
	if g.gate != nil {
		<-g.gate
	}
	if g.done != nil {
		defer close(g.done)
	}
	return g.fakeSource.Items(ctx)
}

// #1018: notes come out in source order, not arrival order. The first source
// answers only after the second has, so arrival order is the reverse.
// Mutation: append notes as results arrive (the order flips to b, a).
func TestAggregate_NotesKeepSourceOrder(t *testing.T) {
	secondDone := make(chan struct{})
	first := gatedSource{fakeSource: fakeSource{name: "a", err: errors.New("boom"), notes: []string{"a: own note"}}, gate: secondDone}
	second := gatedSource{fakeSource: fakeSource{name: "b", items: []Item{testItem(KindIssue, "alpha", 1)}, notes: []string{"b: own note"}}, done: secondDone}

	_, notes, err := Aggregate(context.Background(), first, second)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	want := []string{"a: own note", "a: source failed", "b: own note"}
	if !slices.Equal(notes, want) {
		t.Errorf("notes = %q; want %q", notes, want)
	}
}
