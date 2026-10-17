package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func TestSurfaceTeamRegistration(t *testing.T) {
	t.Setenv(mail.EnvWorker, "")
	t.Setenv(mail.EnvDrainWorker, "")
	ledger := filepath.Join(t.TempDir(), "team")
	arguments := []string{"team", "register", "lead", "--ledger", ledger, "--harness", "codex", "--coordinator", "--socket", "/tmp/codex.sock", "--thread", "thread-1", "--worktree", t.TempDir(), "--json"}
	output, stderr, code := runSurface(t, "", arguments...)
	if code != 0 {
		t.Fatalf("register exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var report struct {
		Ledger      string            `json:"ledger"`
		Member      mail.Worker       `json:"member"`
		Environment map[string]string `json:"environment"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ledger != ledger || report.Member.RunID == "" || report.Environment[mail.EnvCoordinator] != "lead" || report.Environment[mail.EnvRunID] != report.Member.RunID {
		t.Fatalf("registration environment = %#v", report)
	}
	if _, _, code := runSurface(t, "", arguments...); code != 2 {
		t.Fatalf("duplicate register exit=%d, want2", code)
	}
	output, stderr, code = runSurface(t, "", "team", "status", "--ledger", ledger, "--json")
	var status struct {
		PeerMessages bool          `json:"peer_messages"`
		Members      []mail.Worker `json:"members"`
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil || code != 0 || status.PeerMessages || len(status.Members) != 1 {
		t.Fatalf("status exit=%d output=%s stderr=%s parse=%v", code, output, stderr, err)
	}
	t.Setenv(mail.EnvWorker, "builder")
	if _, _, code := runSurface(t, "", append(arguments, "--replace")...); code != 2 {
		t.Fatalf("worker could replace coordinator: exit=%d", code)
	}
}

func TestSurfaceTeamHermesRegistrationExportsSession(test *testing.T) {
	test.Setenv(mail.EnvWorker, "")
	test.Setenv(mail.EnvDrainWorker, "")
	ledger := filepath.Join(test.TempDir(), "team")
	worktree := test.TempDir()
	socket := filepath.Join(ledger, "hermes.sock")
	output, stderr, code := runSurface(test, "", "team", "register", "hermes-1", "--ledger", ledger, "--harness", "hermes", "--socket", socket, "--thread", "session-1", "--run-id", "run-1", "--worktree", worktree, "--json")
	if code != 0 {
		test.Fatalf("register exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var report struct {
		Member      mail.Worker       `json:"member"`
		Environment map[string]string `json:"environment"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		test.Fatal(err)
	}
	if report.Member.Harness != mail.HarnessHermes || report.Environment["FORGECTL_HERMES_SOCKET"] != socket || report.Environment["FORGECTL_HERMES_SESSION_ID"] != "session-1" || report.Environment[mail.EnvRunID] != "run-1" {
		test.Fatalf("Hermes registration = %#v", report)
	}
}

func TestSurfaceTeamExtensionExactBuild(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "team")
	arguments := []string{"team", "extension", "--ledger", ledger, "--json"}
	output, stderr, code := runSurface(t, "", arguments...)
	if code != 0 {
		t.Fatalf("extension exit=%d output=%s stderr=%s", code, output, stderr)
	}
	var report struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(report.Path)
	if err != nil || string(contents) != string(mail.PiExtension) {
		t.Fatalf("extension differs from embedded build: %v", err)
	}
	if _, _, code := runSurface(t, "", arguments...); code != 0 {
		t.Fatalf("same extension was not reusable: exit=%d", code)
	}
	if err := os.WriteFile(report.Path, []byte("different build"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSurface(t, "", arguments...); code != 2 {
		t.Fatalf("overwrote different extension: exit=%d", code)
	}
	contents, err = os.ReadFile(report.Path)
	if err != nil || string(contents) != "different build" {
		t.Fatalf("refusal did not preserve existing content: %q, %v", contents, err)
	}
}

func TestSurfaceTeamRequiresPrivateDirectory(t *testing.T) {
	parent := t.TempDir()
	public := filepath.Join(parent, "public")
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := root.Mkdir("public", 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "link")
	if err := os.Symlink(parent, linked); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"relative", public, linked} {
		if _, _, code := runSurface(t, "", "team", "extension", "--ledger", directory, "--json"); code != 2 {
			t.Fatalf("accepted unsafe ledger %q: exit=%d", directory, code)
		}
	}
}

func TestSurfaceSendUnconfirmedIsNotSuccess(t *testing.T) {
	_, adapter := useMailLedger(t)
	adapter.errs = []error{mail.Unconfirmed("acknowledgement was lost")}
	output, stderr, code := runSurface(t, "", "send", "pi-1", "one task", "--json")
	var result sendResult
	if err := json.Unmarshal([]byte(output), &result); err != nil || code != 1 || result.Status != "unconfirmed" {
		t.Fatalf("unconfirmed exit=%d output=%s stderr=%s parse=%v", code, output, stderr, err)
	}
	output, stderr, code = runSurface(t, "", "flush", "--json")
	var report flushReport
	if err := json.Unmarshal([]byte(output), &report); err != nil || code != 0 || report.Unconfirmed != 1 || len(adapter.texts) != 1 {
		t.Fatalf("flush redelivered or hid uncertainty: exit=%d output=%s stderr=%s sends=%d parse=%v", code, output, stderr, len(adapter.texts), err)
	}
}
