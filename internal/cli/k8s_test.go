package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	k8spkg "github.com/cameronsjo/forgectl/internal/k8s"
)

// realExitError runs a trivial child process that exits with code so tests
// can exercise a genuine *exec.ExitError — the shape OSRunner.RunInteractive
// actually returns in production, which a hand-built *forgexec.CommandError
// is not.
func realExitError(t *testing.T, code int) error {
	t.Helper()
	script := "exit " + strconv.Itoa(code)
	err := exec.CommandContext(t.Context(), "sh", "-c", script).Run() // #nosec G204 -- test helper; script is built from an integer literal
	if err == nil {
		t.Fatalf("sh -c %s unexpectedly succeeded", script)
	}
	return err
}

type cliStreamingRunner struct {
	name  string
	args  []string
	calls int
	run   func(io.Writer, io.Writer) error
}

func (r *cliStreamingRunner) RunStreaming(_ context.Context, _ io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	r.calls++
	r.name = name
	r.args = append([]string(nil), args...)
	if r.run != nil {
		return r.run(stdout, stderr)
	}
	return nil
}

func executeK8sLogs(t *testing.T, runner *cliStreamingRunner, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()
	cmd := newK8sCmdForClient(k8spkg.New(runner), &forgexec.FakeRunner{})
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"logs"}, args...))
	return stdout, stderr, cmd.ExecuteContext(context.Background())
}

func TestK8sLogs_ForwardsOrdinaryKubectlArgsAndConsumesOnlyHelperFlags(t *testing.T) {
	runner := &cliStreamingRunner{}
	_, _, err := executeK8sLogs(t, runner,
		"-n", "prod", "-l", "app=api", "-f",
		"--log-level", "warn", "--color=never", "--all-containers",
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runner.name != "kubectl" {
		t.Errorf("name = %q, want kubectl", runner.name)
	}
	want := []string{"logs", "-n", "prod", "-l", "app=api", "-f", "--all-containers"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %#v, want %#v", runner.args, want)
	}
	if runner.calls != 1 {
		t.Errorf("calls = %d, want 1", runner.calls)
	}
}

func TestK8sLogs_DoubleDashStopsHelperFlagConsumption(t *testing.T) {
	runner := &cliStreamingRunner{}
	_, _, err := executeK8sLogs(t, runner, "pod/api", "--", "--color", "always", "--log-level=debug")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"logs", "pod/api", "--color", "always", "--log-level=debug"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %#v, want %#v", runner.args, want)
	}
}

func TestK8sLogs_InvalidHelperFlagsRefuseBeforeKubectl(t *testing.T) {
	for _, tc := range [][]string{
		{"pod/api", "--log-level", "verbose"},
		{"pod/api", "--color", "sometimes"},
		{"pod/api", "--log-level"},
		{"pod/api", "--color"},
		{},
	} {
		runner := &cliStreamingRunner{}
		_, _, err := executeK8sLogs(t, runner, tc...)
		if err == nil {
			t.Errorf("args %q: expected error", tc)
		}
		if runner.calls != 0 {
			t.Errorf("args %q: kubectl calls = %d, want 0", tc, runner.calls)
		}
	}
}

func TestK8sLogs_HelpDoesNotInvokeKubectl(t *testing.T) {
	runner := &cliStreamingRunner{}
	stdout, _, err := executeK8sLogs(t, runner, "--help")
	if err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	if runner.calls != 0 {
		t.Errorf("kubectl calls = %d, want 0", runner.calls)
	}
	if !strings.Contains(stdout.String(), "--log-level") || !strings.Contains(stdout.String(), "NO_COLOR") {
		t.Errorf("help missing helper contract: %q", stdout.String())
	}
}

func TestK8sLogs_KubectlHelpAfterAResourceIsForwarded(t *testing.T) {
	runner := &cliStreamingRunner{}
	_, _, err := executeK8sLogs(t, runner, "pod/api", "--help")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := runner.args, []string{"logs", "pod/api", "--help"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %#v, want %#v", got, want)
	}
}

