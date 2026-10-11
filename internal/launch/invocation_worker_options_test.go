package launch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// workerOptionsRequest is a claude worker request whose matched project block
// sets its own model and CLAUDE_CONFIG_DIR, so an override has something to
// beat.
func workerOptionsRequest(t *testing.T) InvocationRequest {
	t.Helper()
	target := projectDir(t)
	return InvocationRequest{
		Config: config.LaunchConfig{Projects: []config.LaunchProject{{
			Match: target, Model: "opus", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/from/profile/env"},
		}}},
		CWD:            target,
		BaseEnv:        []string{"HOME=/home/op", "CLAUDE_CONFIG_DIR=/from/launcher"},
		Resolve:        fixedResolver(ResolvedBinary{Path: "/stub/claude", Source: BinaryPATH}),
		Worker:         true,
		StdoutTerminal: true,
	}
}

func modelArg(args []string) string {
	if i := slices.Index(args, "--model"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestBuildInvocation_WorkerModelOverride(t *testing.T) {
	req := workerOptionsRequest(t)
	req.Model = "sonnet"
	built, err := BuildInvocation(req)
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if got := modelArg(built.Invocation.Args); got != "sonnet" {
		t.Fatalf("--model %q in %q, want the override sonnet", got, built.Invocation.Args)
	}
	if built.Profile.Effort != EffortForModel("sonnet") {
		t.Errorf("effort %q, want it derived again from the override (%q)", built.Profile.Effort, EffortForModel("sonnet"))
	}

	for name, mod := range map[string]func(*InvocationRequest){
		"leading dash":   func(r *InvocationRequest) { r.Model = "--dangerously-skip-permissions" },
		"space":          func(r *InvocationRequest) { r.Model = "opus -p" },
		"not a worker":   func(r *InvocationRequest) { r.Model, r.Worker = "sonnet", false },
		"codex + claude": func(r *InvocationRequest) { r.Model, r.Harness = "sonnet", "codex" },
	} {
		t.Run(name, func(t *testing.T) {
			r := workerOptionsRequest(t)
			mod(&r)
			if built, err := BuildInvocation(r); err == nil {
				t.Fatalf("accepted; argv %q", built.Invocation.Args)
			}
		})
	}
	r := workerOptionsRequest(t)
	r.Model = "-p"
	if _, err := BuildInvocation(r); !errors.Is(err, config.ErrInvalidModel) {
		t.Fatalf("leading '-': err %v, want config.ErrInvalidModel", err)
	}
}

func TestBuildInvocation_WorkerConfigDir(t *testing.T) {
	req := workerOptionsRequest(t)
	req.ConfigDir = "/home/op/.claude-work"
	built, err := BuildInvocation(req)
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if got := envMap(t, built.Invocation.Env)["CLAUDE_CONFIG_DIR"]; got != "/home/op/.claude-work" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want the profile's dir over the launcher's and the profile env's", got)
	}
	if n := countKey(built.Invocation.Env, "CLAUDE_CONFIG_DIR"); n != 1 {
		t.Fatalf("CLAUDE_CONFIG_DIR appears %d times", n)
	}

	// No profile: the launcher's value passes through the allowlist as before.
	plain := workerOptionsRequest(t)
	plain.Config.Projects[0].Env = nil
	built, err = BuildInvocation(plain)
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if got := envMap(t, built.Invocation.Env)["CLAUDE_CONFIG_DIR"]; got != "/from/launcher" {
		t.Fatalf("no profile: CLAUDE_CONFIG_DIR = %q, want the launcher's", got)
	}

	for name, mod := range map[string]func(*InvocationRequest){
		"relative":     func(r *InvocationRequest) { r.ConfigDir = "rel/dir" },
		"not a worker": func(r *InvocationRequest) { r.ConfigDir, r.Worker = "/abs", false },
		"codex worker": func(r *InvocationRequest) { r.ConfigDir, r.Harness = "/abs", "codex" },
	} {
		t.Run(name, func(t *testing.T) {
			r := workerOptionsRequest(t)
			r.Config.Projects[0].Model = ""
			mod(&r)
			if _, err := BuildInvocation(r); err == nil || !strings.Contains(err.Error(), "config dir") {
				t.Fatalf("err %v, want a config dir refusal", err)
			}
		})
	}
}
