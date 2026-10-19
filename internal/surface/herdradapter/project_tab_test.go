//go:build unix

package herdradapter

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func projectWorkspaceJSON(workspace, label string) []byte {
	return projectWorkspaceRowsJSON(map[string]any{"workspace_id": workspace, "label": label})
}

func projectWorkspaceRowsJSON(rows ...map[string]any) []byte {
	return mustJSON(map[string]any{"id": "workspace:list", "result": map[string]any{
		"type": "workspace_list", "workspaces": rows,
	}})
}

func projectTabsJSON(rows ...map[string]any) []byte {
	return mustJSON(map[string]any{"id": "tab:list", "result": map[string]any{
		"type": "tab_list", "tabs": rows,
	}})
}

func projectPaneJSON(workspace, tab, pane, terminal string) []byte {
	return mustJSON(map[string]any{"id": "pane:get", "result": map[string]any{
		"type": "pane_info", "pane": map[string]any{
			"pane_id": pane, "tab_id": tab, "workspace_id": workspace, "terminal_id": terminal,
			"agent": "", "agent_status": "idle", "focused": false,
		},
	}})
}

func projectCreateJSON(workspace, tab, pane, terminal string) []byte {
	return mustJSON(map[string]any{"id": "tab:create", "result": map[string]any{
		"type":      "tab_created",
		"tab":       map[string]any{"tab_id": tab, "workspace_id": workspace},
		"root_pane": map[string]any{"pane_id": pane, "tab_id": tab, "workspace_id": workspace, "terminal_id": terminal},
	}})
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestProjectTabStartUsesParentAndTerminalIdentity(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a, _, parent := startOn(t, run, map[string]string{sessionEnv: "project"})
	parent = parentWithTerminal(t, parent, "term_1")
	spec, childTag := newSpec(t)
	project, err := a.ForProjectTab(parent)
	if err != nil {
		t.Fatalf("ForProjectTab: %v", err)
	}
	childWorkspace := wsA
	childTab := "w2R:t2"
	childPane := "w2R:p2"
	childTerminal := "term_2"
	childLabel, err := projectLabelFor("Codex + Pi", childTag.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	created := false
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrProjectTabCreate {
			created = true
			return stdout(projectCreateJSON(childWorkspace, childTab, childPane, childTerminal)), nil
		}
		if cmd.Kind == exec.KindHerdrProjectTabProbe || cmd.Kind == exec.KindHerdrProjectTabReconcile || cmd.Kind == exec.KindHerdrProbe {
			if hasSubcommand(cmd, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, parent.OwnershipName())), nil
			}
			if hasSubcommand(cmd, "tab", "list") {
				rows := []map[string]any{{"tab_id": tabA, "workspace_id": wsA, "label": "parent", "pane_count": 1}}
				if created {
					rows = append(rows, map[string]any{"tab_id": childTab, "workspace_id": wsA, "label": childLabel, "pane_count": 1})
				}
				return stdout(projectTabsJSON(rows...)), nil
			}
		}
		if cmd.Kind == exec.KindHerdrPaneStatus {
			if containsArg(cmd.Args, exec.Opaque(paneA)) {
				return stdout(projectPaneJSON(wsA, tabA, paneA, "term_1")), nil
			}
			return stdout(projectPaneJSON(wsA, childTab, childPane, childTerminal)), nil
		}
		return base(cmd)
	}

	result := project.Start(context.Background(), spec)
	ref, ok := result.Ref()
	if !ok || result.Outcome() != backend.RefKnown {
		t.Fatalf("Start result = %v, ref=%v; want RefKnown", result, ok)
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		t.Fatalf("HerdrIdentity: %v", err)
	}
	if identity.Workspace() != wsA || identity.Tab() != childTab || identity.Pane() != childPane || identity.Terminal() != childTerminal {
		t.Fatalf("child identity = %#v, want workspace/tab/pane/terminal %q/%q/%q/%q", identity, wsA, childTab, childPane, childTerminal)
	}
	create, found := commandOfKind(run.calls(), exec.KindHerdrProjectTabCreate)
	if !found || !hasSubcommand(create, "tab", "create") {
		t.Fatal("project tab create command was not issued")
	}
	if !containsArg(create.Args, exec.Opaque(wsA)) || !containsArg(create.Args, exec.Opaque(childTag.OwnershipName())) {
		t.Fatal("project tab create did not carry exact workspace and child marker")
	}
	if result := a.Close(context.Background(), ref); result.State() != backend.CloseIdentityMismatch {
		t.Fatalf("generic Adapter.Close state = %v, want identity-mismatch", result.State())
	}
}

