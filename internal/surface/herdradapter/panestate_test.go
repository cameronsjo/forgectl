//go:build unix

package herdradapter

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
)

// drainOwnPane is a HERDR_PANE_ID set on the calling process: the drain's
// own pane, which a pane-state change must never target.
const drainOwnPane = "w0:p-drain-own-pane"

// TestPaneState marks and clears only the owned worker's root pane named by
// its ref, pinned to the session, and changes nothing when a check fails.
func TestPaneState(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HERDR_PANE_ID", drainOwnPane)
	src := []exec.Arg{exec.MustFixed("--source"), exec.MustFixed(herdr.DrainAgentSource), exec.MustFixed("--agent"), exec.MustFixed(herdr.DrainAgentSource)}

	noEnvPane := func(t *testing.T, cmd exec.SensitiveCommand) {
		t.Helper()
		if hasArg(cmd, exec.Opaque(drainOwnPane)) {
			t.Fatal("the caller's own HERDR_PANE_ID was targeted")
		}
	}

	t.Run("reports blocked on the owned root pane", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "blocked"))
		if err := a.ReportBlocked(ctx, ref, "forgectl: w needs you — blocked"); err != nil {
			t.Fatalf("ReportBlocked: %v", err)
		}
		cmd, ok := commandOfKind(run.calls(), exec.KindHerdrPaneAgent)
		if !ok {
			t.Fatal("no pane-agent call was made")
		}
		want := append([]exec.Arg{exec.MustFixed("--session"), exec.Opaque(a.Session()),
			exec.MustFixed("pane"), exec.MustFixed("report-agent"), exec.Opaque(paneA)}, src...)
		want = append(want, exec.MustFixed("--state"), exec.MustFixed("blocked"),
			exec.MustFixed("--message"), exec.Opaque("forgectl: w needs you — blocked"))
		assertArgv(t, cmd, want)
		noEnvPane(t, cmd)
	})

	t.Run("releases the owned root pane", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "idle"))
		if err := a.ReleaseBlocked(ctx, ref); err != nil {
			t.Fatalf("ReleaseBlocked: %v", err)
		}
		cmd, ok := commandOfKind(run.calls(), exec.KindHerdrPaneAgent)
		if !ok {
			t.Fatal("no pane-agent call was made")
		}
		want := append([]exec.Arg{exec.MustFixed("--session"), exec.Opaque(a.Session()),
			exec.MustFixed("pane"), exec.MustFixed("release-agent"), exec.Opaque(paneA)}, src...)
		assertArgv(t, cmd, want)
		noEnvPane(t, cmd)
	})

	refused := []struct {
		name  string
		setup func(run *scriptedRunner, owner string)
		want  error
	}{
		{"workspace without our marker", func(run *scriptedRunner, _ string) {
			run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, "someone-else"}))
		}, ErrScreenUnreadable},
		{"gone workspace", func(run *scriptedRunner, _ string) {
			run.reply1(exec.KindHerdrProbe, listJSON())
		}, ErrWorkerGone},
		{"herdr places the pane elsewhere", func(run *scriptedRunner, owner string) {
			run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, owner}))
			run.reply1(exec.KindHerdrPaneStatus, paneGetJSON("w9:p1", "w9", "claude", "idle"))
		}, ErrScreenUnreadable},
	}
	for _, c := range refused {
		t.Run(c.name+" is refused", func(t *testing.T) {
			for _, report := range []bool{true, false} {
				a, run, ref := startClean(t)
				c.setup(run, ref.OwnershipName())
				var err error
				if report {
					err = a.ReportBlocked(ctx, ref, "m")
				} else {
					err = a.ReleaseBlocked(ctx, ref)
				}
				if !errors.Is(err, c.want) {
					t.Fatalf("report=%v: err = %v, want %v", report, err, c.want)
				}
				if _, ok := commandOfKind(run.calls(), exec.KindHerdrPaneAgent); ok {
					t.Fatalf("report=%v: the pane state was changed after a failed check", report)
				}
			}
		})
	}

	t.Run("a failed report-agent is ErrPaneStateFailed", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.reply1(exec.KindHerdrPaneStatus, paneGetJSON(paneA, wsA, "claude", "blocked"))
		run.on(exec.KindHerdrPaneAgent, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrPaneAgent, exec.OutcomeExit)
		})
		if err := a.ReportBlocked(ctx, ref, "m"); !errors.Is(err, ErrPaneStateFailed) {
			t.Fatalf("err = %v, want ErrPaneStateFailed", err)
		}
	})
}

func assertArgv(t *testing.T, cmd exec.SensitiveCommand, want []exec.Arg) {
	t.Helper()
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv has %d args, want %d", len(cmd.Args), len(want))
	}
	for i := range want {
		if !cmd.Args[i].Equal(want[i]) {
			t.Fatalf("argv[%d] differs from the expected pane-agent argv", i)
		}
	}
}
