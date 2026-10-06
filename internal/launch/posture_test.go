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

func TestStricterPosture(t *testing.T) {
	repo := Profile{Harness: "claude", Model: "opus", PermissionMode: "plan", Sandbox: "workspace-write", ApprovalPolicy: "never", AllowDanger: true}
	floor := Profile{PermissionMode: "acceptEdits", Sandbox: "read-only", ApprovalPolicy: "on-request", AllowDanger: false}
	got, err := StricterPosture(repo, floor)
	if err != nil {
		t.Fatal(err)
	}
	want := Profile{Harness: "claude", Model: "opus", PermissionMode: "plan", Sandbox: "read-only", ApprovalPolicy: "on-request", AllowDanger: false}
	if got.Harness != want.Harness || got.Model != want.Model || got.PermissionMode != want.PermissionMode ||
		got.Sandbox != want.Sandbox || got.ApprovalPolicy != want.ApprovalPolicy || got.AllowDanger != want.AllowDanger {
		t.Fatalf("StricterPosture = %+v, want %+v", got, want)
	}
	// Equal rank keeps a's spelling.
	if got, _ := StricterPosture(Profile{PermissionMode: "manual", Sandbox: "read-only", ApprovalPolicy: "never"}, Profile{PermissionMode: "default", Sandbox: "read-only", ApprovalPolicy: "never"}); got.PermissionMode != "manual" {
		t.Errorf("equal rank picked %q, want a's manual", got.PermissionMode)
	}
	// An unknown value never wins, and is reported.
	got, err = StricterPosture(Profile{PermissionMode: "yolo", Sandbox: "read-only", ApprovalPolicy: "never"}, floor)
	if err == nil {
		t.Fatalf("an unknown mode merged silently into %+v", got)
	}
}
