package launch

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// TestBuildInvocation_HarnessOverrideClosedSet: an override outside claude,
// codex and pi is refused before the resolver runs.
func TestBuildInvocation_HarnessOverrideClosedSet(t *testing.T) {
	target := projectDir(t)
	for _, harness := range []string{"bash", "Claude", "gemini"} {
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

// TestBuildInvocation_HarnessOverrideToPi: an override to pi resolves the pi
// binary and drops the claude model, as a codex override does.
func TestBuildInvocation_HarnessOverrideToPi(t *testing.T) {
	target := projectDir(t)
	var asked string
	built, err := BuildInvocation(InvocationRequest{
		StdoutTerminal: true,
		Config:         strictRepoConfig(target, "claude", "sonnet"),
		CWD:            target,
		Harness:        "pi",
		Resolve: func(h string, _ config.LaunchDefaults) (ResolvedBinary, error) {
			asked = h
			return ResolvedBinary{Path: "/stub/pi", Source: BinaryPATH}, nil
		},
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	if asked != "pi" || built.Invocation.Harness != "pi" || built.Posture != PosturePiSession {
		t.Fatalf("resolved %q, harness %q, posture %q; want pi throughout", asked, built.Invocation.Harness, built.Posture)
	}
	if slices.Contains(built.Invocation.Args, "sonnet") {
		t.Errorf("pi argv %q carries the claude profile's model", built.Invocation.Args)
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

// TestBuildInvocation_WorkerFloor pins the T1 worker posture: the danger flag
// forced off on a default config, and an explicit bypass or full-access
// sandbox refused. Pi workers are allowed (operator request, 2026-10-08).
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
		"bypassPermissions":   {Worker: config.LaunchWorker{PermissionMode: "bypassPermissions"}},
		"danger-full-access":  {Defaults: config.LaunchDefaults{Harness: "codex"}, Worker: config.LaunchWorker{Sandbox: "danger-full-access"}},
		"claude dontAsk":      {Worker: config.LaunchWorker{PermissionMode: "dontAsk"}},
		"codex never asks":    {Defaults: config.LaunchDefaults{Harness: "codex"}, Worker: config.LaunchWorker{ApprovalPolicy: "never"}},
		"unknown worker mode": {Worker: config.LaunchWorker{PermissionMode: "acceptEdit"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BuildInvocation(InvocationRequest{Config: lc, CWD: target, Worker: true, Resolve: bin, StdoutTerminal: true})
			if !errors.Is(err, ErrWorkerPosture) {
				t.Fatalf("err = %v, want ErrWorkerPosture", err)
			}
		})
	}

	// forgectl#1060: a plan-mode worker's shell must prompt, not go to the
	// auto-mode classifier, so every claude worker carries the setting.
	for _, mode := range []string{"plan", "default", "manual", "acceptEdits", "auto"} {
		t.Run("claude worker in "+mode+" turns off auto mode during plan", func(t *testing.T) {
			built, err := BuildInvocation(InvocationRequest{
				StdoutTerminal: true,
				Config:         config.LaunchConfig{Worker: config.LaunchWorker{PermissionMode: mode}},
				CWD:            target, Worker: true, Resolve: bin,
			})
			if err != nil {
				t.Fatalf("BuildInvocation: %v", err)
			}
			// Exactly after the leading --permission-mode pair: anywhere later
			// could follow a variadic flag and swallow its list.
			// Literal, not built from the production constants, so a wrong
			// constant cannot pass by agreeing with itself. --mcp-config is
			// variadic in Claude Code, so the flag after its one value is load-
			// bearing: a bare token there would join the config list.
			// A worker is a full harness (ADR-0010, 2026-10-08): no isolation
			// flags, only the worker settings after the posture.
			want := []string{"--permission-mode", mode,
				"--settings", `{"useAutoModeDuringPlan":false}`,
			}
			// Only acceptEdits carries the allow list; a plan worker that could
			// commit and push unprompted would be looser than its mode.
			if mode == "acceptEdits" {
				want[len(want)-1] = `{"useAutoModeDuringPlan":false,"permissions":{` +
					`"allow":["Bash(go test *)","Bash(go build *)","Bash(make *)","Bash(git add *)","Bash(git commit *)","Bash(git push *)","Bash(gh pr create *)","Bash(gh pr view *)"]}}`
			}
			if args := built.Invocation.Args; len(args) < len(want) || !slices.Equal(args[:len(want)], want) {
				t.Errorf("worker argv %q, want it to start %q", args, want)
			}
			if slices.Contains(built.Invocation.Args, "--ide") {
				t.Errorf("worker argv %q connects to the operator's IDE", built.Invocation.Args)
			}
		})
	}

	// User args land after the posture, where last-flag-wins would let them
	// undo the floor, so a worker takes none. A passthrough call (--version,
	// a subcommand) is refused the same way, with the posture error.
	for name, args := range map[string][]string{
		"settings override": {"--settings", `{"useAutoModeDuringPlan":true}`},
		"mode override":     {"--permission-mode", "bypassPermissions"},
		"print prompt":      {"-p", "hello"},
		"version":           {"--version"},
		"subcommand":        {"agents", "--help"},
	} {
		t.Run("worker refuses user args: "+name, func(t *testing.T) {
			_, err := BuildInvocation(InvocationRequest{
				StdoutTerminal: true,
				Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "plan"}},
				CWD:            target, Worker: true, Resolve: bin, Args: args,
			})
			if !errors.Is(err, ErrWorkerPosture) {
				t.Fatalf("err = %v, want ErrWorkerPosture", err)
			}
		})
	}

	t.Run("codex worker gets no claude settings", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex", Sandbox: "workspace-write", ApprovalPolicy: "on-request"}},
			CWD:            target, Worker: true, Resolve: bin,
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if slices.Contains(built.Invocation.Args, "--settings") {
			t.Errorf("codex worker argv %q carries a claude flag", built.Invocation.Args)
		}
	})

	piConfig := config.LaunchConfig{Projects: []config.LaunchProject{{Match: target, Harness: "pi"}}}
	t.Run("pi worker from the repo profile", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true, Config: piConfig, CWD: target, Worker: true, Resolve: bin,
			Model: "qwen-27b", Prompt: "fix the login bug",
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		// Pi takes the model flag and the brief after `--`, and nothing else.
		want := []string{"--model", "qwen-27b", "--", "fix the login bug"}
		if !built.Worker || built.Invocation.Harness != "pi" || !slices.Equal(built.Invocation.Args, want) {
			t.Errorf("pi worker: worker %v harness %q argv %q, want a pi worker with %q",
				built.Worker, built.Invocation.Harness, built.Invocation.Args, want)
		}
	})

	t.Run("pi worker by override", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{StdoutTerminal: true, CWD: target, Worker: true, Resolve: bin, Harness: "pi"})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if built.Invocation.Harness != "pi" || len(built.Invocation.Args) != 0 {
			t.Errorf("pi override worker: harness %q argv %q, want pi with no args", built.Invocation.Harness, built.Invocation.Args)
		}
	})

	t.Run("pi worker refuses user args", func(t *testing.T) {
		_, err := BuildInvocation(InvocationRequest{StdoutTerminal: true, Config: piConfig, CWD: target, Worker: true, Resolve: bin,
			Args: []string{"--tools", "bash"}})
		if !errors.Is(err, ErrWorkerPosture) {
			t.Fatalf("err = %v, want ErrWorkerPosture", err)
		}
	})

	// Measured on Pi 1.0.4: an '@' argument is a file to attach, even after `--`.
	for _, prompt := range []string{"@README.md say OK", "\n  @notes.md"} {
		t.Run("pi worker refuses an @ brief", func(t *testing.T) {
			_, err := BuildInvocation(InvocationRequest{StdoutTerminal: true, Config: piConfig, CWD: target, Worker: true, Resolve: bin,
				Prompt: prompt})
			if err == nil || !strings.Contains(err.Error(), "'@'") {
				t.Fatalf("err = %v, want the '@' refusal", err)
			}
		})
	}

	t.Run("pi worker takes no session id or config dir", func(t *testing.T) {
		for _, req := range []InvocationRequest{
			{SessionID: "0b0e9a54-55a1-4c42-9b9e-6f3c3f6b8c11"},
			{ConfigDir: "/profiles/work"},
		} {
			req.StdoutTerminal, req.Config, req.CWD, req.Worker, req.Resolve = true, piConfig, target, true, bin
			if _, err := BuildInvocation(req); err == nil {
				t.Errorf("pi worker accepted %+v", req)
			}
		}
	})

	t.Run("floor refuses a harness it does not name", func(t *testing.T) {
		if _, err := applyWorkerFloor(Profile{Harness: "gemini"}); !errors.Is(err, ErrWorkerPosture) {
			t.Fatalf("err = %v, want ErrWorkerPosture", err)
		}
	})

	t.Run("non-worker launch has no worker settings", func(t *testing.T) {
		built, err := BuildInvocation(InvocationRequest{StdoutTerminal: true, CWD: target, Resolve: bin})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		if slices.Contains(built.Invocation.Args, "--settings") {
			t.Errorf("an ordinary launch got a --settings flag: %q", built.Invocation.Args)
		}
	})

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

