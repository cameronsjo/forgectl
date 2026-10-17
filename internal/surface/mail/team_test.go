package mail

import (
	"errors"
	"reflect"
	"testing"
	"time"
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

func TestRegisterSameRunSupportsNoOpAndSessionEnrichment(test *testing.T) {
	roster := FileRoster{Dir: test.TempDir()}
	initialMember := Worker{
		Name:        "lead",
		Harness:     HarnessCodex,
		Worktree:    test.TempDir(),
		Coordinator: true,
		RunID:       "run-1",
		Socket:      "/tmp/isolated.sock",
		PaneID:      "pane-1",
	}
	registeredMember, err := roster.Register(initialMember, false)
	if err != nil {
		test.Fatal(err)
	}
	stateAt := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if err := roster.Update(initialMember.Name, func(currentMember *Worker) error {
		currentMember.State = StateIdle
		currentMember.StateAt = stateAt
		currentMember.Watchers = []string{"observer"}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	beforeEnrichment, err := roster.Get(initialMember.Name)
	if err != nil {
		test.Fatal(err)
	}

	noOpResult, err := roster.Register(initialMemberWithRunID(initialMember, registeredMember.RunID), true)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(noOpResult, beforeEnrichment) {
		test.Fatalf("same-run no-op result = %#v, want %#v", noOpResult, beforeEnrichment)
	}
	noOpStored, err := roster.Get(initialMember.Name)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(noOpStored, beforeEnrichment) {
		test.Fatalf("same-run no-op changed roster: %#v, want %#v", noOpStored, beforeEnrichment)
	}

	enrichmentRequest := initialMemberWithRunID(initialMember, registeredMember.RunID)
	enrichmentRequest.ThreadID = "thread-1"
	enrichedMember, err := roster.Register(enrichmentRequest, true)
	if err != nil {
		test.Fatal(err)
	}
	if enrichedMember.RunID != registeredMember.RunID || enrichedMember.ThreadID != enrichmentRequest.ThreadID || enrichedMember.State != beforeEnrichment.State || !enrichedMember.StateAt.Equal(beforeEnrichment.StateAt) || !reflect.DeepEqual(enrichedMember.Watchers, beforeEnrichment.Watchers) {
		test.Fatalf("session enrichment result = %#v, want run/session and preserved state %#v", enrichedMember, beforeEnrichment)
	}
	storedEnrichedMember, err := roster.Get(initialMember.Name)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(storedEnrichedMember, enrichedMember) {
		test.Fatalf("stored enrichment = %#v, returned %#v", storedEnrichedMember, enrichedMember)
	}
}

func TestRegisterSameRunRefusalsDoNotMutateRoster(test *testing.T) {
	roster := FileRoster{Dir: test.TempDir()}
	registeredMember, err := roster.Register(Worker{
		Name:        "lead",
		Harness:     HarnessCodex,
		Worktree:    test.TempDir(),
		Coordinator: true,
		RunID:       "run-1",
		Socket:      "/tmp/isolated.sock",
		PaneID:      "pane-1",
		ThreadID:    "thread-1",
	}, false)
	if err != nil {
		test.Fatal(err)
	}
	beforeRefusal, err := roster.List()
	if err != nil {
		test.Fatal(err)
	}
	otherWorktree := test.TempDir()
	refusalCases := []struct {
		name   string
		mutate func(*Worker)
	}{
		{name: "native-session-change", mutate: func(request *Worker) { request.ThreadID = "thread-2" }},
		{name: "native-session-clear", mutate: func(request *Worker) { request.ThreadID = "" }},
		{name: "worktree-change", mutate: func(request *Worker) { request.Worktree = otherWorktree }},
		{name: "pane-change", mutate: func(request *Worker) { request.PaneID = "pane-2" }},
		{name: "socket-change", mutate: func(request *Worker) { request.Socket = "/tmp/other.sock" }},
		{name: "harness-change", mutate: func(request *Worker) { request.Harness = HarnessPi }},
		{name: "coordinator-role-change", mutate: func(request *Worker) { request.Coordinator = false }},
	}
	assertRefusal := func(caseName string, refusalRequest Worker) {
		if _, err := roster.Register(refusalRequest, true); err == nil {
			test.Fatalf("refusal case %s was accepted: %#v", caseName, refusalRequest)
		}
		afterRefusal, listErr := roster.List()
		if listErr != nil {
			test.Fatal(listErr)
		}
		if !reflect.DeepEqual(afterRefusal, beforeRefusal) {
			test.Fatalf("refusal case %s mutated roster: %#v, want %#v", caseName, afterRefusal, beforeRefusal)
		}
	}
	for _, refusalCase := range refusalCases {
		refusalRequest := registeredMember
		refusalCase.mutate(&refusalRequest)
		assertRefusal(refusalCase.name, refusalRequest)
	}
	assertRefusal("duplicate-run-member", Worker{
		Name:     "other",
		Harness:  HarnessCodex,
		RunID:    registeredMember.RunID,
		Socket:   "/tmp/other.sock",
		ThreadID: "thread-other",
		Worktree: otherWorktree,
	})
}

func TestRegisterFreshRunAllowsIdentityReplacement(test *testing.T) {
	roster := FileRoster{Dir: test.TempDir()}
	initialMember := Worker{Name: "lead", Harness: HarnessCodex, Worktree: test.TempDir(), Coordinator: true, RunID: "run-1", Socket: "/tmp/isolated.sock", ThreadID: "thread-1"}
	if _, err := roster.Register(initialMember, false); err != nil {
		test.Fatal(err)
	}
	replacementMember := Worker{Name: initialMember.Name, Harness: HarnessCodex, Worktree: test.TempDir(), Coordinator: true, RunID: "run-2", Socket: "/tmp/replacement.sock", ThreadID: "thread-2"}
	replacedMember, err := roster.Register(replacementMember, true)
	if err != nil {
		test.Fatal(err)
	}
	if replacedMember.RunID != replacementMember.RunID || replacedMember.Worktree != replacementMember.Worktree || replacedMember.Socket != replacementMember.Socket || replacedMember.ThreadID != replacementMember.ThreadID {
		test.Fatalf("fresh-run replacement = %#v, want %#v", replacedMember, replacementMember)
	}
}

func initialMemberWithRunID(member Worker, runID string) Worker {
	member.RunID = runID
	return member
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