func TestK8sLogs_ColorPolicyHonorsNOColorAndExplicitAlways(t *testing.T) {
	previous := k8sOutputIsTerminal
	k8sOutputIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { k8sOutputIsTerminal = previous })

	line := `{"level":"error","message":"failed"}` + "\n"
	newRunner := func() *cliStreamingRunner {
		return &cliStreamingRunner{run: func(stdout, _ io.Writer) error {
			_, err := io.WriteString(stdout, line)
			return err
		}}
	}

	t.Setenv("NO_COLOR", "")
	auto := newRunner()
	stdout, _, err := executeK8sLogs(t, auto, "pod/api")
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if strings.ContainsRune(stdout.String(), '\x1b') {
		t.Errorf("auto output colored despite NO_COLOR presence: %q", stdout.String())
	}

	always := newRunner()
	stdout, _, err = executeK8sLogs(t, always, "pod/api", "--color", "always")
	if err != nil {
		t.Fatalf("always: %v", err)
	}
	if !strings.ContainsRune(stdout.String(), '\x1b') {
		t.Errorf("--color always output has no style: %q", stdout.String())
	}
}

func TestK8sLogs_OptsIntoKubectlExitCode(t *testing.T) {
	sentinel := errors.New("exit status 7")
	runner := &cliStreamingRunner{run: func(_, _ io.Writer) error {
		return &forgexec.CommandError{Name: "kubectl", ExitCode: 7, Err: sentinel}
	}}
	_, _, err := executeK8sLogs(t, runner, "pod/api")
	if err == nil {
		t.Fatal("expected kubectl failure")
	}
	if got := ExitCode(err); got != 7 {
		t.Errorf("ExitCode = %d, want 7", got)
	}
}

func TestK8sLogs_CancellationRemainsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &cliStreamingRunner{}
	cmd := newK8sCmdForClient(k8spkg.New(runner), &forgexec.FakeRunner{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"logs", "pod/api"})
	err := cmd.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if runner.calls != 0 {
		t.Errorf("kubectl calls = %d, want 0", runner.calls)
	}
}

func executeK8sNs(t *testing.T, runner *forgexec.FakeRunner, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	cmd := newK8sCmdForClient(k8spkg.New(&cliStreamingRunner{}), runner)
	stdout := new(bytes.Buffer)
	cmd.SetOut(stdout)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"ns"}, args...))
	return stdout, cmd.ExecuteContext(context.Background())
}

func TestK8sNs_NoArgsPrintsCurrentNamespace(t *testing.T) {
	runner := &forgexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "staging", nil
	}}
	stdout, err := executeK8sNs(t, runner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := stdout.String(), "staging\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	want := []string{"config", "view", "--minify", "-o", "jsonpath={..namespace}"}
	if got := runner.Last(); got.Name != "kubectl" || !reflect.DeepEqual(got.Args, want) {
		t.Errorf("call = %#v, want kubectl %#v", got, want)
	}
}

func TestK8sNs_NoArgsFallsBackToDefaultWhenUnset(t *testing.T) {
	runner := &forgexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "", nil
	}}
	stdout, err := executeK8sNs(t, runner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := stdout.String(), "default\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestK8sNs_OneArgSetsCurrentContextNamespace(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	stdout, err := executeK8sNs(t, runner, "prod")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	want := []string{"config", "set-context", "--current", "--namespace=prod"}
	if got := runner.Last(); got.Name != "kubectl" || !reflect.DeepEqual(got.Args, want) {
		t.Errorf("call = %#v, want kubectl %#v", got, want)
	}
}

func TestK8sNs_TwoArgsRejected(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	_, err := executeK8sNs(t, runner, "prod", "extra")
	if err == nil {
		t.Fatal("expected error for two args")
	}
	if len(runner.Calls) != 0 {
		t.Errorf("kubectl calls = %d, want 0", len(runner.Calls))
	}
}

