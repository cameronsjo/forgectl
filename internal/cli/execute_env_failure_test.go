package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
)

// stubEnvFailure makes CaptureEnvSnapshot fail the way a relative
// $XDG_CONFIG_HOME or an unset $HOME does, replaces the logger with a no-op,
// and hands Execute a root whose `resume snapshot` records that it ran. It
// returns a pointer to that record and to whether the boundary was prepared.
func stubEnvFailure(t *testing.T) (ran, boundary *bool) {
	t.Helper()
	ran, boundary = new(bool), new(bool)
	prevEnv, prevBoundary, prevLogger, prevRoot := captureEnvSnapshot, prepareLegacyBoundary, setupLogger, buildRoot
	t.Cleanup(func() {
		captureEnvSnapshot, prepareLegacyBoundary, setupLogger, buildRoot = prevEnv, prevBoundary, prevLogger, prevRoot
	})
	captureEnvSnapshot = func() (config.EnvSnapshot, error) {
		return config.EnvSnapshot{}, errors.New("resolve user config directory: path in $XDG_CONFIG_HOME is relative")
	}
	prepareLegacyBoundary = func(config.EnvSnapshot, config.MigrationFS) (*config.LegacyMigrationBoundary, error) {
		*boundary = true
		return nil, errors.New("sentinel")
	}
	setupLogger = func(config.Config) io.Closer { return io.NopCloser(nil) }
	buildRoot = func(module.Deps) *cobra.Command {
		root := &cobra.Command{Use: "forgectl", SilenceErrors: true, SilenceUsage: true}
		resume := &cobra.Command{Use: "resume"}
		snapshot := &cobra.Command{Use: "snapshot", RunE: func(*cobra.Command, []string) error {
			*ran = true
			return nil
		}}
		snapshot.Flags().Bool("quiet", false, "")
		resume.AddCommand(snapshot)
		root.AddCommand(resume)
		return root
	}
	return ran, boundary
}

// TestExecute_EnvFailureStillRunsHookVerb pins #738 item 7: `resume
// snapshot` is wired to every session's Stop hook and documented to always
// exit 0, but a CaptureEnvSnapshot failure returned before the config gate's
// hook exemption could apply, so the hook exited 1 with nothing on stderr.
func TestExecute_EnvFailureStillRunsHookVerb(t *testing.T) {
	ran, boundary := stubEnvFailure(t)
	withArgs(t, "resume", "snapshot", "--quiet")

	if err := Execute(context.Background()); err != nil {
		t.Fatalf("Execute = %v, want nil: a hook verb must not fail over the environment", err)
	}
	if !*ran {
		t.Error("resume snapshot did not run")
	}
	if *boundary {
		t.Error("the legacy boundary was prepared from an environment that failed to capture")
	}
}

// TestExecute_EnvFailureStopsOtherVerbs is the control: every other verb
// still stops at the failure, before the command tree is built.
func TestExecute_EnvFailureStopsOtherVerbs(t *testing.T) {
	for _, argv := range [][]string{{"resume"}, {"resume", "list"}, {"snapshot"}, {"config"}} {
		ran, _ := stubEnvFailure(t)
		rootBuilt := false
		inner := buildRoot
		buildRoot = func(d module.Deps) *cobra.Command { rootBuilt = true; return inner(d) }
		withArgs(t, argv...)

		if err := Execute(context.Background()); err == nil {
			t.Errorf("Execute(%q) = nil, want the environment error", argv)
		}
		if rootBuilt || *ran {
			t.Errorf("Execute(%q) went on past the environment failure", argv)
		}
	}
}

func TestInvokesHookVerb(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"resume", "snapshot"}, true},
		{[]string{"--no-icons", "resume", "--quiet", "snapshot"}, true},
		{[]string{"resume", "snapshot", "--quiet"}, true},
		{[]string{"resume"}, false},
		{[]string{"resume", "list"}, false},
		{[]string{"snapshot"}, false},
		{[]string{"surface", "event", "--harness", "claude"}, true},
		{[]string{"surface", "send", "coord", "hi"}, false},
		{nil, false},
	} {
		if got := invokesHookVerb(c.args); got != c.want {
			t.Errorf("invokesHookVerb(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}
