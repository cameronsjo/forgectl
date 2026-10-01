//go:build unix && !aix && !illumos && !solaris

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// gitRealRunner runs git for real and answers gitleaks through fake.
type gitRealRunner struct {
	exec.OSRunner
	fake *exec.FakeRunner
	bin  string
}

func (g gitRealRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	if name == g.bin {
		return g.fake.RunWithEnvFiltered(ctx, env, unset, name, args...)
	}
	return g.OSRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// fifoGitignoreRoot makes a projects root of n git repos, each with a FIFO
// .gitignore and a .env.
func fifoGitignoreRoot(t *testing.T, n int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		repo := filepath.Join(root, fmt.Sprintf("r%d", i))
		init := osexec.Command("git", "init", "-q", repo) //nolint:gosec,noctx // G204: test setup running git with fixed arguments
		if out, err := init.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		if err := syscall.Mkfifo(filepath.Join(repo, ".gitignore"), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestAuditSecrets_GitBudgetAloneExits1: with gitleaks off, the budget
// running out on git status is still a partial result: exit 1.
//
// Mutation that turns it red: drop the GitBudgetExhausted verdict.
func TestAuditSecrets_GitBudgetAloneExits1(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := fifoGitignoreRoot(t, 3)
	cmd := newAuditSecretsCmd(auditSecretsDeps{
		resolveRoot:    func() (string, error) { return root, nil },
		runner:         exec.OSRunner{},
		lookPath:       absentLookPath,
		gitRepoTimeout: 300 * time.Millisecond,
	})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--gitleaks=off", "--timeout=400ms"})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "scan budget ran out") {
		t.Errorf("err = %v, want exit 1 naming the spent budget", err)
	}
	if !strings.Contains(out.String(), "scan budget ran out before git status answered") {
		t.Errorf("text output lacks the budget note:\n%s", out.String())
	}
}

// TestAuditSecrets_OneBudgetBoundsFIFORepos: N repos whose .gitignore is a
// FIFO each hang git until their slice ends. With 300ms slices and a 1s
// budget, the whole verb takes about the budget, the repos past it are
// marked budget-exhausted without git starting, gitleaks is skipped
// budget_exhausted for every repo without running, and the exit is 1.
//
// Mutations that turn it red: give each repo a slice of
// context.Background() instead of the budget (the run then takes N slices);
// drop the budget verdict (exit 0).
func TestAuditSecrets_OneBudgetBoundsFIFORepos(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const repos = 8
	root := fifoGitignoreRoot(t, repos)
	bin := filepath.Join(t.TempDir(), "gitleaks")
	if err := os.WriteFile(bin, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{}
	cmd := newAuditSecretsCmd(auditSecretsDeps{
		resolveRoot:    func() (string, error) { return root, nil },
		runner:         gitRealRunner{fake: fake, bin: bin},
		lookPath:       func(string) (string, error) { return bin, nil },
		gitRepoTimeout: 300 * time.Millisecond,
	})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--json", "--timeout=1s"})
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- cmd.Execute() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("audit secrets ran far past its 1s budget")
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond || elapsed > 6*time.Second {
		t.Errorf("took %s, want about the 1s budget", elapsed)
	}
	if err == nil || ExitCode(err) != 1 {
		t.Errorf("err = %v, want exit 1 for a partial result", err)
	}
	var got struct {
		GitFailed int    `json:"git_status_failed_repos"`
		GitBudget int    `json:"git_status_budget_exhausted_repos"`
		Timeout   string `json:"timeout"`
		Findings  []struct {
			Flags []string `json:"flags"`
		} `json:"findings"`
		Gitleaks struct {
			Status  string `json:"status"`
			Skipped []struct {
				Repo, Reason string
			} `json:"repos_skipped"`
		} `json:"gitleaks"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got.GitFailed != repos || got.GitBudget < repos-4 || got.Timeout != "1s" {
		t.Errorf("git failed=%d budget-exhausted=%d timeout=%q; want all %d failed, most past the budget", got.GitFailed, got.GitBudget, got.Timeout, repos)
	}
	for _, f := range got.Findings {
		if !slices.Contains(f.Flags, audit.FlagGitUnknown) {
			t.Errorf("finding flags %v, want git-unknown", f.Flags)
		}
	}
	if got.Gitleaks.Status != gitleaks.StatusTimedOut || len(got.Gitleaks.Skipped) != repos {
		t.Errorf("gitleaks = %+v, want timed_out with every repo skipped", got.Gitleaks)
	}
	for _, sk := range got.Gitleaks.Skipped {
		if sk.Reason != gitleaks.SkipBudgetExhausted {
			t.Errorf("skip reason %q, want budget_exhausted", sk.Reason)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("gitleaks was started %d times on a spent budget", len(fake.Calls))
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

// TestAuditSecrets_UnsafeScannerConfigPastTheFindingsCap: the skip set comes
// from the walk's own check as it enters a repo, so a FIFO .gitleaksignore
// the walk never visited (the findings cap stopped it first) still keeps
// the repo from gitleaks.
//
// Mutation that turns it red: build gitleaksSkips from the findings list.
func TestAuditSecrets_UnsafeScannerConfigPastTheFindingsCap(t *testing.T) {
	fx := newSecretsFixture(t)
	if err := os.Remove(filepath.Join(fx.repo, ".gitleaks.toml")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fx.repo, ".gitleaksignore"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	report, err := audit.ScanSecrets(audit.SecretsOptions{Root: fx.root, MaxFindings: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		if f.Kind == audit.KindScannerConfig {
			t.Fatalf("the cap let the scanner config through (%+v); the test needs it cut", f)
		}
	}
	if skip := gitleaksSkips(report); skip[fx.repo] != gitleaks.SkipScannerConfigNotRegular {
		t.Errorf("skips = %v, want %s skipped", skip, fx.repo)
	}
}