// TestBuildInvocation_WorkerPrompt pins the first brief's channel: the last
// two argv elements, after the whole posture, behind a `--`, so a prompt that
// looks like a flag or a subcommand stays a prompt.
func TestBuildInvocation_WorkerPrompt(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	for name, lc := range map[string]config.LaunchConfig{
		"claude": {Defaults: config.LaunchDefaults{PermissionMode: "acceptEdits"}},
		"codex":  {Defaults: config.LaunchDefaults{Harness: "codex", Sandbox: "workspace-write", ApprovalPolicy: "on-request"}},
	} {
		for _, prompt := range []string{"Fix the bug.", "--permission-mode bypassPermissions", "mcp"} {
			t.Run(name+" "+prompt, func(t *testing.T) {
				built, err := BuildInvocation(InvocationRequest{
					StdoutTerminal: true, Config: lc, CWD: target, Worker: true, Resolve: bin, Prompt: prompt,
				})
				if err != nil {
					t.Fatalf("BuildInvocation: %v", err)
				}
				args := built.Invocation.Args
				if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != prompt {
					t.Fatalf("argv %q, want it to end with -- %q", args, prompt)
				}
				if slices.Index(args, "--") != len(args)-2 {
					t.Fatalf("argv %q has a -- before the prompt's", args)
				}
			})
		}
	}

	t.Run("only a worker takes a prompt", func(t *testing.T) {
		_, err := BuildInvocation(InvocationRequest{StdoutTerminal: true, CWD: target, Resolve: bin, Prompt: "hi"})
		if err == nil {
			t.Fatal("an ordinary launch accepted a prompt")
		}
	})
}