func TestProjectTabSkipsBootstrapWhenIdentityChangesDuringIdle(t *testing.T) {
	for _, mutation := range []string{"server", "pane", "terminal"} {
		t.Run(mutation, func(t *testing.T) {
			testProjectTabSkipsBootstrapOnMutation(t, mutation)
		})
	}
}

func TestForProjectTabRequiresCompleteParentIdentity(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a, _, parent := startOn(t, run, map[string]string{sessionEnv: "project"})
	if _, err := a.ForProjectTab(parent); err == nil {
		t.Fatal("ForProjectTab accepted a parent without terminal identity")
	}
}

func TestProjectTabRefusesDuplicateParentWorkspaceMarkerBeforeCreate(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	a, _, parent := startOn(t, run, map[string]string{sessionEnv: "project"})
	parent = parentWithTerminal(t, parent, "term_1")
	spec, _ := newSpec(t)
	firstLabel, err := projectLabelFor("Codex", parent.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	secondLabel, err := projectLabelFor("Pi", parent.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
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
	project, err := a.ForProjectTab(parent)
	if err != nil {
		t.Fatalf("ForProjectTab: %v", err)
	}
	result := project.Start(context.Background(), spec)
	if result.Outcome() != backend.NotMutated {
		t.Fatalf("ProjectTab.Start = %v, want NotMutated", result)
	}
	if _, found := commandOfKind(run.calls(), exec.KindHerdrProjectTabCreate); found {
		t.Fatal("project tab create ran with a duplicate parent workspace marker")
	}
}

func TestProjectTabRefusesDuplicateCompositeChildMarkerBeforeCreate(test *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	adapter, _, parent := startOn(test, run, map[string]string{sessionEnv: "project"})
	parent = parentWithTerminal(test, parent, "term_1")
	spec, tag := newSpec(test)
	firstLabel, err := projectLabelFor("Codex", tag.OwnershipName())
	if err != nil {
		test.Fatal(err)
	}
	secondLabel, err := projectLabelFor("Pi", tag.OwnershipName())
	if err != nil {
		test.Fatal(err)
	}
	base := run.fake.RunFunc
	run.fake.RunFunc = func(command exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if command.Kind == exec.KindHerdrProjectTabProbe {
			if hasSubcommand(command, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, parent.OwnershipName())), nil
			}
			if hasSubcommand(command, "tab", "list") {
				return stdout(projectTabsJSON(
					map[string]any{"tab_id": tabA, "workspace_id": wsA, "label": "parent", "pane_count": 1},
					map[string]any{"tab_id": "w2R:t2", "workspace_id": wsA, "label": firstLabel, "pane_count": 1},
					map[string]any{"tab_id": "w2R:t3", "workspace_id": wsA, "label": secondLabel, "pane_count": 1},
				)), nil
			}
		}
		if command.Kind == exec.KindHerdrPaneStatus && containsArg(command.Args, exec.Opaque(paneA)) {
			return stdout(projectPaneJSON(wsA, tabA, paneA, "term_1")), nil
		}
		return base(command)
	}
	project, err := adapter.ForProjectTab(parent)
	if err != nil {
		test.Fatalf("ForProjectTab: %v", err)
	}
	before := len(run.calls())
	result := project.Start(context.Background(), spec)
	if result.Outcome() != backend.NotMutated {
		test.Fatalf("ProjectTab.Start = %v, want NotMutated", result)
	}
	if _, found := commandOfKind(run.calls()[before:], exec.KindHerdrProjectTabCreate); found {
		test.Fatal("project tab create ran with duplicate composite child markers")
	}
	if _, found := commandOfKind(run.calls()[before:], exec.KindHerdrBootstrap); found {
		test.Fatal("bootstrap ran with duplicate composite child markers")
	}
}

