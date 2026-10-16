package mail

import (
	"errors"
	"testing"
)

func TestRegisterTeamRebindsExplicitly(t *testing.T) {
	roster := FileRoster{Dir: t.TempDir()}
	member := Worker{Name: "lead", Harness: HarnessCodex, Worktree: t.TempDir(), Coordinator: true, Socket: "/tmp/isolated.sock", ThreadID: "thread-1"}
	first, err := roster.Register(member, false)
	if err != nil || first.RunID == "" {
		t.Fatalf("first registration = %#v, %v", first, err)
	}
	if _, err := roster.Register(member, false); err == nil {
		t.Fatal("duplicate registration did not require explicit replacement")
	}
	second := member
	second.Name = "other-lead"
	if _, err := roster.Register(second, false); err == nil {
		t.Fatal("registered a second coordinator")
	}
	member.ThreadID = "thread-2"
	rebound, err := roster.Register(member, true)
	if err != nil || rebound.RunID == first.RunID || rebound.ThreadID != "thread-2" {
		t.Fatalf("rebound = %#v, %v", rebound, err)
	}
	member.Coordinator = false
	if _, err := roster.Register(member, true); err == nil {
		t.Fatal("replacement changed the coordinator role")
	}
	stored, err := roster.Get("lead")
	if err != nil || stored.RunID != rebound.RunID || !stored.Coordinator {
		t.Fatalf("refused registration changed stored identity: %#v, %v", stored, err)
	}
}

func TestExplicitTeamIdentity(t *testing.T) {
	roster := FileRoster{Dir: t.TempDir()}
	coordinator, err := roster.Register(Worker{Name: "lead", Harness: HarnessCodex, Worktree: t.TempDir(), Coordinator: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := roster.Register(Worker{Name: "builder", Harness: HarnessPi, Worktree: t.TempDir(), Socket: "/tmp/pi.sock"}, false)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"coordinator", map[string]string{EnvCoordinator: "lead", EnvLedger: roster.Dir, EnvRunID: coordinator.RunID}, "lead"},
		{"worker", map[string]string{EnvWorker: "builder", EnvLedger: roster.Dir, EnvRunID: worker.RunID}, "builder"},
		{"missing-ledger", map[string]string{EnvCoordinator: "lead", EnvRunID: coordinator.RunID}, ""},
		{"relative-ledger", map[string]string{EnvCoordinator: "lead", EnvLedger: "relative", EnvRunID: coordinator.RunID}, ""},
		{"both-roles", map[string]string{EnvCoordinator: "lead", EnvWorker: "builder", EnvLedger: roster.Dir, EnvRunID: coordinator.RunID}, ""},
		{"drain-role", map[string]string{EnvCoordinator: "lead", EnvDrainWorker: "builder", EnvLedger: roster.Dir, EnvRunID: coordinator.RunID}, ""},
		{"worker-as-lead", map[string]string{EnvCoordinator: "builder", EnvLedger: roster.Dir, EnvRunID: worker.RunID}, ""},
		{"retired-coordinator", map[string]string{EnvCoordinator: "lead", EnvLedger: roster.Dir, EnvRunID: "old-run"}, ""},
		{"retired-worker", map[string]string{EnvWorker: "builder", EnvLedger: roster.Dir, EnvRunID: "old-run"}, ""},
		{"missing-run", map[string]string{EnvCoordinator: "lead", EnvLedger: roster.Dir}, ""},
	}
	for _, example := range cases {
		t.Run(example.name, func(t *testing.T) {
			name, err := SelfName(roster, func(key string) string { return example.values[key] })
			if example.want == "" {
				if !errors.Is(err, ErrNoSelf) {
					t.Fatalf("identity unexpectedly accepted: %q, %v", name, err)
				}
			} else if err != nil || name != example.want {
				t.Fatalf("identity = %q, %v; want %q", name, err, example.want)
			}
		})
	}
}

func TestRegisterTeamRejectsBadEndpoints(t *testing.T) {
	for _, member := range []Worker{
		{Name: "bad", Harness: HarnessPi, Worktree: "/tmp"},
		{Name: "bad", Harness: HarnessPi, Worktree: "/tmp", Socket: "relative"},
		{Name: "bad", Harness: HarnessCodex, Worktree: "/tmp", ThreadID: "--flag"},
		{Name: "bad", Harness: HarnessCodex, Worktree: "/tmp", Socket: "/tmp/inbox.sock?other"},
		{Name: "bad", Harness: HarnessCodex, Worktree: "/tmp", Socket: "/tmp/inbox%2fsock"},
		{Name: "bad", Harness: HarnessCodex, Worktree: "/tmp", RunID: "bad\nrun"},
		{Name: "bad", Harness: HarnessPane, Worktree: "/tmp"},
		{Name: "bad", Harness: HarnessClaude, Worktree: "relative"},
	} {
		roster := FileRoster{Dir: t.TempDir()}
		if _, err := roster.Register(member, false); err == nil {
			t.Fatalf("accepted bad member: %#v", member)
		}
		members, err := roster.List()
		if err != nil || len(members) != 0 {
			t.Fatalf("invalid member mutated roster: %#v, %v", members, err)
		}
	}
}
