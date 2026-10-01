package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/audit/gitleaks"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// auditCanary is a fake secret planted in every field of the gitleaks
// report fixture that carries secret text.
const auditCanary = "ghp_AUDITc4n4ryC4N4RYc4n4ryC4N4RYc4n4" //nolint:gosec // G101: a fake token, the canary these tests look for

// secretsFixture is a projects root with one repo holding a tracked .env,
// an ignored one, a loose key, and scanner config, plus a gitleaks binary
// stand-in outside the root.
type secretsFixture struct {
	root, repo, bin string
	// tracked lists repo-relative paths git reports as tracked.
	tracked []string
}

func newSecretsFixture(t *testing.T) secretsFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "app")
	for rel, mode := range map[string]os.FileMode{
		".git/HEAD":      0o600,
		".env":           0o600,
		".env.local":     0o600,
		"deploy/id_rsa":  0o644,
		".gitleaks.toml": 0o600,
	} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "gitleaks")
	if err := os.WriteFile(bin, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return secretsFixture{root: root, repo: repo, bin: bin, tracked: []string{".env"}}
}

// gitleaksReport renders one finding per file, each with the canary in
// every field gitleaks writes secret text into.
func gitleaksReport(files ...string) string {
	var rows []map[string]any
	for i, f := range files {
		rows = append(rows, map[string]any{
			"RuleID": "github-pat", "Description": auditCanary, "StartLine": i + 3,
			"Match": "t=" + auditCanary, "Secret": auditCanary, "Line": "t=" + auditCanary,
			"File": f, "Message": auditCanary, "Tags": []string{auditCanary},
			"Fingerprint": fmt.Sprintf("%s:github-pat:%d", f, i+3),
		})
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// secretsRunner answers git ls-files from fx.tracked and gitleaks through
// onGitleaks (nil: report one finding in conf.txt).
func secretsRunner(t *testing.T, fx secretsFixture, onGitleaks func(args []string) (string, error)) *exec.FakeRunner {
	t.Helper()
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch name {
		case gitenv.Bin:
			var out strings.Builder
			for _, a := range args {
				rel, ok := strings.CutPrefix(a, ":(literal)")
				if ok && slices.Contains(fx.tracked, rel) {
					out.WriteString("H " + rel + "\x00")
				}
			}
			return out.String(), nil
		case fx.bin:
			if len(args) == 1 && args[0] == "version" {
				return "8.30.1", nil
			}
			if onGitleaks != nil {
				return onGitleaks(args)
			}
			rp := args[slices.Index(args, "--report-path")+1]
			return "", os.WriteFile(filepath.Clean(rp), []byte(gitleaksReport(filepath.Join(args[len(args)-1], "conf.txt"))), 0o600)
		}
		t.Errorf("unexpected command %s %q", name, args)
		return "", errors.New("unexpected")
	}}
}

