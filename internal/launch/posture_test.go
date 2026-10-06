package launch

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestWorkerFloorWalksEveryKnownValue: the floor accepts exactly the values
// at or below each cap. Ranking dontAsk (or any looser mode) at or below the
// cap, or raising the cap, turns this red (ADR-0010 item 4).
func TestWorkerFloorWalksEveryKnownValue(t *testing.T) {
	wantClaude := map[string]bool{"plan": true, "default": true, "manual": true, "acceptEdits": true}
	for _, mode := range claudePermissionRank.known() {
		_, err := applyWorkerFloor(Profile{Harness: "claude", PermissionMode: mode})
		if got := err == nil; got != wantClaude[mode] {
			t.Errorf("claude worker in %s: allowed=%v, want %v (%v)", mode, got, wantClaude[mode], err)
		}
	}
	wantSandbox := map[string]bool{"read-only": true, "workspace-write": true}
	wantApproval := map[string]bool{"untrusted": true, "on-request": true}
	for _, sb := range codexSandboxRank.known() {
		for _, ap := range codexApprovalRank.known() {
			_, err := applyWorkerFloor(Profile{Harness: "codex", Sandbox: sb, ApprovalPolicy: ap})
			if want := wantSandbox[sb] && wantApproval[ap]; (err == nil) != want {
				t.Errorf("codex worker %s/%s: allowed=%v, want %v (%v)", sb, ap, err == nil, want, err)
			}
		}
	}
	for _, p := range []Profile{
		{Harness: "claude", PermissionMode: ""},
		{Harness: "claude", PermissionMode: "acceptEdit"},
		{Harness: "codex", Sandbox: "", ApprovalPolicy: "on-request"},
	} {
		if _, err := applyWorkerFloor(p); !errors.Is(err, ErrWorkerPosture) {
			t.Errorf("worker floor accepted an unranked posture %+v: %v", p, err)
		}
	}
}

func TestValidateRefusesUnknownPermissionMode(t *testing.T) {
	if err := (Profile{Harness: "claude", PermissionMode: "acceptEdit"}).Validate(); err == nil || !strings.Contains(err.Error(), "permission_mode") {
		t.Fatalf("a typo passed Validate: %v", err)
	}
	for _, mode := range append(claudePermissionRank.known(), "") {
		if err := (Profile{Harness: "claude", PermissionMode: mode}).Validate(); err != nil {
			t.Errorf("Validate refused %q: %v", mode, err)
		}
	}
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	_, err := BuildInvocation(InvocationRequest{Config: config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "acceptEdit"}}, CWD: target, Resolve: bin})
	if err == nil {
		t.Fatal("a launch with an unknown permission_mode was built")
	}
}

// TestWorkerProfileMerge pins the worker posture rule: the worker profile
// (or the built-in worker value), made stricter by the matched project
// block's own value; [launch.defaults] does not bind a worker.
func TestWorkerProfileMerge(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	build := func(lc config.LaunchConfig) (Profile, error) {
		built, err := BuildInvocation(InvocationRequest{Config: lc, CWD: target, Worker: true, Resolve: bin, StdoutTerminal: true})
		return built.Profile, err
	}
	cases := map[string]struct {
		lc                      config.LaunchConfig
		mode, sandbox, approval string
	}{
		"built-in claude worker": {lc: config.LaunchConfig{}, mode: "acceptEdits"},
		"defaults do not bind":   {lc: config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "plan"}}, mode: "acceptEdits"},
		"a stricter repo block wins": {
			lc: config.LaunchConfig{Projects: []config.LaunchProject{{Match: target, PermissionMode: "plan"}}}, mode: "plan"},
		"a looser repo block cannot loosen": {
			lc: config.LaunchConfig{Projects: []config.LaunchProject{{Match: target, PermissionMode: "bypassPermissions"}}}, mode: "acceptEdits"},
		"a stricter worker profile wins": {lc: config.LaunchConfig{Worker: config.LaunchWorker{PermissionMode: "default"}}, mode: "default"},
		"a codex field in a claude block is ignored": {
			lc: config.LaunchConfig{Projects: []config.LaunchProject{{Match: target, Sandbox: "yolo"}}}, mode: "acceptEdits"},
		"built-in codex worker": {
			lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"}}, sandbox: "workspace-write", approval: "on-request"},
		"a stricter codex repo block wins": {
			lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"},
				Projects: []config.LaunchProject{{Match: target, Sandbox: "read-only"}}}, sandbox: "read-only", approval: "on-request"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := build(c.lc)
			if err != nil {
				t.Fatalf("BuildInvocation: %v", err)
			}
			if c.mode != "" && p.PermissionMode != c.mode {
				t.Errorf("permission_mode %q, want %q", p.PermissionMode, c.mode)
			}
			if c.sandbox != "" && (p.Sandbox != c.sandbox || p.ApprovalPolicy != c.approval) {
				t.Errorf("codex posture %s/%s, want %s/%s", p.Sandbox, p.ApprovalPolicy, c.sandbox, c.approval)
			}
		})
	}

	t.Run("an unranked repo value refuses the worker", func(t *testing.T) {
		_, err := build(config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"},
			Projects: []config.LaunchProject{{Match: target, ApprovalPolicy: "sometimes"}}})
		if err == nil {
			t.Fatal("an unranked approval_policy in the repo block was accepted")
		}
	})
}

