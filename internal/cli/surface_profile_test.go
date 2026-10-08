//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// profileCfg is a config with one [surface.profiles] entry, work, at dir.
func profileCfg(dir string) config.Config {
	return config.Config{Surface: config.SurfaceConfig{Profiles: map[string]config.SurfaceProfile{"work": {ConfigDir: dir}}}}
}

// TestSurfaceEnqueueStoresTheProfileName: enqueue checks the profile exists
// and the model's shape, and the queue file carries the name, never the
// config_dir path.
func TestSurfaceEnqueueStoresTheProfileName(t *testing.T) {
	deps, repo := queueTestEnv(t)
	deps.Cfg = profileCfg("/cfg/work-account")
	brief := writeBrief(t, "tidy the docs\n")

	out, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), "--repo", repo, "--name", "tidy", "--brief", brief, "--profile", "work", "--model", "sonnet", "--json")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var res enqueueResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Profile != "work" || res.Model != "sonnet" {
		t.Fatalf("result %+v (%v)", res, err)
	}
	//nolint:gosec // G304, G703: the queue file under this test's own temp XDG_STATE_HOME
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "forgectl", "surface", "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/cfg/work-account") || !strings.Contains(string(data), `"profile": "work"`) {
		t.Fatalf("queue.json must hold the profile name only:\n%s", data)
	}

	for name, args := range map[string][]string{
		"unknown profile":    {"--profile", "personal"},
		"profile as a path":  {"--profile", "/cfg/work-account"},
		"model as a flag":    {"--model=--print"},
		"model with a space": {"--model", "opus x"},
	} {
		t.Run(name, func(t *testing.T) {
			all := append([]string{"--repo", repo, "--name", "other", "--brief", brief}, args...)
			if _, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), all...); ExitCode(err) != exitUsage {
				t.Fatalf("exit %d (%v), want %d", ExitCode(err), err, exitUsage)
			}
		})
	}
}

// TestSurfaceLaunchProfileRefusals: --profile and --model need --worktree, an
// unknown profile and a flag-shaped model are refused before anything runs.
func TestSurfaceLaunchProfileRefusals(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	for name, c := range map[string]struct {
		opts surfaceLaunchOptions
		want string
	}{
		"profile without worktree": {surfaceLaunchOptions{Backend: "tmux", Profile: "work"}, "need --worktree"},
		"model without worktree":   {surfaceLaunchOptions{Backend: "tmux", Model: "sonnet"}, "need --worktree"},
		"unknown profile":          {surfaceLaunchOptions{Backend: "herdr", Worktree: "feat/x", DisplayName: "w1", Profile: "personal"}, "--profile: no such [surface.profiles] entry"},
		"model as a flag":          {surfaceLaunchOptions{Backend: "herdr", Worktree: "feat/x", DisplayName: "w1", Model: "-p"}, "--model: a model is"},
		"profile on codex":         {surfaceLaunchOptions{Backend: "herdr", Worktree: "feat/x", DisplayName: "w1", Profile: "work", Harness: "codex"}, "claude workers only"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runSurfaceLaunch(cmd, module.Deps{Cfg: profileCfg("/cfg/work")}, c.opts)
			if ExitCode(err) != exitUsage || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("exit %d (%v), want %d naming %q", ExitCode(err), err, exitUsage, c.want)
			}
		})
	}
}

