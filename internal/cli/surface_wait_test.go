package cli

import (
	"context"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// scriptedWait builds a waitLoop over a fixed sequence of screens, one per
// read, the last repeating, with a clock that moves only when the loop sleeps.
func scriptedWait(screens []ready.Screen, errs map[int]error, marker string) (waitLoop, *int) {
	now := time.Unix(0, 0)
	calls := 0
	return waitLoop{
		read: func(context.Context) (ready.Screen, error) {
			i := calls
			calls++
			if err := errs[i]; err != nil {
				return ready.Screen{}, err
			}
			if i >= len(screens) {
				i = len(screens) - 1
			}
			return screens[i], nil
		},
		evaluate: func(s ready.Screen) ready.Verdict {
			switch {
			case s.Text == "dialog":
				return ready.Verdict{State: ready.StateBlocked, Blocking: "permission prompt"}
			case s.Status == "idle" || s.Status == "done":
				return ready.Verdict{State: ready.StateReady}
			default:
				return ready.Verdict{State: ready.StateNotReady, Reason: "busy"}
			}
		},
		marker:   marker,
		now:      func() time.Time { return now },
		sleep:    func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
		timeout:  10 * time.Minute,
		interval: time.Second,
		settle:   3 * time.Second,
		quiet:    20 * time.Second,
	}, &calls
}

func TestWaitSettled(t *testing.T) {
	ctx := context.Background()
	idle := ready.Screen{Status: "idle"}
	working := ready.Screen{Status: "working"}

	t.Run("settles after a turn and the settle window", func(t *testing.T) {
		l, calls := scriptedWait([]ready.Screen{working, working, idle}, nil, "")
		r := waitSettled(ctx, l)
		if r.State != readyStateSettled || !r.TurnSeen || *calls != 6 {
			t.Fatalf("result %+v after %d reads, want settled after 6 (2 working, 4 idle)", r, *calls)
		}
	})

	t.Run("a report on screen settles without a turn seen", func(t *testing.T) {
		rep := ready.Screen{Status: "done", Text: "⏺ REPORT " + testBriefMarker + ": done"}
		l, _ := scriptedWait([]ready.Screen{rep}, nil, testBriefMarker)
		r := waitSettled(ctx, l)
		if r.State != readyStateSettled || r.TurnSeen || !r.Report {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("idle with no turn and no report waits for quiet", func(t *testing.T) {
		l, calls := scriptedWait([]ready.Screen{idle}, nil, testBriefMarker)
		r := waitSettled(ctx, l)
		if r.State != readyStateSettled || *calls != 21 {
			t.Fatalf("result %+v after %d reads, want settled at the 20 s quiet mark", r, *calls)
		}
	})

	t.Run("going back to work restarts the settle window", func(t *testing.T) {
		l, calls := scriptedWait([]ready.Screen{working, idle, idle, working, idle}, nil, "")
		r := waitSettled(ctx, l)
		if r.State != readyStateSettled || *calls != 8 {
			t.Fatalf("result %+v after %d reads, want settled after 8", r, *calls)
		}
	})

	t.Run("an unreadable read breaks the settle window", func(t *testing.T) {
		l, calls := scriptedWait([]ready.Screen{working, idle}, map[int]error{3: herdradapter.ErrScreenUnreadable}, "")
		r := waitSettled(ctx, l)
		if r.State != readyStateSettled || *calls != 8 {
			t.Fatalf("result %+v after %d reads, want settled after 8", r, *calls)
		}
	})

	t.Run("unsent text in the input box never settles", func(t *testing.T) {
		l, _ := scriptedWait([]ready.Screen{working, {Status: "idle", Text: "unsent"}}, nil, "")
		inner := l.evaluate
		l.evaluate = func(s ready.Screen) ready.Verdict {
			v := inner(s)
			if s.Text == "unsent" {
				v.Input = "half a brief"
			}
			return v
		}
		l.timeout = time.Minute
		r := waitSettled(ctx, l)
		if r.State == readyStateSettled || r.State == ready.StateReady {
			t.Fatalf("result %+v: settled or reported ready with text left in the box", r)
		}
	})

	t.Run("a dialog ends the wait at once", func(t *testing.T) {
		l, calls := scriptedWait([]ready.Screen{working, {Text: "dialog"}}, nil, "")
		r := waitSettled(ctx, l)
		if r.State != ready.StateBlocked || r.Blocking == "" || *calls != 2 {
			t.Fatalf("result %+v after %d reads", r, *calls)
		}
	})

	t.Run("a gone workspace ends the wait", func(t *testing.T) {
		l, _ := scriptedWait([]ready.Screen{working}, map[int]error{1: herdradapter.ErrWorkerGone}, "")
		if r := waitSettled(ctx, l); r.State != readyStateGone {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("still working at the timeout is not settled", func(t *testing.T) {
		l, _ := scriptedWait([]ready.Screen{working}, nil, "")
		l.timeout = 10 * time.Second
		r := waitSettled(ctx, l)
		if r.State == readyStateSettled || !r.TurnSeen {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("an echo of the brief is not a report", func(t *testing.T) {
		echo := ready.Screen{Status: "idle", Text: "❯ " + worker.Compose("go", testBriefMarker, worker.ViaTyped)}
		l, calls := scriptedWait([]ready.Screen{echo}, nil, testBriefMarker)
		r := waitSettled(ctx, l)
		if r.Report || *calls != 21 {
			t.Fatalf("result %+v after %d reads: the echo counted as a report", r, *calls)
		}
	})
}
