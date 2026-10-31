//go:build unix

package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLockBriefSerializes: a second brief to one worker waits for the first
// to release, gives up with ErrBriefBusy when its context ends first, and
// then reads the row as it is under the lock. Another worker's lock is
// separate.
func TestLockBriefSerializes(t *testing.T) {
	l, err := openAt(t.TempDir(), "/repo/one", "default")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"w", "other"} {
		if err := l.Begin(Row{Name: name, Branch: "b", Stage: StageLaunched, StartedAt: time.Unix(1e9, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	release, _, err := l.LockBrief(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := l.LockBrief(ctx, "w"); !errors.Is(err, ErrBriefBusy) {
		t.Fatalf("second LockBrief while held = %v, want ErrBriefBusy", err)
	}
	relOther, _, err := l.LockBrief(context.Background(), "other")
	if err != nil {
		t.Fatalf("another worker's lock waited on this one: %v", err)
	}
	relOther()

	// The holder records a brief, then releases; the waiter reads it.
	if err := l.Update("w", func(r *Row) { r.Brief = &Brief{Marker: "m1", Count: 1, SentAt: time.Unix(5, 0), Via: ViaTyped} }); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); release() }()
	start := time.Now()
	rel2, row, err := l.LockBrief(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	if time.Since(start) < 80*time.Millisecond {
		t.Fatal("the second LockBrief did not wait for the first to release")
	}
	if row.Brief == nil || row.Brief.Marker != "m1" {
		t.Fatalf("row under the lock = %+v, want the brief the holder recorded", row.Brief)
	}
	if _, _, err := l.LockBrief(context.Background(), "nobody"); !errors.Is(err, ErrNoRow) {
		t.Fatalf("LockBrief for no row = %v, want ErrNoRow", err)
	}
}