// TestWorkerProfileReachesEnvAndTranscript drives the real build step: a
// spec's profile dir becomes the worker's CLAUDE_CONFIG_DIR, its model the
// --model value, and the ledger row's transcript sits under that dir.
func TestWorkerProfileReachesEnvAndTranscript(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/launcher/claude")
	led := testWorkerLedger(t)
	var launched []launch.Invocation
	spec := workerSpec{name: "w1", branch: "feat/w1", harness: "claude", configDir: "/cfg/work", model: "sonnet"}
	if _, err := attemptWorker(context.Background(), led, "w1", "feat/w1", inProcessStepsFor(t, led, &launched, spec)); err != nil {
		t.Fatalf("attemptWorker: %v", err)
	}
	if len(launched) != 1 {
		t.Fatalf("launched %d", len(launched))
	}
	if got := envOf(launched[0].Env, "CLAUDE_CONFIG_DIR"); got != "/cfg/work" {
		t.Fatalf("worker CLAUDE_CONFIG_DIR = %q, want the profile's /cfg/work", got)
	}
	if i := slices.Index(launched[0].Args, "--model"); i < 0 || launched[0].Args[i+1] != "sonnet" {
		t.Fatalf("argv %q: want --model sonnet", launched[0].Args)
	}
	if row := onlyRow(t, led); !strings.HasPrefix(row.Transcript, "/cfg/work/projects/") {
		t.Fatalf("transcript %q, want one under the profile's config dir", row.Transcript)
	}

	// No profile: the launcher's CLAUDE_CONFIG_DIR passes through as before.
	led = testWorkerLedger(t)
	launched = nil
	if _, err := attemptWorker(context.Background(), led, "w1", "feat/w1", inProcessSteps(t, led, &launched)); err != nil {
		t.Fatalf("attemptWorker: %v", err)
	}
	if got := envOf(launched[0].Env, "CLAUDE_CONFIG_DIR"); got != "/launcher/claude" {
		t.Fatalf("no profile: CLAUDE_CONFIG_DIR = %q, want the launcher's", got)
	}
	if row := onlyRow(t, led); !strings.HasPrefix(row.Transcript, "/launcher/claude/projects/") {
		t.Fatalf("no profile: transcript %q", row.Transcript)
	}
}