func TestK8sNs_EmptyOrWhitespaceArgRejectedBeforeKubectl(t *testing.T) {
	for _, arg := range []string{"", "   "} {
		runner := &forgexec.FakeRunner{}
		_, err := executeK8sNs(t, runner, arg)
		if err == nil {
			t.Errorf("arg %q: expected error", arg)
		}
		if len(runner.Calls) != 0 {
			t.Errorf("arg %q: kubectl calls = %d, want 0", arg, len(runner.Calls))
		}
	}
}

func TestK8sNs_OptsIntoKubectlExitCode(t *testing.T) {
	sentinel := errors.New("exit status 1")
	runner := &forgexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "", &forgexec.CommandError{Name: "kubectl", ExitCode: 1, Err: sentinel}
	}}
	_, err := executeK8sNs(t, runner)
	if err == nil {
		t.Fatal("expected kubectl failure")
	}
	if got := ExitCode(err); got != 1 {
		t.Errorf("ExitCode = %d, want 1", got)
	}
}

func executeK8sExec(t *testing.T, runner *forgexec.FakeRunner, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	cmd := newK8sCmdForClient(k8spkg.New(&cliStreamingRunner{}), runner)
	stdout := new(bytes.Buffer)
	cmd.SetOut(stdout)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"exec"}, args...))
	return stdout, cmd.ExecuteContext(context.Background())
}

func TestK8sExec_ForwardsArgvVerbatimAndRunsInteractively(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	_, err := executeK8sExec(t, runner, "-it", "pod/api", "-c", "sidecar", "--", "sh")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"exec", "-it", "pod/api", "-c", "sidecar", "--", "sh"}
	got := runner.Last()
	if got.Name != "kubectl" || !reflect.DeepEqual(got.Args, want) {
		t.Errorf("call = %#v, want kubectl %#v", got, want)
	}
	if !got.Interactive {
		t.Errorf("call.Interactive = false, want true")
	}
}

func TestK8sExec_NoArgsRefusesBeforeKubectl(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	_, err := executeK8sExec(t, runner)
	if err == nil {
		t.Fatal("expected error for missing args")
	}
	if len(runner.Calls) != 0 {
		t.Errorf("kubectl calls = %d, want 0", len(runner.Calls))
	}
}

func TestK8sExec_HelpDoesNotInvokeKubectl(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	stdout, err := executeK8sExec(t, runner, "--help")
	if err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("kubectl calls = %d, want 0", len(runner.Calls))
	}
	if !strings.Contains(stdout.String(), "kubectl exec") {
		t.Errorf("help missing kubectl exec contract: %q", stdout.String())
	}
}

func TestK8sExec_KubectlHelpAfterAResourceIsForwarded(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	_, err := executeK8sExec(t, runner, "pod/api", "--help")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"exec", "pod/api", "--help"}
	if got := runner.Last(); got.Name != "kubectl" || !reflect.DeepEqual(got.Args, want) {
		t.Errorf("args = %#v, want %#v", got, want)
	}
}

func TestK8sExec_OptsIntoKubectlExitCode(t *testing.T) {
	// OSRunner.RunInteractive returns cmd.Run()'s error unwrapped — a bare
	// *exec.ExitError, never a *forgexec.CommandError — so the fake must
	// produce the same shape production actually returns.
	runner := &forgexec.FakeRunner{InteractiveErr: realExitError(t, 126)}
	_, err := executeK8sExec(t, runner, "pod/api", "--", "sh")
	if err == nil {
		t.Fatal("expected kubectl failure")
	}
	if got := ExitCode(err); got != 126 {
		t.Errorf("ExitCode = %d, want 126", got)
	}
}

func executeK8sInspect(t *testing.T, runner *forgexec.FakeRunner, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	cmd := newK8sCmdForClient(k8spkg.New(&cliStreamingRunner{}), runner)
	stdout := new(bytes.Buffer)
	cmd.SetOut(stdout)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"inspect"}, args...))
	return stdout, cmd.ExecuteContext(context.Background())
}

