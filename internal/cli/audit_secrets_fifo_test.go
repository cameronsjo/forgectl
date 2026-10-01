//go:build unix && !aix && !illumos && !solaris

package cli

import (
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/audit"
	"github.com/cameronsjo/forgectl/internal/audit/gitleaks"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestAuditSecrets_FIFOGitignoreDoesNotHang runs the real git against a repo
// whose .gitignore is a FIFO, which makes `ls-files --exclude-standard`
// wait forever. The per-repo deadline (shortened through the seam) must end
// it, and the repo's findings read git-unknown.
//
// Mutation that turns it red: drop the deadline in gitstate.FuncWithTimeout.
func TestAuditSecrets_FIFOGitignoreDoesNotHang(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "r")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	init := osexec.Command("git", "-C", repo, "init", "-q") //nolint:gosec,noctx // G204: test setup running git with fixed arguments
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := syscall.Mkfifo(filepath.Join(repo, ".gitignore"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newAuditSecretsCmd(auditSecretsDeps{
		resolveRoot:      func() (string, error) { return root, nil },
		runner:           exec.OSRunner{},
		lookPath:         absentLookPath,
		gitStatusTimeout: 300 * time.Millisecond,
	})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--json", "--gitleaks=off"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		if f, err := os.OpenFile(filepath.Clean(filepath.Join(repo, ".gitignore")), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("audit secrets hung on a FIFO .gitignore")
	}
	var got struct {
		GitFailed int `json:"git_status_failed_repos"`
		Findings  []struct {
			Path  string   `json:"path"`
			Flags []string `json:"flags"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got.GitFailed != 1 || len(got.Findings) != 1 || !slices.Contains(got.Findings[0].Flags, audit.FlagGitUnknown) {
		t.Errorf("report = %+v, want the .env listed git-unknown and one failed repo", got)
	}
}

// TestAuditSecrets_SkipsRepoWithFIFOScannerConfig: a repo whose root
// .gitleaksignore is a FIFO is never handed to gitleaks, which would block
// opening it. The report names it skipped and the verb exits 1.
//
// Mutation that turns it red: return an empty map from gitleaksSkips.
func TestAuditSecrets_SkipsRepoWithFIFOScannerConfig(t *testing.T) {
	fx := newSecretsFixture(t)
	if err := os.Remove(filepath.Join(fx.repo, ".gitleaks.toml")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fx.repo, ".gitleaksignore"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	scanned := false
	runner := secretsRunner(t, fx, func([]string) (string, error) {
		scanned = true
		return "", errors.New("must not run")
	})
	stdout, _, err := runAuditSecrets(t, fx, runner, nil, "--json")
	if scanned {
		t.Error("gitleaks was run over a repo with a FIFO .gitleaksignore")
	}
	if err == nil || ExitCode(err) != 1 {
		t.Errorf("err = %v, want exit 1 for a skipped repo", err)
	}
	var got struct {
		Gitleaks struct {
			Status  string `json:"status"`
			Skipped []struct {
				Repo, Reason string
			} `json:"repos_skipped"`
		} `json:"gitleaks"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.Gitleaks.Status != gitleaks.StatusRan || len(got.Gitleaks.Skipped) != 1 ||
		got.Gitleaks.Skipped[0].Repo != fx.repo || got.Gitleaks.Skipped[0].Reason != gitleaks.SkipScannerConfigNotRegular {
		t.Errorf("gitleaks block = %+v", got.Gitleaks)
	}
}
