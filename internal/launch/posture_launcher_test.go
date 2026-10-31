package launch

import (
	"errors"
	"strings"
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
	p, _, notes, err := applyWorkerProfile(Profile{Harness: "codex"}, config.LaunchConfig{}, nil, "plan")
	if err != nil {
		t.Fatal(err)
	}
	if p.Sandbox != workerDefaultSandbox || p.ApprovalPolicy != workerDefaultApproval {
		t.Fatalf("codex posture = %q/%q, want the built-in worker values", p.Sandbox, p.ApprovalPolicy)
	}
	// Ignored, but never silently: a plan launcher is told its ceiling did
	// not reach a codex worker.
	if len(notes) != 1 || !strings.Contains(notes[0], "--inherit-mode applies to claude workers only") {
		t.Fatalf("notes = %q, want the --inherit-mode note", notes)
	}
}

// TestInheritLauncherModeOptIn pins [launch.worker] inherit_launcher_mode:
// with it the launcher's mode replaces the built-in value, capped at auto,
// and every other source still only tightens.
func TestInheritLauncherModeOptIn(t *testing.T) {
	cases := []struct {
		name, launcher, repo, want, source string
	}{
		{name: "auto launcher raises to auto", launcher: "auto", want: "auto", source: PostureFromLauncher},
		{name: "bypass launcher is capped at auto", launcher: "bypassPermissions", want: "auto", source: PostureFromLauncher},
		{name: "dontAsk launcher is capped at auto", launcher: "dontAsk", want: "auto", source: PostureFromLauncher},
		{name: "plan launcher gives plan", launcher: "plan", want: "plan", source: PostureFromLauncher},
		{name: "no launcher mode keeps the built-in value", want: "acceptEdits", source: PostureFromDefault},
		{name: "a stricter repo block still wins", launcher: "auto", repo: "default", want: "default", source: PostureFromRepo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := config.LaunchConfig{Worker: config.LaunchWorker{InheritLauncherMode: true}}
			var proj *config.LaunchProject
			if tc.repo != "" {
				proj = &config.LaunchProject{PermissionMode: tc.repo}
			}
			p, values, _, err := applyWorkerProfile(Profile{Harness: "claude"}, lc, proj, tc.launcher)
			if err != nil {
				t.Fatal(err)
			}
			if p.PermissionMode != tc.want || len(values) != 1 || values[0].Source != tc.source {
				t.Fatalf("got %q from %+v, want %q from %s", p.PermissionMode, values, tc.want, tc.source)
			}
			if _, err := applyWorkerFloor(p); err != nil {
				t.Errorf("worker floor refused %q: %v", p.PermissionMode, err)
			}
		})
	}
}

// TestInheritLauncherModeNeverPastAuto sweeps every launcher mode under the
// opt-in: the result is never looser than auto.
func TestInheritLauncherModeNeverPastAuto(t *testing.T) {
	lc := config.LaunchConfig{Worker: config.LaunchWorker{InheritLauncherMode: true}}
	for _, launcher := range claudePermissionRank.known() {
		p, _, _, err := applyWorkerProfile(Profile{Harness: "claude"}, lc, nil, launcher)
		if err != nil {
			t.Fatalf("%s: %v", launcher, err)
		}
		if !claudePermissionRank.atMost(p.PermissionMode, launcherModeCap) {
			t.Errorf("launcher %q under the opt-in gave %q, looser than auto", launcher, p.PermissionMode)
		}
	}
}

func TestInheritLauncherModeRefusesAnExplicitMode(t *testing.T) {
	lc := config.LaunchConfig{Worker: config.LaunchWorker{InheritLauncherMode: true, PermissionMode: "auto"}}
	_, _, _, err := applyWorkerProfile(Profile{Harness: "claude"}, lc, nil, "auto")
	if !errors.Is(err, ErrInheritConflict) {
		t.Fatalf("err = %v, want ErrInheritConflict", err)
	}
	// The refusal names the fix, both ways.
	for _, want := range []string{"remove permission_mode", "inherit_launcher_mode = false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	// launch which and forgectl config stay readable: the preview shows what
	// permission_mode gives and names the conflict.
	wp := workerPosture(Profile{Harness: "claude"}, lc, nil)
	if !strings.Contains(wp.Refused, "remove permission_mode") || len(wp.Values) != 1 || wp.Values[0].Value != "auto" {
		t.Fatalf("preview = %+v, want the auto value from permission_mode and the conflict in Refused", wp)
	}
}

func TestInheritLauncherModeWithoutTheFlagNotes(t *testing.T) {
	lc := config.LaunchConfig{Worker: config.LaunchWorker{InheritLauncherMode: true}}
	p, _, notes, err := applyWorkerProfile(Profile{Harness: "claude"}, lc, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.PermissionMode != workerDefaultPermissionMode {
		t.Fatalf("mode = %q, want the built-in value", p.PermissionMode)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no --inherit-mode was given") {
		t.Fatalf("notes = %q, want the missing-flag note", notes)
	}
	// With the flag there is no such note.
	if _, _, notes, _ := applyWorkerProfile(Profile{Harness: "claude"}, lc, nil, "auto"); len(notes) != 0 {
		t.Errorf("notes with the flag = %q, want none", notes)
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

// TestLauncherModeNeverRaises sweeps every ranked launcher mode against every
// [launch.worker] value and every project value, unset included: the result
// with a launcher mode is never looser than the result without one, so no
// future source branch can let the caller's claim raise a worker.
func TestLauncherModeNeverRaises(t *testing.T) {
	values := append([]string{""}, claudePermissionRank.known()...)
	for _, launcher := range claudePermissionRank.known() {
		for _, wv := range values {
			for _, pv := range values {
				lc := config.LaunchConfig{Worker: config.LaunchWorker{PermissionMode: wv}}
				var proj *config.LaunchProject
				if pv != "" {
					proj = &config.LaunchProject{PermissionMode: pv}
				}
				base, _, _, baseErr := applyWorkerProfile(Profile{Harness: "claude"}, lc, proj, "")
				got, _, _, err := applyWorkerProfile(Profile{Harness: "claude"}, lc, proj, launcher)
				if baseErr != nil {
					if err == nil {
						t.Errorf("launcher %q, worker %q, project %q: refused without a launcher mode, accepted with one", launcher, wv, pv)
					}
					continue
				}
				if err != nil {
					t.Errorf("launcher %q, worker %q, project %q: %v", launcher, wv, pv, err)
					continue
				}
				if !claudePermissionRank.atMost(got.PermissionMode, base.PermissionMode) {
					t.Errorf("launcher %q, worker %q, project %q: %q is looser than %q without it",
						launcher, wv, pv, got.PermissionMode, base.PermissionMode)
				}
			}
		}
	}
}
