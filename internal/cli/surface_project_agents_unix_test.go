//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/surface/projectview"
)

func TestSurfaceProjectAgentsSnapshotIsReadOnlyAndNeedsNoHerdr(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Agents snapshot")
	directory, err := os.OpenRoot(root)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	before, err := directory.ReadFile("registry.json")
	if err != nil {
		test.Fatal(err)
	}
	output, stderr, code := runSurface(test, "", "project", "--root", root, "agents", created.Project.ID, "--json")
	if code != 0 || stderr != "" {
		test.Fatalf("agents snapshot failed: exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var snapshot projectview.AgentsSnapshot
	if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
		test.Fatal(err)
	}
	if snapshot.Version != projectview.AgentsVersion || snapshot.ProjectID != created.Project.ID || snapshot.TotalThreads != 1 || len(snapshot.Rows) != 1 || snapshot.Rows[0].Enrollment != string(project.EnrollmentUnbound) || snapshot.Rows[0].State != "unbound" {
		test.Fatalf("unexpected agents snapshot: %#v", snapshot)
	}
	after, err := directory.ReadFile("registry.json")
	if err != nil || string(before) != string(after) {
		test.Fatalf("agents snapshot changed registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, created.Project.ID)); !os.IsNotExist(err) {
		test.Fatalf("agents snapshot created a team directory: %v", err)
	}
}

func TestSurfaceProjectAgentsPartialSnapshotPrintsAndFails(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Partial agents")
	member := mail.Worker{Name: "lead", Harness: mail.HarnessCodex, Coordinator: true, Worktree: created.Project.Worktree, RunID: "codex-run", ThreadID: "codex-thread"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bindProjectCLIThread(test, root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "none")
	previous := projectObserveNative
	projectObserveNative = func(_ context.Context, worker mail.Worker) (projectNativeObservation, error) {
		now := time.Now().UTC()
		return projectNativeObservation{
			Source:          project.EvidenceSourceCodexThreadRead,
			NativeSessionID: worker.ThreadID,
			Provider:        "openai",
			Model:           "gpt-test",
			Activity: &activity.Snapshot{
				Version:    activity.Version,
				Source:     activity.SourceCodex,
				Coverage:   activity.CoverageNativeHistory,
				ObservedAt: now,
				Spans:      []activity.Span{},
				Partial:    true,
			},
		}, nil
	}
	test.Cleanup(func() { projectObserveNative = previous })

	printed, stderr, code := runSurface(test, "", "project", "--root", root, "agents", created.Project.ID)
	if code != exitFailed || !strings.Contains(printed, "partial=true") || !strings.Contains(printed, "bounded-native-activity") || !strings.Contains(stderr, "partial") {
		test.Fatalf("partial printed snapshot exit=%d stdout=%q stderr=%q", code, printed, stderr)
	}

	isolateJSONContractEnv(test)
	output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), "surface", "project", "--root", root, "agents", created.Project.ID, "--json")
	var snapshot projectview.AgentsSnapshot
	if decodeErr := json.Unmarshal([]byte(output), &snapshot); decodeErr != nil || err == nil || ExitCode(err) != exitFailed || !snapshot.Partial || snapshot.Rows[0].Reason != "bounded-native-activity" {
		test.Fatalf("partial JSON snapshot err=%v stdout=%s stderr=%s decode=%v snapshot=%#v", err, output, stderr, decodeErr, snapshot)
	}
	if object := decodeOneStderrObject(test, stderr); object["code"] != jsonCodeFailed {
		test.Fatalf("partial JSON error=%v", object)
	}
}

func TestSurfaceProjectAgentsRejectsLiveJSONAndNonTTY(test *testing.T) {
	if err := (projectAgentsOptions{Live: true}).validate(); err != nil {
		test.Fatalf("read-only live viewer unexpectedly requires herdr target: %v", err)
	}
	if err := (projectAgentsOptions{Live: true, HerdrSession: "projects-test"}).validate(); err == nil {
		test.Fatal("one-sided live herdr target was accepted")
	}
	root := filepath.Join(test.TempDir(), "registry")
	previousTTY := deskHasTerminal
	previousDumb := termIsDumb
	test.Cleanup(func() {
		deskHasTerminal = previousTTY
		termIsDumb = previousDumb
	})
	deskHasTerminal = func() bool { return false }
	termIsDumb = func() bool { return false }
	output, stderr, code := runSurface(test, "", "project", "--root", root, "agents", "project-id", "--live", "--json")
	if code != exitUsage || output != "" || !strings.Contains(stderr, "cannot be combined") {
		test.Fatalf("live/json contract changed: exit=%d stdout=%q stderr=%q", code, output, stderr)
	}
	output, stderr, code = runSurface(test, "", "project", "--root", root, "agents", "project-id", "--live")
	if code != exitUsage || output != "" || !strings.Contains(stderr, "terminal") {
		test.Fatalf("non-TTY live contract changed: exit=%d stdout=%q stderr=%q", code, output, stderr)
	}
}

func TestSurfaceProjectAgentsBootstrapIsFixedAndQuoted(test *testing.T) {
	bootstrap, err := projectAgentsBootstrap("/tmp/forgectl's build", "/tmp/projects with spaces", "project-id", "projects-test", "/opt/herdr/bin/herdr")
	if err != nil {
		test.Fatal(err)
	}
	expected := `'/tmp/forgectl'\''s build' surface project '--root' '/tmp/projects with spaces' agents project-id '--live' '--herdr-session' projects-test '--herdr-binary' /opt/herdr/bin/herdr`
	if !bootstrap.SensitiveArg().Equal(exec.Opaque(expected)) {
		test.Fatalf("agents bootstrap is not the exact quoted command: %q", expected)
	}
	if _, err := projectAgentsBootstrap("/tmp/forgectl", "/tmp/invalid\nroot", "project-id", "projects-test", "/opt/herdr"); err == nil {
		test.Fatal("newline entered a bootstrap command")
	}
	if _, err := projectAgentsBootstrap("/tmp/forgectl", "/tmp/root", "project-id", "projects-test", "relative-herdr"); err == nil {
		test.Fatal("relative herdr binary entered a bootstrap command")
	}
}

func TestFocusProjectAgentRefusesReplacementBeforePresentation(test *testing.T) {
	store, created, member, presentation, _ := prepareProjectThreadView(test)
	member.RunID = "replacement-run"
	registerProjectCLIMember(test, created.Ledger, member, true)
	err := focusProjectAgent(test.Context(), store, presentation, created.Project.ID, created.Project.CoordinatorThreadID, "view-run", projectPresentationOptions{Session: "projects-test", Binary: "/test/herdr"})
	if !errors.Is(err, project.ErrConflict) || presentation.threadFocus != 0 {
		test.Fatalf("replacement reached presentation: err=%v focus=%d", err, presentation.threadFocus)
	}
}
