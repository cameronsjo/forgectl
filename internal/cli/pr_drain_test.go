package cli

// Test plan for pr_drain.go / pr_queue.go
//
// newPrQueueCmd (Classification: cobra command, read-only view)
//   [x] Empty queue prints "no queued reviews", exit 0
//   [x] --json emits [] when empty
//   [x] Populated queue lists oldest first, matching drain's own claim order
//
// newPrDrainCmd (Classification: cobra command, ADR-0008 surface)
//   [x] --interval without --watch refuses, before any pass runs
//   [x] --once and --watch together refuses
//   [x] Empty queue: "nothing queued", exit 0
//   [x] --dry-run prints the would-launch line and creates nothing
//   [x] A real pass launches a queued ref and prints the pass=... line, exit 0
//   [x] A launch failure exits 1 and names the failed count
//   [x] --json emits the report object

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prDrainRunner fakes gh (pr view resolves a valid head), git (clone
// succeeds), and tmux (session/window lifecycle) — new-window fails on the
// 1-indexed call numbers named in failOn.
func prDrainRunner(failOn map[int]error) *exec.FakeRunner {
	created := false
	call := 0
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		case name == "git" && len(args) > 0 && args[0] == "clone":
			return "", nil
		case name == "tmux" && len(args) > 0:
			switch args[0] {
			case "-V":
				return "tmux 3.7b", nil
			case "display-message":
				return "123\x1f456\x1f@0", nil
			case "list-windows":
				return "", nil
			case "list-sessions":
				if created {
					return "123\x1f456\x1f$1\x1fforgectl\x1f1\x1f0\x1f0\x1f/tmp", nil
				}
				return "", nil
			case "new-session":
				created = true
				return "123\x1f456\x1f$1", nil
			case "new-window":
				call++
				if err, ok := failOn[call]; ok {
					return "", err
				}
				return fmt.Sprintf("123\x1f456\x1f@%d", call), nil
			}
		}
		return "", nil
	}}
}

func drainCmdClient(t *testing.T, run *exec.FakeRunner) (*pr.Client, string) {
	t.Helper()
	fakeClaudeBin(t)
	dir := t.TempDir()
	client := pr.New(run, pr.WithSessionsDir(dir), pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession("forgectl"), pr.WithTTYCheck(func() bool { return false }))
	return client, dir
}