// TestBuildInvocation_WorkerSessionID pins --session-id: a claude worker gets
// it before the prompt's `--`; anything else, or a malformed id, is refused.
func TestBuildInvocation_WorkerSessionID(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	claude := config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "acceptEdits"}}
	codex := config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex", Sandbox: "workspace-write", ApprovalPolicy: "on-request"}}
	const id = "0f8e2c1a-3b4d-4e5f-8a6b-7c8d9e0f1a2b"

	built, err := BuildInvocation(InvocationRequest{
		StdoutTerminal: true, Config: claude, CWD: target, Worker: true, Resolve: bin, Prompt: "Fix it.", SessionID: id,
	})
	if err != nil {
		t.Fatalf("BuildInvocation: %v", err)
	}
	args := built.Invocation.Args
	i := slices.Index(args, "--session-id")
	if i < 0 || i+1 >= len(args) || args[i+1] != id || i > slices.Index(args, "--") {
		t.Fatalf("argv %q, want --session-id %s before the prompt's --", args, id)
	}

	refused := map[string]InvocationRequest{
		"not a worker":   {StdoutTerminal: true, Config: claude, CWD: target, Resolve: bin, SessionID: id},
		"a codex worker": {StdoutTerminal: true, Config: codex, CWD: target, Worker: true, Resolve: bin, SessionID: id},
		"uppercase":      {StdoutTerminal: true, Config: claude, CWD: target, Worker: true, Resolve: bin, SessionID: strings.ToUpper(id)},
		"a flag":         {StdoutTerminal: true, Config: claude, CWD: target, Worker: true, Resolve: bin, SessionID: "--permission-mode"},
	}
	for name, req := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := BuildInvocation(req); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestWorkerClaudeSettingsJSON pins the worker settings as data: both parse,