func TestProjectTabValidatesPersistedParentRootBeforeCreate(t *testing.T) {
	for _, mutation := range []string{"tab", "terminal"} {
		t.Run(mutation, func(t *testing.T) {
			run := newRunner()
			run.sessions = func() (exec.SensitiveResult, error) {
				return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
			}
			a, _, parent := startOn(t, run, map[string]string{sessionEnv: "project"})
			parent = parentWithTerminal(t, parent, "term_1")
			base := run.fake.RunFunc
			run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
				if cmd.Kind == exec.KindHerdrProjectTabProbe {
					if hasSubcommand(cmd, "workspace", "list") {
						return stdout(projectWorkspaceJSON(wsA, parent.OwnershipName())), nil
					}
					if hasSubcommand(cmd, "tab", "list") {
						tab := tabA
						if mutation == "tab" {
							tab = "w2R:t9"
						}
						return stdout(projectTabsJSON(map[string]any{
							"tab_id": tab, "workspace_id": wsA, "label": "parent", "pane_count": 1,
						})), nil
					}
				}
				if cmd.Kind == exec.KindHerdrPaneStatus {
					terminal := "term_1"
					if mutation == "terminal" {
						terminal = "term_wrong"
					}
					return stdout(projectPaneJSON(wsA, tabA, paneA, terminal)), nil
				}
				return base(cmd)
			}
			spec, _ := newSpec(t)
			project, err := a.ForProjectTab(parent)
			if err != nil {
				t.Fatalf("ForProjectTab: %v", err)
			}
			result := project.Start(context.Background(), spec)
			if result.Outcome() != backend.NotMutated {
				t.Fatalf("ProjectTab.Start = %v, want NotMutated", result)
			}
			if _, found := commandOfKind(run.calls(), exec.KindHerdrProjectTabCreate); found {
				t.Fatal("project tab create ran after parent root identity drift")
			}
		})
	}
}

func TestProjectTabFocusRefusesTurnoverAfterFocus(t *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	turnover := false
	oldSocket := liveSocket(liveInode)
	newSocket := liveSocket(liveInode + 1)
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"}, WithLstat(func(path string) (os.FileInfo, error) {
		if turnover {
			return newSocket(path)
		}
		return oldSocket(path)
	}))
	parentSpec, _ := newSpec(t)
	parentResult := a.Start(context.Background(), parentSpec)
	parent, ok := parentResult.Ref()
	if !ok {
		t.Fatalf("parent Start = %v, want reference", parentResult)
	}
	parent = parentWithTerminal(t, parent, "term_1")
	childSpec, childTag := newSpec(t)
	created := false
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrProjectTabCreate {
			created = true
			return stdout(projectCreateJSON(wsA, "w2R:t2", "w2R:p2", "term_2")), nil
		}
		if cmd.Kind == exec.KindHerdrProjectTabProbe {
			if hasSubcommand(cmd, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, parent.OwnershipName())), nil
			}
			if hasSubcommand(cmd, "tab", "list") {
				rows := []map[string]any{{"tab_id": tabA, "workspace_id": wsA, "label": "parent", "pane_count": 1}}
				if created {
					rows = append(rows, map[string]any{"tab_id": "w2R:t2", "workspace_id": wsA, "label": childTag.OwnershipName(), "pane_count": 1})
				}
				return stdout(projectTabsJSON(rows...)), nil
			}
		}
		if cmd.Kind == exec.KindHerdrPaneStatus {
			if containsArg(cmd.Args, exec.Opaque(paneA)) {
				return stdout(projectPaneJSON(wsA, tabA, paneA, "term_1")), nil
			}
			return stdout(projectPaneJSON(wsA, "w2R:t2", "w2R:p2", "term_2")), nil
		}
		if cmd.Kind == exec.KindHerdrProjectTabFocus {
			turnover = true
			return stdout([]byte(`{"id":"tab:focus","result":{"type":"ok"}}`)), nil
		}
		return base(cmd)
	}
	project, err := a.ForProjectTab(parent)
	if err != nil {
		t.Fatalf("ForProjectTab: %v", err)
	}
	result := project.Start(context.Background(), childSpec)
	ref, ok := result.Ref()
	if !ok || result.Outcome() != backend.RefKnown {
		t.Fatalf("ProjectTab.Start = %v, want RefKnown", result)
	}
	if err := project.Focus(context.Background(), ref); err == nil {
		t.Fatal("ProjectTab.Focus accepted server turnover after focus")
	}
}

