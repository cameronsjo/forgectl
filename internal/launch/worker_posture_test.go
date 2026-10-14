package launch

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestResolveWorkerPosture is forgectl#1125: the display path reports the
// posture the worker launch would build, field by field with its source, and
// a floor refusal as Refused rather than an error.
func TestResolveWorkerPosture(t *testing.T) {
	target := projectDir(t)
	cases := map[string]struct {
		lc      config.LaunchConfig
		harness string
		want    []PostureValue
		refused string
	}{
		"built-in claude worker": {
			lc: config.LaunchConfig{}, harness: "claude",
			want: []PostureValue{{"permission_mode", "acceptEdits", PostureFromDefault}},
		},
		"a stricter repo block wins": {
			lc:      config.LaunchConfig{Projects: []config.LaunchProject{{Match: target, PermissionMode: "plan"}}},
			harness: "claude",
			want:    []PostureValue{{"permission_mode", "plan", PostureFromRepo}},
		},
		"codex from launch.worker": {
			lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"},
				Worker: config.LaunchWorker{Sandbox: "read-only"}},
			harness: "codex",
			want: []PostureValue{
				{"sandbox", "read-only", PostureFromWorker},
				{"approval_policy", "on-request", PostureFromDefault},
			},
		},
		"pi takes no posture field": {lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "pi"}}, harness: "pi"},
		"the floor refuses": {
			lc:      config.LaunchConfig{Worker: config.LaunchWorker{PermissionMode: "bypassPermissions"}},
			harness: "claude",
			want:    []PostureValue{{"permission_mode", "bypassPermissions", PostureFromWorker}},
			refused: "not allowed for a worker",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveWorkerPosture(c.lc, target)
			if err != nil {
				t.Fatal(err)
			}
			if got.Harness != c.harness || !reflect.DeepEqual(got.Values, c.want) {
				t.Errorf("posture %s %+v, want %s %+v", got.Harness, got.Values, c.harness, c.want)
			}
			if (c.refused == "") != (got.Refused == "") || !strings.Contains(got.Refused, c.refused) {
				t.Errorf("refused %q, want %q", got.Refused, c.refused)
			}
			// The display agrees with the launch about whether it may start.
			_, buildErr := BuildInvocation(InvocationRequest{Config: c.lc, CWD: target, Worker: true,
				Resolve: fixedResolver(ResolvedBinary{Path: "/stub/harness", Source: BinaryPATH}), StdoutTerminal: true})
			if (buildErr != nil) != (got.Refused != "") || (buildErr != nil && !errors.Is(buildErr, ErrWorkerPosture)) {
				t.Errorf("launch error %v, display refused %q", buildErr, got.Refused)
			}
		})
	}
}

// TestDefaultWorkerPosture matches no project block, as `forgectl config`
// shows no directory's profile.
func TestDefaultWorkerPosture(t *testing.T) {
	lc := config.LaunchConfig{Projects: []config.LaunchProject{{Match: "/", PermissionMode: "plan"}}}
	got := DefaultWorkerPosture(lc)
	want := []PostureValue{{"permission_mode", "acceptEdits", PostureFromDefault}}
	if got.Harness != "claude" || !reflect.DeepEqual(got.Values, want) || got.Refused != "" {
		t.Errorf("default worker posture %+v", got)
	}
}