// turn off auto mode during plan, and deny nothing; only the acceptEdits
// settings pre-approve commands, and exactly the agreed list.
func TestWorkerClaudeSettingsJSON(t *testing.T) {
	wantAllow := []string{
		"Bash(go test *)", "Bash(go build *)", "Bash(make *)",
		"Bash(git add *)", "Bash(git commit *)", "Bash(git push *)",
		"Bash(gh pr create *)", "Bash(gh pr view *)",
	}
	for _, tc := range []struct {
		name, json string
		allow      []string
	}{
		{"workerClaudeSettings", workerClaudeSettings, nil},
		{"workerClaudeEditSettings", workerClaudeEditSettings, wantAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s struct {
				UseAutoModeDuringPlan *bool `json:"useAutoModeDuringPlan"`
				Permissions           struct {
					Allow []string `json:"allow"`
					Deny  []string `json:"deny"`
				} `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(tc.json), &s); err != nil {
				t.Fatalf("worker settings do not parse: %v", err)
			}
			if s.UseAutoModeDuringPlan == nil || *s.UseAutoModeDuringPlan {
				t.Error("useAutoModeDuringPlan is not false")
			}
			if len(s.Permissions.Deny) != 0 {
				t.Errorf("worker settings deny %q; a worker is a full harness (ADR-0010, 2026-10-08)", s.Permissions.Deny)
			}
			if !slices.Equal(s.Permissions.Allow, tc.allow) {
				t.Errorf("allow list = %q, want exactly %q (widening it needs a security review)", s.Permissions.Allow, tc.allow)
			}
		})
	}
}

// TestSessionArgsKeepsTheIDEForOrdinaryLaunches: only a worker is detached.
func TestSessionArgsKeepsTheIDEForOrdinaryLaunches(t *testing.T) {
	p := Profile{PermissionMode: "plan", Model: "opus"}
	if !slices.Contains(SessionArgs(p), "--ide") {
		t.Fatal("an ordinary session lost --ide")
	}
	p.Detached = true
	if slices.Contains(SessionArgs(p), "--ide") {
		t.Fatal("a detached session kept --ide")
	}
}

// TestWorkerEnvAllowlist: a worker inherits only the allowlisted variables;
// the launcher session's handles and settings env do not reach it, while an
// ordinary launch keeps its whole environment.
func TestWorkerEnvAllowlist(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	base := []string{
		"PATH=/usr/bin", "HOME=/h", "LC_ALL=C",
		"CLAUDE_CODE_MESSAGING_TOKEN=secret", "HERDR_SOCKET_PATH=/s", "GH_TOKEN=t", "OTEL_EXPORTER_OTLP_ENDPOINT=x",
	}
	build := func(worker bool) []string {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "plan"}},
			CWD:            target, Worker: worker, Resolve: bin, BaseEnv: base,
			InjectedEnv: map[string]string{"FORGECTL_INJECTED": "1"},
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		return built.Invocation.Env
	}
	env := build(true)
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/h", "LC_ALL=C", "FORGECTL_INJECTED=1"} {
		if !slices.Contains(env, kept) {
			t.Errorf("worker env lost %s: %q", kept, env)
		}
	}
	for _, e := range env {
		for _, dropped := range []string{"CLAUDE_CODE_MESSAGING_TOKEN=", "HERDR_SOCKET_PATH=", "GH_TOKEN=", "OTEL_EXPORTER_OTLP_ENDPOINT="} {
			if strings.HasPrefix(e, dropped) {
				t.Errorf("worker env kept %s", e)
			}
		}
	}
	if !slices.Contains(build(false), "CLAUDE_CODE_MESSAGING_TOKEN=secret") {
		t.Error("an ordinary launch lost its inherited environment")
	}
}

// TestWorkerEnvCarriesTheDrainWorkerMarker: every worker launch sets
// FORGECTL_DRAIN_WORKER=1, over an inherited value and a profile's env, and
// an ordinary launch does not add it.
func TestWorkerEnvCarriesTheDrainWorkerMarker(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	build := func(worker bool, base []string, injected map[string]string) []string {
		built, err := BuildInvocation(InvocationRequest{
			StdoutTerminal: true,
			Config:         config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "plan"}},
			CWD:            target, Worker: worker, Resolve: bin, BaseEnv: base, InjectedEnv: injected,
		})
		if err != nil {
			t.Fatalf("BuildInvocation: %v", err)
		}
		return built.Invocation.Env
	}
	if DrainWorkerEnv != "FORGECTL_DRAIN_WORKER" {
		t.Fatalf("DrainWorkerEnv is %q; intake and the docs name FORGECTL_DRAIN_WORKER", DrainWorkerEnv)
	}
	marker := DrainWorkerEnv + "=1"
	env := build(true, []string{"PATH=/usr/bin", DrainWorkerEnv + "=0"}, map[string]string{DrainWorkerEnv: ""})
	var seen []string
	for _, e := range env {
		if strings.HasPrefix(e, DrainWorkerEnv+"=") {
			seen = append(seen, e)
		}
	}
	if !slices.Equal(seen, []string{marker}) {
		t.Fatalf("worker env carries %q, want exactly %q: %q", seen, marker, env)
	}
	for _, e := range build(false, []string{"PATH=/usr/bin"}, nil) {
		if strings.HasPrefix(e, DrainWorkerEnv+"=") {
			t.Fatalf("an ordinary launch carries %s", e)
		}
	}
}
