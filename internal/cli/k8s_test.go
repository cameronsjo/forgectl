package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	k8spkg "github.com/cameronsjo/forgectl/internal/k8s"
)

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

var _ forgexec.StreamingRunner = (*cliStreamingRunner)(nil)
var _ forgexec.Runner = (*forgexec.FakeRunner)(nil)
