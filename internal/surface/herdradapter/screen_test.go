//go:build unix

package herdradapter

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func paneGetJSON(pane, ws, agent, status string) []byte {
	return []byte(`{"id":"cli:pane:get","result":{"pane":{"pane_id":"` + pane + `","workspace_id":"` + ws +
		`","agent":"` + agent + `","agent_status":"` + status + `"}}}`)
}

// TestWorkerScreen reads an owned worker's root pane, and refuses every
// answer that is not provably about that pane.
func TestWorkerScreen(t *testing.T) {
	ctx := context.Background()

	t.Run("reads text and status of the owned root pane", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		run.reply1(exec.KindHerdrScreenRead, []byte("hello\n❯\n\n"))

		s, err := a.WorkerScreen(ctx, ref)
		if err != nil {
			t.Fatalf("WorkerScreen: %v", err)
		}
		if s.Text != "hello\n❯" || s.Agent != "claude" || s.Status != "idle" {
			t.Fatalf("screen = %+v", s)
		}
		read, ok := commandOfKind(run.calls(), exec.KindHerdrScreenRead)
		if !ok {
			t.Fatal("no screen read was made")
		}
		if !read.Args[0].Equal(exec.MustFixed("--session")) {
			t.Errorf("screen read is not pinned to the session: %v", read.Args)
		}
	})

	t.Run("gone workspace is ErrWorkerGone", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON())
		if _, err := a.WorkerScreen(ctx, ref); !errors.Is(err, ErrWorkerGone) {
			t.Fatalf("err = %v, want ErrWorkerGone", err)
		}
	})

	t.Run("workspace without our marker is unreadable and never read", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, "someone-else"}))
		if _, err := a.WorkerScreen(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
		if _, ok := commandOfKind(run.calls(), exec.KindHerdrScreenRead); ok {
			t.Error("a pane in a workspace we do not own was read")
		}
	})

	t.Run("herdr answering for another pane is unreadable", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON("w9:p1", "w9", "claude", "idle"))
		if _, err := a.WorkerScreen(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
	})

	t.Run("failed screen read is unreadable", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		run.on(exec.KindHerdrScreenRead, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{
					Stderr: exec.BoundedOutputForTest(errorJSON("pane_not_found"), exec.OutputComplete),
				},
				exec.SensitiveErrorForTest(exec.KindHerdrScreenRead, exec.OutcomeExit)
		})
		if _, err := a.WorkerScreen(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
	})

	t.Run("truncated screen read is unreadable", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		run.on(exec.KindHerdrScreenRead, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte("❯"), exec.OutputOverflowed)}, nil
		})
		if _, err := a.WorkerScreen(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
			t.Fatalf("err = %v, want ErrScreenUnreadable", err)
		}
	})
}
