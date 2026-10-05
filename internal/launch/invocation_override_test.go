package launch

import (
	"errors"
	"slices"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// strictRepoConfig is a launch config whose project block is stricter than any
// default on both harnesses' posture axes: plan mode with no danger flag for
// claude, and an untrusted, read-only sandbox for codex.
func strictRepoConfig(match, harness, model string) config.LaunchConfig {
	allowDefault := true
	allowRepo := false
	return config.LaunchConfig{
		Defaults: config.LaunchDefaults{
			PermissionMode: "acceptEdits",
			AllowDanger:    &allowDefault,
			ApprovalPolicy: "never",
			Sandbox:        "danger-full-access",
		},
		Projects: []config.LaunchProject{{
			Match:          match,
			Harness:        harness,
			Model:          model,
			PermissionMode: "plan",
			AllowDanger:    &allowRepo,
			ApprovalPolicy: "untrusted",
			Sandbox:        "read-only",
		}},
	}
}

// TestBuildInvocation_HarnessOverrideKeepsRepoPosture is the plan's posture
// negative control: a repo profile stricter than the defaults stays strict when
// --harness switches the harness. A switch that rebuilt the posture from the
// defaults would hand a codex worker danger-full-access in a repo that asked
// for read-only, with nothing in the argv but the flag to show it.
func TestBuildInvocation_HarnessOverrideKeepsRepoPosture(t *testing.T) {
	target := projectDir(t)
	bin := ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH}

	t.Run("claude repo to codex", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         strictRepoConfig(target, "claude", "sonnet"),
			CWD:            target,
			Harness:        "codex",
			Resolve:        fixedResolver(bin),
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if built.Invocation.Harness != "codex" {
			t.Fatalf("harness = %q, want codex", built.Invocation.Harness)
		}
		if built.Profile.Model != "" {
			t.Errorf("model = %q, want the codex built-in (empty): a claude model must not cross", built.Profile.Model)
		}
		args := built.Invocation.Args
		if !containsPair(args, "--sandbox", "read-only") {
			t.Errorf("argv %q lost the repo's read-only sandbox", args)
		}
		if !containsPair(args, "--ask-for-approval", "untrusted") {
			t.Errorf("argv %q lost the repo's untrusted approval policy", args)
		}
	})

	t.Run("codex repo to claude", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         strictRepoConfig(target, "codex", ""),
			CWD:            target,
			Harness:        "claude",
			Resolve:        fixedResolver(bin),
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if built.Invocation.Harness != "claude" {
			t.Fatalf("harness = %q, want claude", built.Invocation.Harness)
		}
		args := built.Invocation.Args
		if !containsPair(args, "--permission-mode", "plan") {
			t.Errorf("argv %q lost the repo's plan permission mode", args)
		}
		if slices.Contains(args, "--allow-dangerously-skip-permissions") {
			t.Errorf("argv %q gained the danger flag the repo turned off", args)
		}
	})
}

// TestBuildInvocation_HarnessOverrideSameHarnessKeepsModel pins the no-op case:
// naming the harness the profile already has must not reset its model.
func TestBuildInvocation_HarnessOverrideSameHarnessKeepsModel(t *testing.T) {
	target := projectDir(t)
	built, err := BuildInvocation(InvocationRequest{
		StdoutTerminal: true,
		Config:         strictRepoConfig(target, "claude", "sonnet"),
		CWD:            target,
		Harness:        "claude",
		Resolve:        fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if built.Profile.Model != "sonnet" {
		t.Errorf("model = %q, want the repo's sonnet", built.Profile.Model)
	}
}

// TestBuildInvocation_HarnessOverrideRefusesPi pins the v1 refusal: pi has no
// posture flag forgectl can pass, so an override to it is refused before the
// resolver runs — as is anything outside the closed set.
func TestBuildInvocation_HarnessOverrideRefusesPi(t *testing.T) {
	target := projectDir(t)
	for _, harness := range []string{"pi", "bash", "Claude"} {
		resolved := false
		_, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         strictRepoConfig(target, "claude", ""),
			CWD:            target,
			Harness:        harness,
			Resolve: func(string, config.LaunchDefaults) (ResolvedBinary, error) {
				resolved = true
				return ResolvedBinary{Path: "/stub/x", Source: BinaryPATH}, nil
			},
		})
		if !errors.Is(err, ErrHarnessOverride) {
			t.Errorf("override %q: err = %v, want ErrHarnessOverride", harness, err)
		}
		if resolved {
			t.Errorf("override %q reached the resolver before being refused", harness)
		}
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestBuildInvocation_WorkerFloor pins the T1 worker posture: pi refused from
// the profile as well as the flag, the danger flag forced off on a default
// config, and an explicit bypass or full-access sandbox refused.
func TestBuildInvocation_WorkerFloor(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	allow := true

	t.Run("default config drops the danger flag", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{AllowDanger: &allow}},
			CWD:            target, Worker: true, Resolve: bin,
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if slices.Contains(built.Invocation.Args, "--allow-dangerously-skip-permissions") {
			t.Errorf("worker argv %q carries the danger flag", built.Invocation.Args)
		}
	})

	t.Run("repo add_dir is dropped", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{AddDir: []string{"/elsewhere"}}},
			CWD:            target, Worker: true, Resolve: bin,
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if slices.Contains(built.Invocation.Args, "--add-dir") {
			t.Errorf("worker argv %q reaches past its worktree", built.Invocation.Args)
		}
	})

	for name, lc := range map[string]config.LaunchConfig{
		"pi from the repo profile": {Projects: []config.LaunchProject{{Match: target, Harness: "pi"}}},
		"bypassPermissions":        {Defaults: config.LaunchDefaults{PermissionMode: "bypassPermissions"}},
		"danger-full-access":       {Defaults: config.LaunchDefaults{Harness: "codex", Sandbox: "danger-full-access"}},
		"claude auto mode":         {Defaults: config.LaunchDefaults{PermissionMode: "auto"}},
		"claude dontAsk":           {Defaults: config.LaunchDefaults{PermissionMode: "dontAsk"}},
		"claude unknown mode":      {Defaults: config.LaunchDefaults{PermissionMode: "acceptEdit"}},
		"codex never asks":         {Defaults: config.LaunchDefaults{Harness: "codex", Sandbox: "workspace-write", ApprovalPolicy: "never"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BuildInvocation(InvocationRequest{Config: lc, CWD: target, Worker: true, Resolve: bin, StdoutTerminal: true})
			if !errors.Is(err, ErrWorkerPosture) {
				t.Fatalf("err = %v, want ErrWorkerPosture", err)
			}
		})
	}

	t.Run("non-worker launch is unchanged", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{AllowDanger: &allow}},
			CWD:            target, Resolve: bin,
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if !slices.Contains(built.Invocation.Args, "--allow-dangerously-skip-permissions") {
			t.Errorf("an ordinary launch lost its configured danger flag: %q", built.Invocation.Args)
		}
	})
}