func envOf(env []string, key string) string {
	v := ""
	for _, e := range env {
		if k, val, ok := strings.Cut(e, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// gitTop makes a git repo and returns its top level.
func gitTop(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	//nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if out, err := osexec.CommandContext(t.Context(), "git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	top, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return top
}

// TestDrainResolvesTheProfileAtLaunch: the drain resolves a row's profile
// name against the config it launches with (not the one at enqueue), passes
// the dir and the row's model into the spec, and an unknown name fails as a
// launch-config error before the launcher runs.
func TestDrainResolvesTheProfileAtLaunch(t *testing.T) {
	top := gitTop(t)
	brief := "fix it"
	row := worker.QueueRow{Name: "w1", Repo: top, Brief: brief, BriefSHA256: worker.BriefSHA256(brief), LaunchID: "launch-p",
		State: worker.QueueClaimed, Profile: "work", Model: "sonnet"}
	var specs []workerSpec
	fake := func(_ context.Context, _ io.Writer, _ module.Deps, spec workerSpec, _ string) (workerAttempt, error) {
		specs = append(specs, spec)
		return workerAttempt{}, nil
	}
	a := drainLaunchWith(exec.OSRunner{}, fake)(t.Context(), profileCfg("/cfg/now"), row)
	if a.Class != drain.ErrNone || len(specs) != 1 || specs[0].configDir != "/cfg/now" || specs[0].model != "sonnet" {
		t.Fatalf("attempt %+v, specs %+v; want the profile dir from the launch-time config and the row's model", a, specs)
	}

	a = drainLaunchWith(exec.OSRunner{}, fake)(t.Context(), config.Config{}, row)
	if a.Class != drain.ErrLaunchConfig || !a.CreatedNothing || len(specs) != 1 || !strings.Contains(a.Err, "work") {
		t.Fatalf("unknown profile: attempt %+v, launches %d; want a launch-config failure before launching", a, len(specs))
	}
}

// TestDrainUnknownProfilePauses: a row naming a profile the config no longer
// defines goes back to queued with no attempt counted, and claiming pauses.
func TestDrainUnknownProfilePauses(t *testing.T) {
	_, d, q := newFakeDrain(t)
	topA, topB := gitTop(t), gitTop(t)
	if _, _, err := q.EnqueueLaunch("a", topA, "brief a", "", worker.QueueLaunch{Profile: "work"}, drainT0); err != nil {
		t.Fatal(err)
	}
	enqueueAt(t, q, "b", topB, drainT0.Add(time.Minute))
	launches := 0
	d.io.launch = drainLaunchWith(exec.OSRunner{}, func(context.Context, io.Writer, module.Deps, workerSpec, string) (workerAttempt, error) {
		launches++
		return workerAttempt{}, nil
	})
	d.tick(t.Context())
	d.tick(t.Context())
	if launches != 0 {
		t.Fatalf("launched %d workers; the unknown profile must pause claiming", launches)
	}
	if r := rowNamed(t, q, "a"); r.State != worker.QueueQueued || r.Attempts != 0 || r.LaunchID != "" || r.Profile != "work" {
		t.Fatalf("row a %+v; want queued, no attempt, its profile name kept", r)
	}
	if !strings.Contains(d.pauses.Reason(), "launch-config") || !strings.Contains(d.pauses.Reason(), "work") {
		t.Fatalf("pause %q; want launch-config naming the profile", d.pauses.Reason())
	}
}

// slotsDrain is a fake drain whose claude-slots check answers from checks,
// one per call (the last repeats), counting calls.
func slotsDrain(t *testing.T, checks ...drain.SlotsCheck) (*fakeDrain, *drainer, *worker.Queue, *int) {
	t.Helper()
	f, d, q := newFakeDrain(t)
	calls := 0
	d.io.slots = func(context.Context) drain.SlotsCheck {
		c := checks[min(calls, len(checks)-1)]
		calls++
		return c
	}
	return f, d, q, &calls
}

func eventsOfKind(f *fakeDrain, kind string) []drain.Event {
	var out []drain.Event
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestDrainSlotsFreeLaunches(t *testing.T) {
	f, d, q, calls := slotsDrain(t, drain.SlotsCheck{Exit: 0})
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	enqueueAt(t, q, "b", "/repo/b", drainT0.Add(time.Minute))
	d.tick(t.Context())
	if strings.Join(f.launched, ",") != "a,b" || *calls != 2 {
		t.Fatalf("launched %v after %d checks; want a,b with one check each", f.launched, *calls)
	}
	if n := len(eventsOfKind(f, drain.EventSlotsHeld)) + len(eventsOfKind(f, drain.EventError)); n != 0 {
		t.Fatalf("%d slots events for free checks", n)
	}
}

// TestDrainSlotsHoldIsQueuedAndQuiet: exit 1 puts the claimed row back to
// queued with no attempt and no launch id, claims nothing more that tick, and
// records one slots-held event however many ticks it lasts.
func TestDrainSlotsHoldIsQueuedAndQuiet(t *testing.T) {
	held := drain.SlotsCheck{Exit: 1, Reason: "3 of 3 claude sessions live"}
	f, d, q, calls := slotsDrain(t, held, held, held, drain.SlotsCheck{Exit: 0})
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	enqueueAt(t, q, "b", "/repo/b", drainT0.Add(time.Minute))
	for range 3 {
		d.tick(t.Context())
	}
	if len(f.launched) != 0 || *calls != 3 {
		t.Fatalf("launched %v after %d checks; want none, one check per tick", f.launched, *calls)
	}
	for _, name := range []string{"a", "b"} {
		if r := rowNamed(t, q, name); r.State != worker.QueueQueued || r.Attempts != 0 || r.LaunchID != "" {
			t.Fatalf("row %s %+v; want queued, no attempt, no launch id", name, r)
		}
	}
	if r := rowNamed(t, q, "a"); !strings.Contains(r.LastError, "3 of 3 claude sessions live") {
		t.Fatalf("row a last_error %q; want the tool's reason", r.LastError)
	}
	heldEvents := eventsOfKind(f, drain.EventSlotsHeld)
	if len(heldEvents) != 1 || !strings.Contains(heldEvents[0].Error, "3 of 3 claude sessions live") {
		t.Fatalf("slots-held events %+v; want exactly one naming the reason", heldEvents)
	}
	if n := len(eventsOfKind(f, drain.EventState)); n != 0 {
		t.Fatalf("%d state events while held; the claim and requeue must stay quiet", n)
	}
	d.tick(t.Context())
	if strings.Join(f.launched, ",") != "a,b" {
		t.Fatalf("after the hold cleared: launched %v, want a,b", f.launched)
	}
}

// TestDrainSlotsMissingLaunchesWithOneNote: no claude-slots at start means
// one note, then launches as before.
func TestDrainSlotsMissingLaunchesWithOneNote(t *testing.T) {
	f, d, q := newFakeDrain(t)
	d.announce()
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	d.tick(t.Context())
	enqueueAt(t, q, "b", "/repo/b", drainT0)
	d.tick(t.Context())
	notes := eventsOfKind(f, drain.EventNote)
	if len(notes) != 1 || notes[0].Error != drain.NoSlotsNote {
		t.Fatalf("notes %+v; want one %q", notes, drain.NoSlotsNote)
	}
	if strings.Join(f.launched, ",") != "a,b" {
		t.Fatalf("launched %v, want a,b", f.launched)
	}
}

// TestDrainSlotsBrokenToolDoesNotBlock: another exit code or a timeout
// launches as if the tool were missing, with one error event per condition.
func TestDrainSlotsBrokenToolDoesNotBlock(t *testing.T) {
	for name, check := range map[string]drain.SlotsCheck{
		"exit 2":  {Exit: 2, Reason: "usage: claude-slots check N"},
		"timeout": {Exit: -1, TimedOut: true},
		"no run":  {Exit: -1, Err: "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			f, d, q, _ := slotsDrain(t, check)
			d.announce()
			enqueueAt(t, q, "a", "/repo/a", drainT0)
			enqueueAt(t, q, "b", "/repo/b", drainT0.Add(time.Minute))
			d.tick(t.Context())
			enqueueAt(t, q, "c", "/repo/c", drainT0.Add(2*time.Minute))
			d.tick(t.Context())
			if strings.Join(f.launched, ",") != "a,b,c" {
				t.Fatalf("launched %v, want a,b,c", f.launched)
			}
			if errs := eventsOfKind(f, drain.EventError); len(errs) != 1 || !strings.Contains(errs[0].Error, "claude-slots") {
				t.Fatalf("error events %+v; want one for the condition", errs)
			}
			if n := len(eventsOfKind(f, drain.EventNote)); n != 0 {
				t.Fatalf("%d notes; the tool was found", n)
			}
		})
	}
}

// fakeSlotsTool writes an executable claude-slots stand-in running body.
func fakeSlotsTool(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-slots")
	//nolint:gosec // G306: the stand-in must be executable
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunClaudeSlots runs a stand-in for claude-slots (never the real one):
// the arguments, the exit codes, the one-line reason, and the time cap.
func TestRunClaudeSlots(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	tool := fakeSlotsTool(t, `printf '%s\n' "$*" > `+argsFile+`; printf '\n  2 of 2 sessions live  \nsecond line\n'; exit 1`)
	c := runClaudeSlots(t.Context(), tool)
	if c.Exit != 1 || c.TimedOut || c.Err != "" || c.Reason != "2 of 2 sessions live" {
		t.Fatalf("held: %+v", c)
	}
	got, err := os.ReadFile(argsFile) //nolint:gosec // G304: the stand-in's argv file in this test's temp dir
	if err != nil || strings.TrimSpace(string(got)) != "check 1" {
		t.Fatalf("argv %q (%v), want check 1", got, err)
	}
	if c := runClaudeSlots(t.Context(), fakeSlotsTool(t, "exit 0")); c.Exit != 0 || c.TimedOut {
		t.Fatalf("free: %+v", c)
	}
	if c := runClaudeSlots(t.Context(), fakeSlotsTool(t, "echo bad usage >&2; exit 2")); c.Exit != 2 || c.Reason != "bad usage" {
		t.Fatalf("odd exit: %+v", c)
	}
	old := claudeSlotsTimeout
	claudeSlotsTimeout = 200 * time.Millisecond
	t.Cleanup(func() { claudeSlotsTimeout = old })
	start := time.Now()
	if c := runClaudeSlots(t.Context(), fakeSlotsTool(t, "exec sleep 30")); !c.TimedOut {
		t.Fatalf("slow tool: %+v, want timed out", c)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("a slow tool held the check for %s", took)
	}
	if c := runClaudeSlots(t.Context(), filepath.Join(t.TempDir(), "missing")); c.Err == "" {
		t.Fatalf("missing binary: %+v, want a run error", c)
	}
}

func TestLookClaudeSlots(t *testing.T) {
	tool := fakeSlotsTool(t, "exit 0")
	t.Setenv("PATH", filepath.Dir(tool))
	if got := lookClaudeSlots(); got != tool {
		t.Fatalf("lookClaudeSlots = %q, want %q", got, tool)
	}
	t.Setenv("PATH", t.TempDir())
	if got := lookClaudeSlots(); got != "" {
		t.Fatalf("lookClaudeSlots with none on PATH = %q, want empty", got)
	}
}
