package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
)

// scriptedLoop builds a readyLoop over a fixed sequence of reads and a fake
// clock that advances only when the loop sleeps.
func scriptedLoop(reads []func() (ready.Screen, error), eval func(ready.Screen) ready.Verdict, timeout time.Duration) (readyLoop, *int) {
	now := time.Unix(0, 0)
	calls := 0
	return readyLoop{
		read: func(context.Context) (ready.Screen, error) {
			i := calls
			calls++
			if i >= len(reads) {
				i = len(reads) - 1
			}
			return reads[i]()
		},
		evaluate: eval,
		now:      func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error {
			now = now.Add(d)
			return nil
		},
		timeout:  timeout,
		interval: time.Second,
	}, &calls
}

func screen(text string) func() (ready.Screen, error) {
	return func() (ready.Screen, error) { return ready.Screen{Text: text}, nil }
}

func failRead(err error) func() (ready.Screen, error) {
	return func() (ready.Screen, error) { return ready.Screen{}, err }
}

// byText maps a screen's text straight to a verdict, so these tests pin the
// loop's control flow, not the predicates (internal/herdr/ready pins those).
func byText(s ready.Screen) ready.Verdict {
	switch s.Text {
	case "ready":
		return ready.Verdict{State: ready.StateReady}
	case "dialog":
		return ready.Verdict{State: ready.StateBlocked, Blocking: "folder-trust dialog", Reason: "showing the folder-trust dialog"}
	default:
		return ready.Verdict{State: ready.StateNotReady, Reason: "the claude input prompt is not visible"}
	}
}

func TestWaitReady(t *testing.T) {
	ctx := context.Background()

	t.Run("polls until the prompt shows", func(t *testing.T) {
		l, calls := scriptedLoop([]func() (ready.Screen, error){screen("loading"), screen("loading"), screen("ready")}, byText, time.Minute)
		r := waitReady(ctx, l)
		if r.State != string(ready.StateReady) || *calls != 3 {
			t.Fatalf("result %+v after %d reads, want ready after 3", r, *calls)
		}
		if r.WaitedMS != 2000 {
			t.Errorf("waited %dms, want 2000", r.WaitedMS)
		}
	})

	t.Run("a blocking screen ends the wait at once", func(t *testing.T) {
		l, calls := scriptedLoop([]func() (ready.Screen, error){screen("dialog"), screen("ready")}, byText, time.Minute)
		r := waitReady(ctx, l)
		if r.State != string(ready.StateBlocked) || r.Blocking != "folder-trust dialog" || *calls != 1 {
			t.Fatalf("result %+v after %d reads, want blocked on the first read", r, *calls)
		}
	})

	t.Run("not ready at the timeout says so and why", func(t *testing.T) {
		l, _ := scriptedLoop([]func() (ready.Screen, error){screen("loading")}, byText, 5*time.Second)
		r := waitReady(ctx, l)
		if r.State != string(ready.StateNotReady) || !strings.Contains(r.Reason, "not ready after 5s") ||
			!strings.Contains(r.Reason, "prompt is not visible") {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("a gone workspace ends the wait", func(t *testing.T) {
		l, calls := scriptedLoop([]func() (ready.Screen, error){failRead(herdradapter.ErrWorkerGone)}, byText, time.Minute)
		r := waitReady(ctx, l)
		if r.State != readyStateGone || *calls != 1 {
			t.Fatalf("result %+v after %d reads", r, *calls)
		}
	})

	t.Run("an unreadable pane is retried, never read as gone", func(t *testing.T) {
		unreadable := failRead(errors.Join(herdradapter.ErrScreenUnreadable, errors.New("protocol mismatch")))
		l, calls := scriptedLoop([]func() (ready.Screen, error){unreadable, unreadable, screen("ready")}, byText, time.Minute)
		if r := waitReady(ctx, l); r.State != string(ready.StateReady) || *calls != 3 {
			t.Fatalf("result %+v after %d reads, want ready after 3", r, *calls)
		}
	})

	t.Run("still unreadable at the timeout reports unreadable", func(t *testing.T) {
		unreadable := failRead(errors.Join(herdradapter.ErrScreenUnreadable, errors.New("protocol mismatch")))
		l, _ := scriptedLoop([]func() (ready.Screen, error){unreadable}, byText, 3*time.Second)
		if r := waitReady(ctx, l); r.State != readyStateUnreadable {
			t.Fatalf("result %+v, want unreadable", r)
		}
	})

	t.Run("cancellation stops the wait", func(t *testing.T) {
		l, _ := scriptedLoop([]func() (ready.Screen, error){screen("loading")}, byText, time.Minute)
		l.sleep = func(context.Context, time.Duration) error { return context.Canceled }
		r := waitReady(ctx, l)
		if r.State != string(ready.StateNotReady) || !strings.Contains(r.Reason, "canceled") {
			t.Fatalf("result %+v", r)
		}
	})
}

func TestReportReadyExitCodes(t *testing.T) {
	for _, state := range []string{string(ready.StateBlocked), string(ready.StateNotReady), readyStateGone, readyStateUnreadable} {
		t.Run(state, func(t *testing.T) {
			cmd := newSurfaceReadyCmd(module.Deps{Runner: &exec.FakeRunner{}})
			err := reportReady(cmd, readyResult{Name: "w", State: state, Reason: "why"}, true)
			if code := ExitCode(err); code != 1 {
				t.Fatalf("exit %d (%v), want 1", code, err)
			}
		})
	}
	t.Run("ready exits 0", func(t *testing.T) {
		cmd := newSurfaceReadyCmd(module.Deps{Runner: &exec.FakeRunner{}})
		if err := reportReady(cmd, readyResult{Name: "w", State: string(ready.StateReady)}, false); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}
