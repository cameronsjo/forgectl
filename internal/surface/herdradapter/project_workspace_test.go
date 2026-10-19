//go:build unix

package herdradapter

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func projectWorkspaceCreateJSON(workspace, tab, pane, terminal string) []byte {
	return mustJSON(map[string]any{"id": "workspace:create", "result": map[string]any{
		"type":      "workspace_created",
		"workspace": map[string]any{"workspace_id": workspace},
		"tab":       map[string]any{"tab_id": tab, "workspace_id": workspace},
		"root_pane": map[string]any{"pane_id": pane, "tab_id": tab, "workspace_id": workspace, "terminal_id": terminal},
	}})
}

func configureProjectWorkspaceRunner(run *scriptedRunner, marker, paneTerminal string) {
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrCreate {
			return stdout(projectWorkspaceCreateJSON(wsA, "w2R:t1", "w2R:p1", "term_1")), nil
		}
		if cmd.Kind == exec.KindHerdrProjectTabProbe {
			if hasSubcommand(cmd, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, marker)), nil
			}
			if hasSubcommand(cmd, "tab", "list") {
				return stdout(projectTabsJSON(map[string]any{
					"tab_id": "w2R:t1", "workspace_id": wsA, "label": "root", "pane_count": 1,
				})), nil
			}
		}
		if cmd.Kind == exec.KindHerdrPaneStatus {
			return stdout(projectPaneJSON(wsA, "w2R:t1", "w2R:p1", paneTerminal)), nil
		}
		return base(cmd)
	}
}

func TestStartProjectWorkspaceCallsPendingHookBeforeCreate(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(t)
	order := make([]string, 0, 2)
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "term_1")
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrCreate {
			order = append(order, "create")
		}
		return base(cmd)
	}
	var callbackServer backend.ServerID
	var callbackEndpoint backend.EndpointID
	result := a.StartProjectWorkspace(context.Background(), spec, func(server backend.ServerID, endpoint backend.EndpointID) error {
		order = append(order, "callback")
		callbackServer = server
		callbackEndpoint = endpoint
		return nil
	})
	if result.Outcome() != backend.RefKnown {
		t.Logf("calls=%v", run.calls())
		if cause, ok := result.Cause(); ok {
			t.Logf("cause=%v", cause)
		}
		t.Fatalf("StartProjectWorkspace = %v, want RefKnown", result)
	}
	if len(order) != 2 || order[0] != "callback" || order[1] != "create" {
		t.Fatalf("create ordering = %v, want callback then create", order)
	}
	if !callbackServer.Valid() || !callbackEndpoint.Matches(backend.EndpointOf(testSocket)) {
		t.Fatalf("callback identity = %s/%s, want valid server and %s", callbackServer, callbackEndpoint, backend.EndpointOf(testSocket))
	}
	ref, ok := result.Ref()
	if !ok {
		t.Fatal("StartProjectWorkspace returned no reference")
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		t.Fatalf("HerdrIdentity: %v", err)
	}
	if identity.Terminal() != "term_1" {
		t.Fatalf("root terminal = %q, want term_1", identity.Terminal())
	}
}

func TestStartProjectWorkspaceRejectsCallbackFailureBeforeCreate(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(t)
	want := errors.New("journal unavailable")
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error {
		return want
	})
	if result.Outcome() != backend.NotMutated {
		t.Fatalf("StartProjectWorkspace = %v, want NotMutated", result)
	}
	if cause, ok := result.Cause(); !ok || !errors.Is(cause, want) {
		t.Fatalf("callback cause = %v, want %v", cause, want)
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrCreate); found {
		t.Fatal("workspace create ran after callback failure")
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrBootstrap); found {
		t.Fatal("bootstrap ran after callback failure")
	}
}

func TestStartProjectWorkspaceRequiresNamedSession(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)
	called := false
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error {
		called = true
		return nil
	})
	if result.Outcome() != backend.NotMutated {
		t.Fatalf("StartProjectWorkspace = %v, want NotMutated", result)
	}
	if called {
		t.Fatal("callback ran for the default, unnamed herdr session")
	}
	if len(run.calls()) != 0 {
		t.Fatal("unnamed project workspace attempted a herdr command")
	}
}

