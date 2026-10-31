package launch

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestWorkerFollowsLauncherMode pins --inherit-mode: the launcher's mode is a
// ceiling over every source and never raises a worker. The flag is the
// caller's unverifiable claim, so claiming auto must not lift the built-in
// acceptEdits; [launch.worker] is the only way to auto workers.
func TestWorkerFollowsLauncherMode(t *testing.T) {
	cases := []struct {
		name     string
		launcher string
		worker   string // [launch.worker] permission_mode
		repo     string // matched project block's permission_mode
		want     string
		source   string
	}{
		{name: "no launcher mode keeps the built-in value", want: "acceptEdits", source: PostureFromDefault},
		{name: "auto launcher does not raise the built-in value", launcher: "auto", want: "acceptEdits", source: PostureFromDefault},
		{name: "auto launcher, worker set to auto: auto", launcher: "auto", worker: "auto", want: "auto", source: PostureFromWorker},
		{name: "plan launcher, nothing set: plan", launcher: "plan", want: "plan", source: PostureFromLauncher},
		{name: "default launcher, nothing set: default", launcher: "default", want: "default", source: PostureFromLauncher},
		{name: "bypass launcher raises nothing", launcher: "bypassPermissions", want: "acceptEdits", source: PostureFromDefault},
		{name: "bypass launcher, worker auto: capped at auto", launcher: "bypassPermissions", worker: "auto", want: "auto", source: PostureFromWorker},
		{name: "explicit worker value overrides an auto launcher", launcher: "auto", worker: "acceptEdits", want: "acceptEdits", source: PostureFromWorker},
		{name: "plan launcher caps an explicit auto worker", launcher: "plan", worker: "auto", want: "plan", source: PostureFromLauncher},
		{name: "a stricter repo block still wins", launcher: "auto", repo: "plan", want: "plan", source: PostureFromRepo},
		{name: "the launcher caps a looser repo block", launcher: "default", repo: "auto", want: "default", source: PostureFromLauncher},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := config.LaunchConfig{Worker: config.LaunchWorker{PermissionMode: tc.worker}}
			var proj *config.LaunchProject
			if tc.repo != "" {
				proj = &config.LaunchProject{PermissionMode: tc.repo}
			}
			p, values, _, err := applyWorkerProfile(Profile{Harness: "claude"}, lc, proj, tc.launcher)
			if err != nil {
				t.Fatal(err)
			}
			if p.PermissionMode != tc.want {
				t.Errorf("permission_mode = %q, want %q", p.PermissionMode, tc.want)
			}
			if len(values) != 1 || values[0].Source != tc.source || values[0].Value != tc.want {
				t.Errorf("posture values = %+v, want %s from %s", values, tc.want, tc.source)
			}
			// Whatever the inputs, the result passes the worker floor.
			if _, err := applyWorkerFloor(p); err != nil {
				t.Errorf("worker floor refused %q: %v", p.PermissionMode, err)
			}
		})
	}
}

func TestLauncherModeIgnoredForCodex(t *testing.T) {
	p, _, _, err := applyWorkerProfile(Profile{Harness: "codex"}, config.LaunchConfig{}, nil, "bypassPermissions")
	if err != nil {
		t.Fatal(err)
	}
	if p.Sandbox != workerDefaultSandbox || p.ApprovalPolicy != workerDefaultApproval {
		t.Fatalf("codex posture = %q/%q, want the built-in worker values", p.Sandbox, p.ApprovalPolicy)
	}
}

func TestCheckLauncherMode(t *testing.T) {
	if err := CheckLauncherMode("auto"); err != nil {
		t.Errorf("auto: %v", err)
	}
	if err := CheckLauncherMode("yolo"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
