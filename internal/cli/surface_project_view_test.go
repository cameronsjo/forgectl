//go:build unix

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func TestSurfaceProjectTimelineReadOnlyUnknownState(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Read-only evidence")
	directory, err := os.OpenRoot(root)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	before, err := directory.ReadFile("registry.json")
	if err != nil {
		test.Fatal(err)
	}
	for _, threadID := range []string{"", created.Project.CoordinatorThreadID} {
		arguments := []string{"project", "--root", root, "timeline", created.Project.ID, "--json"}
		if threadID != "" {
			arguments = append(arguments, "--thread", threadID)
		}
		output, stderr, code := runSurface(test, "", arguments...)
		var report projectTimelineReport
		if err := json.Unmarshal([]byte(output), &report); err != nil || code != 0 {
			test.Fatalf("timeline exit=%d stdout=%s stderr=%s decode=%v", code, output, stderr, err)
		}
		if report.ProjectID != created.Project.ID || report.ThreadID != threadID || report.View.Live != "unknown" || report.View.Exit != nil || report.View.Partial || len(report.View.Events) != 1 || report.View.Events[0].Name != "PROJECT-THREAD-UNBOUND" {
			test.Fatalf("invented state or missing identity: %#v", report)
		}
		if report.View.Events[0].Fields["process_liveness"] != "unknown" || report.View.Events[0].Fields["native_session_known"] != "false" {
			test.Fatalf("invented native state: %#v", report.View.Events[0])
		}
	}
	after, err := directory.ReadFile("registry.json")
	if err != nil || string(before) != string(after) {
		test.Fatalf("timeline changed registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, created.Project.ID)); !os.IsNotExist(err) {
		test.Fatalf("timeline created a team directory: %v", err)
	}
}

func TestSurfaceProjectTimelineProjectsOnlySanitizedEvidence(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Transport only")
	member := mail.Worker{Name: "lead", Harness: mail.HarnessCodex, Coordinator: true, Worktree: created.Project.Worktree, RunID: "run-lead", ThreadID: "native-lead"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bindProjectCLIThread(test, root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "none")
	now := time.Now().UTC()
	box := mail.Mailbox{Dir: created.Ledger}
	if err := box.Locked(func(transaction *mail.Tx) error {
		message := mail.Message{V: 1, ID: "message-one", From: member.Name, To: member.Name, FromRunID: member.RunID, ToRunID: member.RunID, Priority: mail.PriorityNext, Body: "private-prompt\x1b[31m\u202e", CreatedAt: now}
		if err := transaction.Enqueue(message, now); err != nil {
			return err
		}
		return transaction.Mark(message.ID, mail.StatusSent, "private-detail", false, now.Add(time.Second))
	}); err != nil {
		test.Fatal(err)
	}
	output, stderr, code := runSurface(test, "", "project", "--root", root, "timeline", created.Project.ID, "--thread", created.Project.CoordinatorThreadID, "--json")
	var report projectTimelineReport
	if err := json.Unmarshal([]byte(output), &report); err != nil || code != 0 {
		test.Fatalf("timeline exit=%d stdout=%s stderr=%s decode=%v", code, output, stderr, err)
	}
	if len(report.View.Events) != 2 || report.View.Events[1].Name != "MAIL-TRANSPORT-ACCEPTED" || report.View.Events[1].Fields["to_run_id"] != member.RunID || report.View.Exit != nil || report.View.Live != "unknown" {
		test.Fatalf("transport evidence changed into completion: %#v", report.View)
	}
	for _, forbidden := range []string{"private-prompt", "private-detail", "\x1b", "\u202e", "COMPLETED"} {
		if strings.Contains(output, forbidden) {
			test.Fatalf("timeline exposed %q: %s", forbidden, output)
		}
	}
	output, stderr, code = runSurface(test, "", "project", "--root", root, "timeline", created.Project.ID, "--no-icons")
	if code != 0 || !strings.Contains(output, "MAIL-TRANSPORT-ACCEPTED") || strings.Contains(output, "private-") {
		test.Fatalf("text timeline exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
}

func TestSurfaceProjectTimelinePartialJSONFailsAfterReport(test *testing.T) {
	isolateJSONContractEnv(test)
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Partial evidence")
	if err := os.MkdirAll(created.Ledger, 0o700); err != nil {
		test.Fatal(err)
	}
	mailboxPath := filepath.Join(created.Ledger, "mailbox.jsonl")
	directory, err := os.OpenRoot(created.Ledger)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	for _, contents := range []string{"{broken}\n", "{\"op\":\"msg\"}"} {
		if err := os.WriteFile(mailboxPath, []byte(contents), 0o600); err != nil {
			test.Fatal(err)
		}
		output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), "surface", "project", "--root", root, "timeline", created.Project.ID, "--json")
		var report projectTimelineReport
		if decodeErr := json.Unmarshal([]byte(output), &report); decodeErr != nil || err == nil || ExitCode(err) != exitFailed || !report.View.Partial {
			test.Fatalf("partial timeline err=%v stdout=%s stderr=%s decode=%v", err, output, stderr, decodeErr)
		}
		if object := decodeOneStderrObject(test, stderr); object["code"] != jsonCodeFailed {
			test.Fatalf("partial error=%v", object)
		}
		if strings.Contains(stderr, "32 MiB") {
			test.Fatalf("reported another source's cap: %s", stderr)
		}
		after, readErr := directory.ReadFile("mailbox.jsonl")
		if readErr != nil || string(after) != contents {
			test.Fatalf("partial read repaired mailbox: %v", readErr)
		}
	}
}

func TestSurfaceProjectTimelineUsageRefusals(test *testing.T) {
	isolateJSONContractEnv(test)
	previousTTY := deskHasTerminal
	deskHasTerminal = func() bool { return false }
	test.Cleanup(func() { deskHasTerminal = previousTTY })
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Usage")
	cases := [][]string{
		{"timeline", created.Project.ID, "--live", "--json"},
		{"timeline", created.Project.ID, "--thread", "not-this-project", "--json"},
		{"timeline", "unknown-project", "--json"},
		{"timeline", "--json"},
	}
	for _, arguments := range cases {
		arguments = append([]string{"surface", "project", "--root", root}, arguments...)
		output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), arguments...)
		assertProjectJSONError(test, output, stderr, err, jsonCodeUsage, exitUsage)
	}
	if output, stderr, code := runSurface(test, "", "project", "--root", root, "timeline", created.Project.ID, "--live"); code != exitUsage || output != "" || !strings.Contains(stderr, "terminal") {
		test.Fatalf("nonterminal live exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	absent := filepath.Join(test.TempDir(), "absent")
	if _, _, code := runSurface(test, "", "project", "--root", absent, "timeline", created.Project.ID, "--json"); code != exitUsage {
		test.Fatalf("absent timeline exit=%d", code)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		test.Fatalf("absent timeline created root: %v", err)
	}
}