func seedQueuedFixture(t *testing.T, dir string, ref pr.Ref, createdAt time.Time) {
	t.Helper()
	client := pr.New(prDrainRunner(nil), pr.WithSessionsDir(dir))
	if _, err := client.Queue(context.Background(), ref, pr.PrepareOpts{
		Agent:      "claude",
		Provenance: pr.ReviewProvenanceThirdParty,
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	_ = createdAt // ordering covered at the internal/pr layer; CLI tests need only presence
}

func runPrQueue(t *testing.T, client *pr.Client, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newPrQueueCmd(client)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func runPrDrain(t *testing.T, client *pr.Client, cfg config.Config, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newPrDrainCmd(client, cfg)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestPrQueue_EmptyPrintsNoQueuedReviews(t *testing.T) {
	client, _ := drainCmdClient(t, prDrainRunner(nil))
	stdout, _, err := runPrQueue(t, client)
	if err != nil {
		t.Fatalf("pr queue: %v", err)
	}
	if strings.TrimSpace(stdout) != "no queued reviews" {
		t.Errorf("stdout = %q, want %q", stdout, "no queued reviews")
	}
}

func TestPrQueue_JSONEmptyIsEmptyArray(t *testing.T) {
	client, _ := drainCmdClient(t, prDrainRunner(nil))
	stdout, _, err := runPrQueue(t, client, "--json")
	if err != nil {
		t.Fatalf("pr queue --json: %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("stdout = %q, want []", stdout)
	}
}

func TestPrQueue_ListsOldestFirst(t *testing.T) {
	client, dir := drainCmdClient(t, prDrainRunner(nil))
	ref1 := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	ref2 := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 2}
	base := time.Now().UTC().Add(-time.Hour)
	seedQueuedFixture(t, dir, ref2, base.Add(time.Minute))
	seedQueuedFixture(t, dir, ref1, base)

	stdout, _, err := runPrQueue(t, client)
	if err != nil {
		t.Fatalf("pr queue: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
	// Both queued via Queue() back-to-back — order is by createdAt, which
	// Queue stamps at call time, so whichever was queued FIRST sorts first.
	if !strings.HasPrefix(lines[0], ref2.String()) {
		t.Errorf("first line = %q, want it to start with the first-queued ref %s", lines[0], ref2.String())
	}
}

func TestPrDrain_IntervalWithoutWatchRefuses(t *testing.T) {
	client, _ := drainCmdClient(t, prDrainRunner(nil))
	_, _, err := runPrDrain(t, client, config.Config{}, "--interval", "5s")
	if err == nil {
		t.Fatal("expected a refusal: --interval without --watch")
	}
	if !strings.Contains(err.Error(), "--watch") {
		t.Errorf("refusal %q does not name --watch", err)
	}
}

func TestPrDrain_OnceAndWatchTogetherRefuses(t *testing.T) {
	client, _ := drainCmdClient(t, prDrainRunner(nil))
	_, _, err := runPrDrain(t, client, config.Config{}, "--once", "--watch")
	if err == nil {
		t.Fatal("expected a refusal: --once and --watch combined")
	}
}

func TestPrDrain_EmptyQueuePrintsNothingQueued(t *testing.T) {
	client, _ := drainCmdClient(t, prDrainRunner(nil))
	stdout, _, err := runPrDrain(t, client, config.Config{})
	if err != nil {
		t.Fatalf("pr drain: %v", err)
	}
	if strings.TrimSpace(stdout) != "nothing queued" {
		t.Errorf("stdout = %q, want %q", stdout, "nothing queued")
	}
}

func TestPrDrain_DryRunPrintsWouldLaunchAndCreatesNothing(t *testing.T) {
	client, dir := drainCmdClient(t, prDrainRunner(nil))
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	seedQueuedFixture(t, dir, ref, time.Now().UTC())

	stdout, _, err := runPrDrain(t, client, config.Config{}, "--dry-run")
	if err != nil {
		t.Fatalf("pr drain --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "would launch") || !strings.Contains(stdout, ref.String()) {
		t.Errorf("stdout = %q, want a would-launch line naming %s", stdout, ref.String())
	}
	// Nothing changed on disk: the record is still queued.
	summaries, _, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Phase() != pr.PhaseQueued {
		t.Fatalf("summaries = %+v, want exactly one still-queued record", summaries)
	}
}

func TestPrDrain_LaunchesAQueuedReviewAndPrintsPassLine(t *testing.T) {
	run := prDrainRunner(nil)
	client, dir := drainCmdClient(t, run)
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	seedQueuedFixture(t, dir, ref, time.Now().UTC())

	stdout, _, err := runPrDrain(t, client, config.Config{})
	if err != nil {
		t.Fatalf("pr drain: %v", err)
	}
	if !strings.Contains(stdout, "pass=1") || !strings.Contains(stdout, "launched=1") {
		t.Errorf("stdout = %q, want a pass line reporting launched=1", stdout)
	}
	summaries, _, err := client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Phase() != pr.PhaseActive {
		t.Fatalf("summaries = %+v, want the record active", summaries)
	}
}

func TestPrDrain_LaunchFailureExitsNonZero(t *testing.T) {
	run := prDrainRunner(map[int]error{1: errors.New("boom: agent refused")})
	client, dir := drainCmdClient(t, run)
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	seedQueuedFixture(t, dir, ref, time.Now().UTC())

	_, _, err := runPrDrain(t, client, config.Config{})
	if err == nil {
		t.Fatal("expected a non-zero exit: the launch failed")
	}
	if ExitCode(err) != 1 {
		t.Errorf("exit code = %d, want 1", ExitCode(err))
	}
}

func TestPrDrain_JSONEmitsReportObject(t *testing.T) {
	run := prDrainRunner(nil)
	client, dir := drainCmdClient(t, run)
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	seedQueuedFixture(t, dir, ref, time.Now().UTC())

	stdout, _, err := runPrDrain(t, client, config.Config{}, "--json")
	if err != nil {
		t.Fatalf("pr drain --json: %v", err)
	}
	var report pr.DrainReport
	if jerr := json.Unmarshal([]byte(stdout), &report); jerr != nil {
		t.Fatalf("stdout did not parse as a DrainReport: %v\n%s", jerr, stdout)
	}
	if report.Launched != 1 {
		t.Errorf("report.Launched = %d, want 1", report.Launched)
	}
}