func TestStartProjectWorkspaceRejectsTurnoverAfterPendingHook(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	oldSocket := liveSocket(liveInode)
	newSocket := liveSocket(liveInode + 1)
	callbackDone := false
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"}, WithLstat(func(path string) (os.FileInfo, error) {
		if callbackDone {
			return newSocket(path)
		}
		return oldSocket(path)
	}))
	spec, _ := newSpec(t)
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error {
		callbackDone = true
		return nil
	})
	if result.Outcome() != backend.NotMutated {
		t.Fatalf("StartProjectWorkspace = %v, want NotMutated after turnover", result)
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrSnapshot); found {
		t.Fatal("workspace snapshot ran after callback turnover")
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrCreate); found {
		t.Fatal("workspace create ran after callback turnover")
	}
}

func TestStartProjectWorkspaceRefusesWrongRootTerminal(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(t)
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "wrong-terminal")
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	if _, ok := result.Ref(); !ok || result.Outcome() != backend.RefKnown {
		t.Fatalf("StartProjectWorkspace = %v, want RefKnown with cause", result)
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrBootstrap); found {
		t.Fatal("bootstrap ran for a root pane with the wrong terminal")
	}
}

func TestStartProjectWorkspaceAcceptsCompositeNavigationLabel(test *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	adapter := newTestAdapter(test, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(test)
	label, err := projectLabelFor("Codex + Hermes", spec.OwnershipName())
	if err != nil {
		test.Fatal(err)
	}
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "term_1")
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrProjectTabProbe && hasSubcommand(cmd, "workspace", "list") {
			return stdout(projectWorkspaceJSON(wsA, label)), nil
		}
		return base(cmd)
	}
	result := adapter.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	if result.Outcome() != backend.RefKnown || result.Failed() {
		test.Fatalf("StartProjectWorkspace = %v, want RefKnown", result)
	}
}

func TestStartProjectWorkspaceRejectsMalformedCompositeLabel(test *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	adapter := newTestAdapter(test, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(test)
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "term_1")
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrProjectTabProbe && hasSubcommand(cmd, "workspace", "list") {
			return stdout(projectWorkspaceJSON(wsA, "Codex + Hermes · "+spec.OwnershipName()+"-forged")), nil
		}
		return base(cmd)
	}
	result := adapter.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	if result.Outcome() != backend.RefKnown || !result.Failed() {
		test.Fatalf("StartProjectWorkspace = %v, want RefKnown-with-cause", result)
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrBootstrap); found {
		test.Fatal("bootstrap ran for malformed composite label")
	}
}

func TestFocusProjectWorkspaceRefusesWrongTerminalWithoutMutation(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(t)
	wrongTerminal := false
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrCreate {
			return stdout(projectWorkspaceCreateJSON(wsA, "w2R:t1", "w2R:p1", "term_1")), nil
		}
		if cmd.Kind == exec.KindHerdrProjectTabProbe {
			if hasSubcommand(cmd, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, spec.OwnershipName())), nil
			}
			if hasSubcommand(cmd, "tab", "list") {
				return stdout(projectTabsJSON(map[string]any{"tab_id": "w2R:t1", "workspace_id": wsA, "label": "root", "pane_count": 1})), nil
			}
		}
		if cmd.Kind == exec.KindHerdrPaneStatus {
			terminal := "term_1"
			if wrongTerminal {
				terminal = "term_wrong"
			}
			return stdout(projectPaneJSON(wsA, "w2R:t1", "w2R:p1", terminal)), nil
		}
		return base(cmd)
	}
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	ref, ok := result.Ref()
	if !ok || result.Outcome() != backend.RefKnown {
		t.Fatalf("StartProjectWorkspace = %v, want RefKnown", result)
	}
	before := len(run.calls())
	wrongTerminal = true
	if err := a.FocusProjectWorkspace(context.Background(), ref); err == nil {
		t.Fatal("FocusProjectWorkspace accepted a changed root terminal")
	}
	for _, call := range run.calls()[before:] {
		if call.Kind == exec.KindHerdrProjectTabFocus {
			t.Fatal("tab focus ran after root terminal mismatch")
		}
	}
}