func TestStricterFailsClosed(t *testing.T) {
	if got, err := claudePermissionRank.stricter("acceptEdits", "plan"); err != nil || got != "plan" {
		t.Fatalf("stricter(acceptEdits, plan) = %q, %v", got, err)
	}
	for _, pair := range [][2]string{{"yolo", "plan"}, {"plan", "yolo"}, {"", "plan"}} {
		if got, err := claudePermissionRank.stricter(pair[0], pair[1]); err == nil || got != "" {
			t.Errorf("stricter(%q, %q) = %q, %v: want \"\" and an error", pair[0], pair[1], got, err)
		}
	}
}

func TestWorkerDefaultsWithinCaps(t *testing.T) {
	for _, c := range []struct {
		r          postureRank
		def, limit string
	}{
		{claudePermissionRank, workerDefaultPermissionMode, workerMaxPermissionMode},
		{codexSandboxRank, workerDefaultSandbox, workerMaxSandbox},
		{codexApprovalRank, workerDefaultApproval, workerMaxApproval},
	} {
		if !c.r.atMost(c.def, c.limit) {
			t.Errorf("built-in worker %s %q is looser than the cap %q", c.r.field, c.def, c.limit)
		}
	}
}

func TestWorkerProfileChecksAndNotes(t *testing.T) {
	target := projectDir(t)
	bin := fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH})
	build := func(lc config.LaunchConfig, harness string) (BuiltInvocation, error) {
		return BuildInvocation(InvocationRequest{Config: lc, CWD: target, Worker: true, Resolve: bin, StdoutTerminal: true, Harness: harness})
	}

	t.Run("a [launch.worker] typo is refused whatever the harness", func(t *testing.T) {
		if _, err := build(config.LaunchConfig{Worker: config.LaunchWorker{Sandbox: "workspace-wirte"}}, "claude"); err == nil {
			t.Fatal("a claude worker accepted a mistyped [launch.worker] sandbox")
		}
	})

	t.Run("an explicit stricter default is named in a note", func(t *testing.T) {
		built, err := build(config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "plan"}}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(built.Notes) != 1 || !strings.Contains(built.Notes[0], `"plan"`) || !strings.Contains(built.Notes[0], "[launch.worker]") {
			t.Fatalf("notes %q", built.Notes)
		}
		built, err = build(config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "plan"}, Worker: config.LaunchWorker{PermissionMode: "plan"}}, "")
		if err != nil || len(built.Notes) != 0 {
			t.Fatalf("a worker that keeps plan still got notes %q, %v", built.Notes, err)
		}
	})

	t.Run("--harness codex reads a claude block's own codex fields", func(t *testing.T) {
		built, err := build(config.LaunchConfig{Projects: []config.LaunchProject{{Match: target, Harness: "claude", Sandbox: "read-only"}}}, "codex")
		if err != nil {
			t.Fatal(err)
		}
		if p := built.Profile; p.Sandbox != "read-only" || p.ApprovalPolicy != "on-request" {
			t.Fatalf("codex posture %s/%s, want read-only/on-request", p.Sandbox, p.ApprovalPolicy)
		}
	})

	t.Run("--harness claude reads a codex block's own permission_mode", func(t *testing.T) {
		built, err := build(config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"},
			Projects: []config.LaunchProject{{Match: target, Harness: "codex", PermissionMode: "plan"}}}, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if built.Profile.PermissionMode != "plan" {
			t.Fatalf("permission_mode %q, want plan", built.Profile.PermissionMode)
		}
	})
}
