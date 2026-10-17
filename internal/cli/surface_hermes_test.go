//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

type hermesCLIFixture struct {
	ledger    string
	member    string
	runID     string
	sessionID string
	worktree  string
	socket    string
}

func newHermesCLIFixture(test *testing.T) hermesCLIFixture {
	test.Helper()
	ledger := test.TempDir()
	if err := os.Chmod(ledger, 0o700); err != nil {
		test.Fatal(err)
	}
	worktree := test.TempDir()
	fixture := hermesCLIFixture{
		ledger:    ledger,
		member:    "hermes-worker",
		runID:     "run-1",
		sessionID: "session-1",
		worktree:  worktree,
		socket:    filepath.Join(ledger, "supervisor.sock"),
	}
	if _, err := (mail.FileRoster{Dir: ledger}).Register(mail.Worker{
		Name:     fixture.member,
		Harness:  mail.HarnessHermes,
		Worktree: fixture.worktree,
		RunID:    fixture.runID,
		ThreadID: fixture.sessionID,
		Socket:   fixture.socket,
	}, false); err != nil {
		test.Fatal(err)
	}
	test.Setenv(mail.EnvWorker, fixture.member)
	test.Setenv(mail.EnvCoordinator, "")
	test.Setenv(mail.EnvDrainWorker, "")
	test.Setenv(mail.EnvLedger, fixture.ledger)
	test.Setenv(mail.EnvRunID, fixture.runID)
	return fixture
}

func (fixture hermesCLIFixture) args(member, runID, worktree, sessionID, ledger string) []string {
	return []string{
		"hermes",
		"--command", "/bin/true",
		"--model", "openai-codex:test",
		"--ledger", ledger,
		"--member", member,
		"--worktree", worktree,
		"--socket", fixture.socket,
		"--run-id", runID,
		"--session-id", sessionID,
		"--json",
	}
}

func TestSurfaceHermesRejectsIdentityMismatches(test *testing.T) {
	cases := []struct {
		name      string
		member    string
		runID     string
		worktree  string
		sessionID string
		wantError string
	}{
		{
			name:      "member",
			member:    "other-worker",
			runID:     "run-1",
			worktree:  "fixture",
			sessionID: "session-1",
			wantError: "member does not match the current team identity",
		},
		{
			name:      "run",
			member:    "hermes-worker",
			runID:     "run-2",
			worktree:  "fixture",
			sessionID: "session-1",
			wantError: "run id does not match the registered member",
		},
		{
			name:      "worktree",
			member:    "hermes-worker",
			runID:     "run-1",
			worktree:  "other",
			sessionID: "session-1",
			wantError: "worktree does not match the registered member",
		},
		{
			name:      "session",
			member:    "hermes-worker",
			runID:     "run-1",
			worktree:  "fixture",
			sessionID: "session-2",
			wantError: "session-id does not match the registered native session",
		},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newHermesCLIFixture(test)
			worktree := fixture.worktree
			if testCase.worktree == "other" {
				worktree = test.TempDir()
			}
			if testCase.worktree == "fixture" {
				worktree = fixture.worktree
			}
			output, stderr, code := runSurface(test, "", fixture.args(testCase.member, testCase.runID, worktree, testCase.sessionID, fixture.ledger)...)
			if code == 0 || output != "" || !strings.Contains(stderr, testCase.wantError) {
				test.Fatalf("identity mismatch exit=%d stdout=%q stderr=%q, want %q", code, output, stderr, testCase.wantError)
			}
		})
	}
}

func TestSurfaceHermesRejectsMixedIdentity(test *testing.T) {
	fixture := newHermesCLIFixture(test)
	test.Setenv(mail.EnvCoordinator, fixture.member)
	output, stderr, code := runSurface(test, "", fixture.args(fixture.member, fixture.runID, fixture.worktree, fixture.sessionID, fixture.ledger)...)
	if code == 0 || output != "" || !strings.Contains(stderr, "coordinator and worker identities cannot be combined") {
		test.Fatalf("mixed identity exit=%d stdout=%q stderr=%q", code, output, stderr)
	}
}

func TestSurfaceHermesRejectsPublicLedger(test *testing.T) {
	fixture := newHermesCLIFixture(test)
	if err := os.Chmod(fixture.ledger, 0o755); err != nil { //nolint:gosec // G302: public mode is required to exercise private-ledger refusal
		test.Fatal(err)
	}
	output, stderr, code := runSurface(test, "", fixture.args(fixture.member, fixture.runID, fixture.worktree, fixture.sessionID, fixture.ledger)...)
	if code == 0 || output != "" || !strings.Contains(stderr, "private") {
		test.Fatalf("public ledger exit=%d stdout=%q stderr=%q", code, output, stderr)
	}
}

func TestSurfaceHermesRejectsLedgerSymlink(test *testing.T) {
	fixture := newHermesCLIFixture(test)
	link := filepath.Join(test.TempDir(), "ledger-link")
	if err := os.Symlink(fixture.ledger, link); err != nil {
		test.Fatal(err)
	}
	output, stderr, code := runSurface(test, "", fixture.args(fixture.member, fixture.runID, fixture.worktree, fixture.sessionID, link)...)
	if code == 0 || output != "" || !strings.Contains(stderr, "private") {
		test.Fatalf("ledger symlink exit=%d stdout=%q stderr=%q", code, output, stderr)
	}
}
