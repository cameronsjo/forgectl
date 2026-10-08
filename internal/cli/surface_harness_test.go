//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// TestSurfaceEnqueueStoresTheHarness: enqueue stores the harness on the row
// (claude when none is named), takes a model for every harness, refuses a
// profile for codex and pi, and refuses a harness outside the three.
func TestSurfaceEnqueueStoresTheHarness(t *testing.T) {
	deps, repo := queueTestEnv(t)
	deps.Cfg = profileCfg("/cfg/work")
	brief := writeBrief(t, "tidy the docs\n")

	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"default", "claude", nil},
		{"codex", "codex", []string{"--harness", "codex", "--model", "gpt-6.1"}},
		{"pi", "pi", []string{"--harness", "pi", "--model", "qwen-27b"}},
		{"pi-main-profile", "pi", []string{"--harness", "pi", "--profile", "main"}},
	} {
		all := append([]string{"--repo", repo, "--name", c.name, "--brief", brief, "--json"}, c.args...)
		out, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), all...)
		if err != nil {
			t.Fatalf("%s: enqueue: %v", c.name, err)
		}
		var res enqueueResult
		if err := json.Unmarshal([]byte(out), &res); err != nil || res.Harness != c.want {
			t.Fatalf("%s: result %+v (%v), want harness %q", c.name, res, err, c.want)
		}
		if r := queueRowNamed(t, c.name); r.Harness != c.want {
			t.Fatalf("%s: stored row harness %q, want %q", c.name, r.Harness, c.want)
		}
	}
	if r := queueRowNamed(t, "pi"); r.Model != "qwen-27b" {
		t.Fatalf("pi row model %q, want qwen-27b", r.Model)
	}

	for name, args := range map[string][]string{
		"unknown harness":   {"--harness", "gemini"},
		"profile for codex": {"--harness", "codex", "--profile", "work"},
		"profile for pi":    {"--harness", "pi", "--profile", "work"},
	} {
		t.Run(name, func(t *testing.T) {
			all := append([]string{"--repo", repo, "--name", "other", "--brief", brief}, args...)
			if _, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), all...); ExitCode(err) != exitUsage {
				t.Fatalf("exit %d (%v), want %d", ExitCode(err), err, exitUsage)
			}
		})
	}

	// The same name and brief under another harness is another row: refused.
	_, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), "--repo", repo, "--name", "default", "--brief", brief, "--harness", "pi")
	if err == nil || ExitCode(err) == exitUsage || !strings.Contains(err.Error(), `harness "pi"`) {
		t.Fatalf("enqueue under another harness: exit %d (%v), want a refusal naming both harnesses", ExitCode(err), err)
	}
	// Naming claude again matches the default row: no change.
	if _, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), "--repo", repo, "--name", "default", "--brief", brief, "--harness", "claude"); err != nil {
		t.Fatalf("enqueue the default row again as claude: %v", err)
	}
}

func queueRowNamed(t *testing.T, name string) worker.QueueRow {
	t.Helper()
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no queue row %q", name)
	return worker.QueueRow{}
}

// TestDrainLaunchesWithTheRowsHarness: the drain's spec takes the row's
// harness and model; a row with no harness (written before the field) runs
// claude.
func TestDrainLaunchesWithTheRowsHarness(t *testing.T) {
	top := gitTop(t)
	brief := "fix it"
	for _, c := range []struct{ stored, want, model string }{
		{"", "claude", ""},
		{"claude", "claude", "sonnet"},
		{"codex", "codex", "gpt-6.1"},
		{"pi", "pi", "qwen-27b"},
	} {
		row := worker.QueueRow{Name: "w1", Repo: top, Brief: brief, BriefSHA256: worker.BriefSHA256(brief), LaunchID: "launch-h",
			State: worker.QueueClaimed, Harness: c.stored, Model: c.model}
		var specs []workerSpec
		fake := func(_ context.Context, _ io.Writer, _ module.Deps, spec workerSpec, _ string) (workerAttempt, error) {
			specs = append(specs, spec)
			return workerAttempt{}, nil
		}
		a := drainLaunchWith(exec.OSRunner{}, fake)(t.Context(), config.Config{}, row)
		if a.Class != drain.ErrNone || len(specs) != 1 || specs[0].harness != c.want || specs[0].model != c.model || specs[0].configDir != "" {
			t.Fatalf("stored harness %q: attempt %+v, specs %+v; want harness %q, model %q", c.stored, a, specs, c.want, c.model)
		}
	}
}