func testProjectTabSkipsBootstrapOnMutation(t *testing.T, mutation string) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	idleComplete := false
	stats := 0
	oldSocket := liveSocket(liveInode)
	newSocket := liveSocket(liveInode + 1)
	a := newTestAdapter(t, run, map[string]string{sessionEnv: "project"}, WithLstat(func(path string) (os.FileInfo, error) {
		stats++
		if mutation == "server" && idleComplete {
			return newSocket(path)
		}
		return oldSocket(path)
	}))
	parentSpec, _ := newSpec(t)
	parentResult := a.Start(context.Background(), parentSpec)
	parent, ok := parentResult.Ref()
	if !ok {
		t.Fatalf("parent Start = %v, want reference", parentResult)
	}
	parent = parentWithTerminal(t, parent, "term_1")
	childSpec, childTag := newSpec(t)
	created := false
	base := run.fake.RunFunc
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrProjectTabCreate {
			created = true
			return stdout(projectCreateJSON(wsA, "w2R:t2", "w2R:p2", "term_2")), nil
		}
		if cmd.Kind == exec.KindHerdrProjectTabProbe {
			if hasSubcommand(cmd, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, parent.OwnershipName())), nil
			}
			if hasSubcommand(cmd, "tab", "list") {
				rows := []map[string]any{{"tab_id": tabA, "workspace_id": wsA, "label": "parent", "pane_count": 1}}
				if created {
					rows = append(rows, map[string]any{"tab_id": "w2R:t2", "workspace_id": wsA, "label": childTag.OwnershipName(), "pane_count": 1})
				}
				return stdout(projectTabsJSON(rows...)), nil
			}
		}
		if cmd.Kind == exec.KindHerdrPaneStatus {
			if containsArg(cmd.Args, exec.Opaque(paneA)) {
				return stdout(projectPaneJSON(wsA, tabA, paneA, "term_1")), nil
			}
			paneTab, terminal := "w2R:t2", "term_2"
			if idleComplete && mutation == "pane" {
				paneTab = "w2R:t3"
			}
			if idleComplete && mutation == "terminal" {
				terminal = "term_3"
			}
			return stdout(projectPaneJSON(wsA, paneTab, "w2R:p2", terminal)), nil
		}
		if cmd.Kind == exec.KindHerdrPaneInspect {
			result, err := base(cmd)
			idleComplete = true
			return result, err
		}
		return base(cmd)
	}

	project, err := a.ForProjectTab(parent)
	if err != nil {
		t.Fatalf("ForProjectTab: %v", err)
	}
	result := project.Start(context.Background(), childSpec)
	if _, ok := result.Ref(); !ok {
		t.Fatalf("child Start = %v, want a known reference with a turnover cause", result)
	}
	bootstraps := 0
	for _, call := range run.calls() {
		if call.Kind == exec.KindHerdrBootstrap {
			bootstraps++
		}
	}
	if bootstraps != 1 {
		t.Fatalf("child bootstrap count = %d, want only the parent's bootstrap (mutation=%s, stats=%d)", bootstraps, mutation, stats)
	}
}

func parentWithTerminal(t *testing.T, parent backend.Ref, terminal string) backend.Ref {
	t.Helper()
	identity, err := parent.HerdrIdentity()
	if err != nil {
		t.Fatalf("HerdrIdentity: %v", err)
	}
	identity, err = identity.WithTerminal(terminal)
	if err != nil {
		t.Fatalf("WithTerminal: %v", err)
	}
	ref, err := backend.NewHerdrRef(parent.Source(), parent.Server(), parent.Tag(), identity)
	if err != nil {
		t.Fatalf("NewHerdrRef: %v", err)
	}
	endpoint, ok := parent.Endpoint()
	if !ok {
		t.Fatal("parent reference has no endpoint")
	}
	ref, err = ref.WithEndpoint(endpoint)
	if err != nil {
		t.Fatalf("WithEndpoint: %v", err)
	}
	return ref
}

func hasSubcommand(cmd exec.SensitiveCommand, values ...string) bool {
	for i := 0; i+len(values) <= len(cmd.Args); i++ {
		matched := true
		for j, value := range values {
			if !cmd.Args[i+j].Equal(fixedArg(value)) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func fixedArg(value string) exec.Arg {
	switch value {
	case "workspace":
		return exec.MustFixed("workspace")
	case "tab":
		return exec.MustFixed("tab")
	case "list":
		return exec.MustFixed("list")
	case "create":
		return exec.MustFixed("create")
	case "rename":
		return exec.MustFixed("rename")
	default:
		panic("unexpected fixed test argument")
	}
}

func containsArg(args []exec.Arg, want exec.Arg) bool {
	for _, arg := range args {
		if arg.Equal(want) {
			return true
		}
	}
	return false
}
