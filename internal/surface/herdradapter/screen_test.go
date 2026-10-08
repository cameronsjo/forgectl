//go:build unix

package herdradapter

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
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

// TestWorkerWrites types and presses Enter only into the owned root pane,
// and writes nothing when the ownership check fails.
func TestWorkerWrites(t *testing.T) {
	ctx := context.Background()

	t.Run("types the text into the owned root pane without Enter", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		if err := a.TypeText(ctx, ref, "run the tests"); err != nil {
			t.Fatalf("TypeText: %v", err)
		}
		send, ok := commandOfKind(run.calls(), exec.KindHerdrSendText)
		if !ok {
			t.Fatal("no send-text was made")
		}
		if !hasArg(send, exec.MustFixed("send-text")) || !hasArg(send, exec.Opaque(paneA)) || !hasArg(send, exec.Opaque("run the tests")) {
			t.Errorf("send-text argv does not name the root pane and the text")
		}
		if _, ok := commandOfKind(run.calls(), exec.KindHerdrSendKeys); ok {
			t.Error("TypeText pressed a key")
		}
	})

	t.Run("presses Enter in the owned root pane", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		if err := a.PressEnter(ctx, ref); err != nil {
			t.Fatalf("PressEnter: %v", err)
		}
		keys, ok := commandOfKind(run.calls(), exec.KindHerdrSendKeys)
		if !ok || !hasArg(keys, exec.Opaque(paneA)) || !hasArg(keys, exec.MustFixed("Enter")) {
			t.Fatal("no Enter was sent to the root pane")
		}
	})

	for name, write := range map[string]func(*Adapter, context.Context, backend.Ref) error{
		"TypeText":   func(a *Adapter, ctx context.Context, ref backend.Ref) error { return a.TypeText(ctx, ref, "x") },
		"PressEnter": func(a *Adapter, ctx context.Context, ref backend.Ref) error { return a.PressEnter(ctx, ref) },
	} {
		t.Run(name+" writes nothing to a workspace we do not own", func(t *testing.T) {
			a, run, ref := startClean(t)
			run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, "someone-else"}))
			if err := write(a, ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
				t.Fatalf("err = %v, want ErrScreenUnreadable", err)
			}
			for _, k := range []exec.CommandKind{exec.KindHerdrSendText, exec.KindHerdrSendKeys} {
				if _, ok := commandOfKind(run.calls(), k); ok {
					t.Errorf("%s was sent to a workspace we do not own", k)
				}
			}
		})
		t.Run(name+" writes nothing when herdr places the pane elsewhere", func(t *testing.T) {
			a, run, ref := startClean(t)
			run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
			run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, "w9", "claude", "idle"))
			if err := write(a, ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
				t.Fatalf("err = %v, want ErrScreenUnreadable", err)
			}
			for _, k := range []exec.CommandKind{exec.KindHerdrSendText, exec.KindHerdrSendKeys} {
				if _, ok := commandOfKind(run.calls(), k); ok {
					t.Errorf("%s was sent to a pane herdr places in another workspace", k)
				}
			}
		})
		t.Run(name+" on a gone workspace is ErrWorkerGone", func(t *testing.T) {
			a, run, ref := startClean(t)
			run.reply1(exec.KindHerdrProbe, listJSON())
			if err := write(a, ctx, ref); !errors.Is(err, ErrWorkerGone) {
				t.Fatalf("err = %v, want ErrWorkerGone", err)
			}
		})
	}

	t.Run("a failed send is ErrSendFailed", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		run.on(exec.KindHerdrSendText, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrSendText, exec.OutcomeExit)
		})
		if err := a.TypeText(ctx, ref, "x"); !errors.Is(err, ErrSendFailed) {
			t.Fatalf("err = %v, want ErrSendFailed", err)
		}
	})
}