func TestProjectWorkspaceReconciliationUsesOutcomeUnknownAfterCreateAttempt(t *testing.T) {
	tests := map[string]struct {
		reconcile []byte
	}{
		"zero matches": {
			reconcile: listJSON(),
		},
		"multiple matches": {
			reconcile: func() []byte {
				return listJSON([2]string{wsA, "marker"}, [2]string{wsB, "marker"})
			}(),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			run := newRunner().
				reply1(exec.KindHerdrCreate, []byte("not a project create reply")).
				reply1(exec.KindHerdrProjectTabReconcile, tc.reconcile)
			run.sessions = func() (exec.SensitiveResult, error) {
				return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
			}
			a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
			spec, tag := newSpec(t)
			if name == "multiple matches" {
				run.reply[exec.KindHerdrProjectTabReconcile] = func() (exec.SensitiveResult, error) {
					return stdout(listJSON([2]string{wsA, tag.OwnershipName()}, [2]string{wsB, tag.OwnershipName()})), nil
				}
			}
			result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
			if result.Outcome() != backend.OutcomeUnknown {
				t.Fatalf("StartProjectWorkspace = %v, want OutcomeUnknown", result)
			}
			if _, ok := result.Ref(); ok {
				t.Fatal("ambiguous project workspace create returned a reference")
			}
		})
	}
}

func TestProjectWorkspaceReconciliationDoesNotTreatGoneServerAsNotMutated(t *testing.T) {
	gone := false
	run := newRunner().on(exec.KindHerdrProjectTabReconcile, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrProjectTabReconcile, exec.OutcomeExit)
	})
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrCreate {
			gone = true
			return stdout([]byte("not a project create reply")), nil
		}
		return base(cmd)
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"}, WithLstat(func(path string) (os.FileInfo, error) {
		if gone {
			return absentSocket(path)
		}
		return liveSocket(liveInode)(path)
	}))
	spec, _ := newSpec(t)
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	if result.Outcome() != backend.OutcomeUnknown {
		t.Fatalf("StartProjectWorkspace = %v, want OutcomeUnknown after server disappearance", result)
	}
}

func TestStartProjectWorkspaceRejectsDuplicateOwnershipMarker(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(t)
	firstLabel, err := projectLabelFor("Codex", spec.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	secondLabel, err := projectLabelFor("Hermes", spec.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "term_1")
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrProjectTabProbe && hasSubcommand(cmd, "workspace", "list") {
			return stdout(projectWorkspaceRowsJSON(
				map[string]any{"workspace_id": wsA, "label": firstLabel},
				map[string]any{"workspace_id": wsB, "label": secondLabel},
			)), nil
		}
		return base(cmd)
	}
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	if result.Outcome() != backend.RefKnown || !result.Failed() {
		t.Fatalf("StartProjectWorkspace = %v, want RefKnown-with-cause", result)
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrBootstrap); found {
		t.Fatal("bootstrap ran despite duplicate ownership marker")
	}
}

func TestFocusProjectWorkspaceRejectsDuplicateOwnershipMarker(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(t)
	firstLabel, err := projectLabelFor("Codex", spec.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	secondLabel, err := projectLabelFor("Hermes", spec.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "term_1")
	duplicate := false
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if duplicate && cmd.Kind == exec.KindHerdrProjectTabProbe && hasSubcommand(cmd, "workspace", "list") {
			return stdout(projectWorkspaceRowsJSON(
				map[string]any{"workspace_id": wsA, "label": firstLabel},
				map[string]any{"workspace_id": wsB, "label": secondLabel},
			)), nil
		}
		return base(cmd)
	}
	result := a.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	ref, ok := result.Ref()
	if !ok || result.Outcome() != backend.RefKnown {
		t.Fatalf("StartProjectWorkspace = %v, want RefKnown", result)
	}
	duplicate = true
	before := len(run.calls())
	if err := a.FocusProjectWorkspace(context.Background(), ref); err == nil {
		t.Fatal("FocusProjectWorkspace accepted a duplicate ownership marker")
	}
	for _, call := range run.calls()[before:] {
		if call.Kind == exec.KindHerdrProjectTabFocus {
			t.Fatal("tab focus ran after duplicate ownership marker")
		}
	}
}