// runAuditSecrets runs the command and returns stdout, stderr and the error.
func runAuditSecrets(t *testing.T, fx secretsFixture, runner exec.Runner, look func(string) (string, error), args ...string) (string, string, error) {
	t.Helper()
	if look == nil {
		look = func(string) (string, error) { return fx.bin, nil }
	}
	cmd := newAuditSecretsCmd(auditSecretsDeps{
		resolveRoot: func() (string, error) { return fx.root, nil },
		runner:      runner,
		lookPath:    look,
	})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func absentLookPath(string) (string, error) { return "", errors.New("not found") }

// TestAuditSecrets_JSONShape pins the --json wire shape (ADR-0008: additive
// changes only): the exact key sets of the report, a native row, the
// gitleaks block and a gitleaks row, and the values for the fixture.
func TestAuditSecrets_JSONShape(t *testing.T) {
	fx := newSecretsFixture(t)
	stdout, _, err := runAuditSecrets(t, fx, secretsRunner(t, fx, nil), nil, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if keys := auditSortedKeys(got); keys != "capped_by,depth_skipped,entries_scanned,findings,git_status_failed_repos,gitleaks,ignored_env_files,repos_scanned,root,truncated,unreadable_dirs,unreadable_files" {
		t.Errorf("report keys = %s", keys)
	}
	findings, _ := got["findings"].([]any)
	if len(findings) != 3 {
		t.Fatalf("findings = %v, want the tracked .env, the key and the scanner config", findings)
	}
	row, _ := findings[0].(map[string]any)
	if keys := auditSortedKeys(row); keys != "flags,kind,path,repo,type" {
		t.Errorf("finding keys = %s", keys)
	}
	if row["path"] != filepath.Join(fx.repo, ".env") || row["kind"] != "env" || fmt.Sprint(row["flags"]) != "[tracked]" {
		t.Errorf("first finding = %v", row)
	}
	if got["ignored_env_files"] != float64(1) {
		t.Errorf("ignored_env_files = %v, want the ignored .env.local counted", got["ignored_env_files"])
	}
	gl, _ := got["gitleaks"].(map[string]any)
	if keys := auditSortedKeys(gl); keys != "findings,findings_rejected,min_version,mode,path,reason,repos_failed,repos_scanned,status,truncated,version" {
		t.Errorf("gitleaks keys = %s", keys)
	}
	if gl["status"] != "ran" || gl["mode"] != "auto" || gl["version"] != "8.30.1" || gl["min_version"] != gitleaks.MinVersion || gl["path"] != fx.bin {
		t.Errorf("gitleaks block = %v", gl)
	}
	glf, _ := gl["findings"].([]any)
	if len(glf) != 1 {
		t.Fatalf("gitleaks findings = %v", glf)
	}
	grow, _ := glf[0].(map[string]any)
	if keys := auditSortedKeys(grow); keys != "fingerprint,line,path,repo,rule" {
		t.Errorf("gitleaks finding keys = %s", keys)
	}
	if grow["path"] != filepath.Join(fx.repo, "conf.txt") || grow["repo"] != fx.repo || grow["rule"] != "github-pat" || grow["line"] != float64(3) {
		t.Errorf("gitleaks finding = %v", grow)
	}
}

// TestAuditSecrets_CanaryNowhere: a report carrying the canary in every
// secret-bearing field leaves it out of the text output, the JSON, stderr,
// the error and the log. Mutation that turns it red: decode Secret into
// the finding and print it in the text row.
func TestAuditSecrets_CanaryNowhere(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fx := newSecretsFixture(t)
	for _, args := range [][]string{nil, {"--json"}} {
		stdout, stderr, err := runAuditSecrets(t, fx, secretsRunner(t, fx, nil), nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "conf.txt") {
			t.Fatalf("%v: the gitleaks finding is missing, so the canary check is vacuous:\n%s", args, stdout)
		}
		for where, s := range map[string]string{"stdout": stdout, "stderr": stderr} {
			if strings.Contains(s, auditCanary) {
				t.Errorf("%v: canary in %s", args, where)
			}
		}
	}
	// A failing gitleaks whose stderr carries the canary: the error is a
	// category, never the CommandError's text.
	failing := secretsRunner(t, fx, func([]string) (string, error) {
		return "", &exec.CommandError{Name: fx.bin, Stderr: auditCanary, ExitCode: 1, Err: errors.New("exit status 1")}
	})
	stdout, stderr, err := runAuditSecrets(t, fx, failing, nil)
	if err == nil || strings.Contains(err.Error(), auditCanary) || strings.Contains(stdout+stderr, auditCanary) {
		t.Errorf("failure path: err=%v; canary leaked=%v", err, strings.Contains(fmt.Sprint(err)+stdout+stderr, auditCanary))
	}
	if strings.Contains(logs.String(), auditCanary) {
		t.Errorf("canary in the log:\n%s", logs.String())
	}
}

// TestAuditSecrets_HostileNamesEscaped: a directory name holding an escape
// sequence and a newline reaches the text output escaped, so it cannot
// forge a line or drive the terminal.
func TestAuditSecrets_HostileNamesEscaped(t *testing.T) {
	fx := newSecretsFixture(t)
	evil := filepath.Join(fx.root, "x\x1b]0;pwn\x07\nfake line", ".env")
	if err := os.MkdirAll(filepath.Dir(evil), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evil, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runAuditSecrets(t, fx, secretsRunner(t, fx, nil), absentLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(stdout, "\x1b\x07") {
		t.Errorf("raw control bytes reached the terminal:\n%q", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "fake line") {
			t.Errorf("the newline in the name forged a line: %q", line)
		}
	}
	if !strings.Contains(stdout, "outside-repo") {
		t.Errorf("the hostile .env was not listed:\n%s", stdout)
	}
}

// blockingRunner hangs each gitleaks scan until its context ends.
type blockingRunner struct {
	*exec.FakeRunner
	bin string
}

func (b blockingRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	if name == b.bin && len(args) > 1 {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return b.FakeRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// TestAuditSecrets_ExitCodesAndStatus pins the exit contract and that every
// output states what gitleaks did (ADR-0008 rules 3 and 4).
//
// Mutations that turn it red: exit 0 when gitleaks fails under auto; exit 1
// when it is merely absent under auto; let require pass without gitleaks;
// drop the status line from the text output.
func TestAuditSecrets_ExitCodesAndStatus(t *testing.T) {
	fx := newSecretsFixture(t)
	failing := func([]string) (string, error) { return "", errors.New("exit status 2") }
	cases := []struct {
		name     string
		runner   func() exec.Runner
		look     func(string) (string, error)
		args     []string
		code     int // 0 for no error
		status   string
		textHead string
	}{
		{"ran", func() exec.Runner { return secretsRunner(t, fx, nil) }, nil, nil, 0, "ran", "gitleaks 8.30.1: ran over 1 repos, 1 findings"},
		{"auto absent", func() exec.Runner { return secretsRunner(t, fx, nil) }, absentLookPath, nil, 0, "absent", "gitleaks: not run, gitleaks was not found on PATH; native checks only"},
		{"require absent", func() exec.Runner { return secretsRunner(t, fx, nil) }, absentLookPath, []string{"--gitleaks=require"}, 1, "absent", "gitleaks: not run"},
		{"auto failed", func() exec.Runner { return secretsRunner(t, fx, failing) }, nil, nil, 1, "failed", "gitleaks 8.30.1: FAILED in 1 of 1 repos"},
		{"auto timed out", func() exec.Runner { return blockingRunner{secretsRunner(t, fx, nil), fx.bin} }, nil, []string{"--gitleaks-timeout=50ms"}, 1, "timed_out", "gitleaks 8.30.1: TIMED OUT after 50ms"},
		{"off", func() exec.Runner { return secretsRunner(t, fx, nil) }, func(string) (string, error) {
			t.Error("--gitleaks=off looked gitleaks up")
			return "", errors.New("no")
		}, []string{"--gitleaks=off"}, 0, "off", "gitleaks: not run, --gitleaks=off; native checks only"},
	}
	for _, c := range cases {
		for _, asJSON := range []bool{false, true} {
			args := c.args
			if asJSON {
				args = append(append([]string{}, args...), "--json")
			}
			stdout, _, err := runAuditSecrets(t, fx, c.runner(), c.look, args...)
			code := 0
			if err != nil {
				code = ExitCode(err)
			}
			if code != c.code {
				t.Errorf("%s json=%v: exit %d (%v), want %d", c.name, asJSON, code, err, c.code)
			}
			if asJSON {
				var got struct {
					Gitleaks struct{ Status string } `json:"gitleaks"`
				}
				if err := json.Unmarshal([]byte(stdout), &got); err != nil || got.Gitleaks.Status != c.status {
					t.Errorf("%s: --json status %q (%v), want %q", c.name, got.Gitleaks.Status, err, c.status)
				}
				continue
			}
			if first, _, _ := strings.Cut(stdout, "\n"); !strings.HasPrefix(first, c.textHead) {
				t.Errorf("%s: first line %q, want it to start %q", c.name, first, c.textHead)
			}
		}
	}
}

// TestAuditSecrets_BadFlags exit 2.
func TestAuditSecrets_BadFlags(t *testing.T) {
	fx := newSecretsFixture(t)
	for _, args := range [][]string{{"--gitleaks=maybe"}, {"--gitleaks-timeout=0s"}, {"--gitleaks-timeout=-1m"}} {
		_, _, err := runAuditSecrets(t, fx, secretsRunner(t, fx, nil), nil, args...)
		if err == nil || ExitCode(err) != 2 {
			t.Errorf("%v: err %v, want exit 2", args, err)
		}
	}
}

// TestAuditSecrets_MissingRootFails: an unopenable root exits 1.
func TestAuditSecrets_MissingRootFails(t *testing.T) {
	fx := newSecretsFixture(t)
	fx.root = filepath.Join(fx.root, "nope")
	_, _, err := runAuditSecrets(t, fx, secretsRunner(t, fx, nil), absentLookPath)
	if err == nil || ExitCode(err) != 1 {
		t.Errorf("err = %v, want exit 1", err)
	}

}