// TestPiWorkerLaunchesThroughTheRealBuild drives the real build step for a
// pi spec: the invocation is pi's, with the model flag and the brief after
// `--`, and the ledger row records harness pi with no claude session id.
func TestPiWorkerLaunchesThroughTheRealBuild(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: the stub must be executable to resolve as the pi binary
		t.Fatal(err)
	}
	t.Setenv("FORGECTL_PI_BIN", bin)
	led := testWorkerLedger(t)
	var launched []launch.Invocation
	spec := workerSpec{name: "w1", branch: "feat/w1", harness: "pi", model: "qwen-27b"}
	if _, err := attemptWorker(context.Background(), led, "w1", "feat/w1", inProcessStepsFor(t, led, &launched, spec)); err != nil {
		t.Fatalf("attemptWorker: %v", err)
	}
	if len(launched) != 1 {
		t.Fatalf("launched %d", len(launched))
	}
	inv := launched[0]
	if inv.Harness != "pi" || inv.Binary.Path != bin || !slices.Equal(inv.Args, []string{"--model", "qwen-27b", "--", "Fix it."}) {
		t.Fatalf("invocation harness %q binary %q argv %q; want pi with --model qwen-27b -- <brief>", inv.Harness, inv.Binary.Path, inv.Args)
	}
	if row := onlyRow(t, led); row.Harness != "pi" || row.SessionID != "" || row.Transcript != "" {
		t.Fatalf("ledger row %+v; want harness pi and no claude session", row)
	}
}

// TestDrainSlotsSkipsNonClaudeRows: claude-slots caps claude sessions, so a
// pi or codex row launches while the check would hold a claude row.
func TestDrainSlotsSkipsNonClaudeRows(t *testing.T) {
	f, d, q, calls := slotsDrain(t, drain.SlotsCheck{Exit: 1, Reason: "3 of 3 claude sessions live"})
	for i, h := range []string{"pi", "codex"} {
		if _, _, err := q.EnqueueLaunch(h+"-row", "/repo/"+h, "brief "+h, "", worker.QueueLaunch{Harness: h}, drainT0.Add(time.Duration(i-2)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	enqueueAt(t, q, "claude-row", "/repo/c", drainT0)
	d.tick(t.Context())
	if strings.Join(f.launched, ",") != "pi-row,codex-row" || *calls != 1 {
		t.Fatalf("launched %v after %d checks; want pi-row and codex-row without a check, claude-row held after one", f.launched, *calls)
	}
	if r := rowNamed(t, q, "claude-row"); r.State != worker.QueueQueued {
		t.Fatalf("claude-row %+v; want held in queued", r)
	}
}

// TestDrainSlotsHoldDoesNotBlockLaterRows: a held claude row ahead of a pi
// and a codex row does not stop them; a second claude row behind the hold is
// skipped unclaimed, without another check.
func TestDrainSlotsHoldDoesNotBlockLaterRows(t *testing.T) {
	f, d, q, calls := slotsDrain(t, drain.SlotsCheck{Exit: 1, Reason: "3 of 3 claude sessions live"})
	enqueueAt(t, q, "claude-row", "/repo/c", drainT0.Add(-3*time.Minute))
	for i, h := range []string{"pi", "codex"} {
		if _, _, err := q.EnqueueLaunch(h+"-row", "/repo/"+h, "brief "+h, "", worker.QueueLaunch{Harness: h}, drainT0.Add(time.Duration(i-2)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	enqueueAt(t, q, "claude-row-2", "/repo/c2", drainT0)
	d.tick(t.Context())
	if strings.Join(f.launched, ",") != "pi-row,codex-row" || *calls != 1 {
		t.Fatalf("launched %v after %d checks; want pi-row and codex-row launched behind the held claude-row, one check", f.launched, *calls)
	}
	for _, n := range []string{"claude-row", "claude-row-2"} {
		if r := rowNamed(t, q, n); r.State != worker.QueueQueued || r.Attempts != 0 || r.LaunchID != "" {
			t.Fatalf("%s %+v; want queued, unclaimed, no attempt", n, r)
		}
	}
}

// TestCheckClaimedRefusesABadHarness: the drain re-checks a stored row's
// harness and refuses a profile on a row that is not claude.
func TestCheckClaimedRefusesABadHarness(t *testing.T) {
	brief := "fix it"
	base := worker.QueueRow{Name: "w1", Repo: "/repo/a", Brief: brief, BriefSHA256: worker.BriefSHA256(brief)}
	for name, mutate := range map[string]func(*worker.QueueRow){
		"unknown harness":  func(r *worker.QueueRow) { r.Harness = "gemini" },
		"profile on pi":    func(r *worker.QueueRow) { r.Harness, r.Profile = "pi", "work" },
		"profile on codex": func(r *worker.QueueRow) { r.Harness, r.Profile = "codex", "work" },
	} {
		r := base
		mutate(&r)
		if err := drain.CheckClaimed(r); err == nil {
			t.Errorf("%s: CheckClaimed accepted %+v", name, r)
		}
	}
	for _, h := range []string{"", "claude", "codex", "pi"} {
		r := base
		r.Harness = h
		if err := drain.CheckClaimed(r); err != nil {
			t.Errorf("harness %q: %v", h, err)
		}
	}
	r := base
	r.Harness, r.Profile = "pi", "work"
	if err := drain.CheckClaimed(r); !errors.Is(err, worker.ErrProfileNotClaude) {
		t.Errorf("profile on pi: %v, want ErrProfileNotClaude", err)
	}
}