func TestK8sInspect_RunsDescribeGetEventsInOrder(t *testing.T) {
	runner := &forgexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "output for " + strings.Join(args, " "), nil
	}}
	stdout, err := executeK8sInspect(t, runner, "deployment/api", "-n", "prod")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantCalls := [][]string{
		{"describe", "deployment/api", "-n", "prod"},
		{"get", "deployment/api", "-o", "wide", "-n", "prod"},
		{"get", "events", "--field-selector", "involvedObject.name=api", "-n", "prod"},
	}
	if len(runner.Calls) != len(wantCalls) {
		t.Fatalf("calls = %d, want %d (%#v)", len(runner.Calls), len(wantCalls), runner.Calls)
	}
	for i, want := range wantCalls {
		got := runner.Calls[i]
		if got.Name != "kubectl" || !reflect.DeepEqual(got.Args, want) {
			t.Errorf("call %d = %#v, want kubectl %#v", i, got, want)
		}
	}
	out := stdout.String()
	describeIdx := strings.Index(out, "== describe ==")
	getIdx := strings.Index(out, "== get -o wide ==")
	eventsIdx := strings.Index(out, "== events ==")
	if describeIdx < 0 || getIdx < 0 || eventsIdx < 0 || describeIdx >= getIdx || getIdx >= eventsIdx {
		t.Errorf("sections out of order or missing: %q", out)
	}
}

func TestK8sInspect_RequiresKindSlashName(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	_, err := executeK8sInspect(t, runner, "api")
	if err == nil {
		t.Fatal("expected error for missing kind/name")
	}
	if len(runner.Calls) != 0 {
		t.Errorf("kubectl calls = %d, want 0", len(runner.Calls))
	}
}

// TestK8sInspect_RejectsFlagShapedWorkload guards the flag-injection finding
// from the pre-merge security review: a "contains a slash, has a name" check
// alone accepts a kubectl global flag whose value happens to include a '/',
// which kubectl then parses as its own flag (e.g. --server redirecting the
// call, with its bearer token, to an attacker-chosen host).
func TestK8sInspect_RejectsFlagShapedWorkload(t *testing.T) {
	for _, workload := range []string{
		"--kubeconfig=/tmp/evil.yaml",
		"--server=https://attacker.example/x",
		"--as=cluster-admin/x",
		"-n/x",
		"pod/api,involvedObject.namespace=kube-system",
	} {
		runner := &forgexec.FakeRunner{}
		_, err := executeK8sInspect(t, runner, workload)
		if err == nil {
			t.Errorf("workload %q: expected rejection", workload)
		}
		if len(runner.Calls) != 0 {
			t.Errorf("workload %q: kubectl calls = %d, want 0", workload, len(runner.Calls))
		}
	}
}

func TestK8sInspect_NoArgsRefusesBeforeKubectl(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	_, err := executeK8sInspect(t, runner)
	if err == nil {
		t.Fatal("expected error for missing args")
	}
	if len(runner.Calls) != 0 {
		t.Errorf("kubectl calls = %d, want 0", len(runner.Calls))
	}
}

func TestK8sInspect_StopsAtFirstFailure(t *testing.T) {
	sentinel := errors.New("exit status 1")
	runner := &forgexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", &forgexec.CommandError{Name: "kubectl", ExitCode: 1, Err: sentinel}
	}}
	_, err := executeK8sInspect(t, runner, "pod/api")
	if err == nil {
		t.Fatal("expected kubectl failure")
	}
	if got := ExitCode(err); got != 1 {
		t.Errorf("ExitCode = %d, want 1", got)
	}
	if len(runner.Calls) != 1 {
		t.Errorf("kubectl calls = %d, want 1 (should stop after describe fails)", len(runner.Calls))
	}
}

func TestK8sInspect_HelpDoesNotInvokeKubectl(t *testing.T) {
	runner := &forgexec.FakeRunner{}
	stdout, err := executeK8sInspect(t, runner, "--help")
	if err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("kubectl calls = %d, want 0", len(runner.Calls))
	}
	if !strings.Contains(stdout.String(), "describe/get/events triple") {
		t.Errorf("help missing inspect contract: %q", stdout.String())
	}
}

var _ forgexec.StreamingRunner = (*cliStreamingRunner)(nil)
var _ forgexec.Runner = (*forgexec.FakeRunner)(nil)
