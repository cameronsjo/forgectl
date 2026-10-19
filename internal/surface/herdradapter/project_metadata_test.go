//go:build unix

package herdradapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func TestShowProjectWorkspaceUsesCosmeticMetadataWithoutChangingOwnership(test *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	adapter := newTestAdapter(test, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(test)
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "term_1")
	result := adapter.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	ref, ok := result.Ref()
	if !ok || result.Outcome() != backend.RefKnown {
		test.Fatalf("StartProjectWorkspace = %v, want RefKnown", result)
	}

	if err := adapter.ShowProjectWorkspace(context.Background(), ref, "Project Alpha"); err != nil {
		test.Fatalf("ShowProjectWorkspace: %v", err)
	}

	var workspace, pane, workspaceRename, tabRename *exec.SensitiveCommand
	metadataCount := 0
	for index := range run.calls() {
		call := run.calls()[index]
		if call.Kind != exec.KindHerdrMetadata {
			continue
		}
		metadataCount++
		if hasSubcommand(call, "workspace", "rename") {
			workspaceRename = &call
		} else if hasSubcommand(call, "tab", "rename") {
			tabRename = &call
		} else if containsArg(call.Args, exec.MustFixed("workspace")) {
			workspace = &call
		} else if containsArg(call.Args, exec.MustFixed("pane")) {
			pane = &call
		}
	}
	if metadataCount != 4 || workspace == nil || pane == nil || workspaceRename == nil || tabRename == nil {
		test.Fatalf("metadata calls = %#v, want workspace/tab renames and workspace/pane reports", run.calls())
	}
	label, err := projectLabelFor("Project Alpha", ref.OwnershipName())
	if err != nil {
		test.Fatal(err)
	}
	assertArgv(test, *workspaceRename, []exec.Arg{
		exec.MustFixed("--session"), exec.Opaque(adapter.Session()),
		exec.MustFixed("workspace"), exec.MustFixed("rename"), exec.Opaque(wsA), exec.Opaque(label),
	})
	assertArgv(test, *tabRename, []exec.Arg{
		exec.MustFixed("--session"), exec.Opaque(adapter.Session()),
		exec.MustFixed("tab"), exec.MustFixed("rename"), exec.Opaque(tabA), exec.Opaque(label),
	})
	assertArgv(test, *workspace, []exec.Arg{
		exec.MustFixed("--session"), exec.Opaque(adapter.Session()),
		exec.MustFixed("workspace"), exec.MustFixed("report-metadata"), exec.Opaque(wsA),
		exec.MustFixed("--source"), exec.MustFixed(MetadataSource),
		exec.MustFixed("--token"), exec.Opaque("forgectl_project=Project Alpha"),
	})
	assertArgv(test, *pane, []exec.Arg{
		exec.MustFixed("--session"), exec.Opaque(adapter.Session()),
		exec.MustFixed("pane"), exec.MustFixed("report-metadata"), exec.Opaque("w2R:p1"),
		exec.MustFixed("--source"), exec.MustFixed(MetadataSource),
		exec.MustFixed("--display-agent"), exec.Opaque("Project Alpha"),
	})
}

func TestShowProjectWorkspaceRefusesInvalidNameBeforeReadingHerdr(test *testing.T) {
	adapter, run, ref := startClean(test)
	before := len(run.calls())
	if err := adapter.ShowProjectWorkspace(context.Background(), ref, strings.Repeat("x", MaxTokenValueLen+1)); !errors.Is(err, ErrBadToken) {
		test.Fatalf("err = %v, want ErrBadToken", err)
	}
	if len(run.calls()) != before {
		test.Fatal("invalid project name caused a herdr call")
	}
}

func TestShowProjectWorkspaceRefusesChangedRootBeforeMetadata(test *testing.T) {
	run := newRunner()
	run.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	adapter := newTestAdapter(test, run, map[string]string{sessionEnv: "project"})
	spec, _ := newSpec(test)
	configureProjectWorkspaceRunner(run, spec.OwnershipName(), "wrong-terminal")
	result := adapter.StartProjectWorkspace(context.Background(), spec, func(backend.ServerID, backend.EndpointID) error { return nil })
	ref, ok := result.Ref()
	if !ok {
		test.Fatalf("StartProjectWorkspace = %v, want a known reference", result)
	}
	before := len(run.calls())
	if err := adapter.ShowProjectWorkspace(context.Background(), ref, "Project Alpha"); err == nil {
		test.Fatal("ShowProjectWorkspace accepted a changed root terminal")
	}
	for _, call := range run.calls()[before:] {
		if call.Kind == exec.KindHerdrMetadata {
			test.Fatal("metadata was written after root identity mismatch")
		}
	}
}

type projectThreadMetadataFixture struct {
	adapter             *Adapter
	runner              *scriptedRunner
	project             *ProjectTabAdapter
	child               backend.Ref
	childMarker         string
	childCreated        bool
	childWorkspace      string
	childTab            string
	childPane           string
	childTerminal       string
	serverTurnedOver    bool
	afterRenameTerminal string
	afterRenameServer   bool
}

func newProjectThreadMetadataFixture(test *testing.T) *projectThreadMetadataFixture {
	test.Helper()
	runner := newRunner()
	runner.sessions = func() (exec.SensitiveResult, error) {
		return stdout(sessionsJSON(map[string]any{"name": "project", "running": true, "socket_path": testSocket})), nil
	}
	fixture := &projectThreadMetadataFixture{
		childMarker:    "child-marker",
		childWorkspace: wsA,
		childTab:       "w2R:t2",
		childPane:      "w2R:p2",
		childTerminal:  "term_2",
	}
	fixture.adapter = newTestAdapter(test, runner, map[string]string{sessionEnv: "project"}, WithLstat(func(path string) (os.FileInfo, error) {
		if fixture.serverTurnedOver {
			return liveSocket(liveInode + 1)(path)
		}
		return liveSocket(liveInode)(path)
	}))
	parentSpec, _ := newSpec(test)
	parentResult := fixture.adapter.Start(context.Background(), parentSpec)
	parent, ok := parentResult.Ref()
	if !ok {
		test.Fatalf("parent Start = %v, want a reference", parentResult)
	}
	parent = parentWithTerminal(test, parent, "term_1")
	childSpec, childTag := newSpec(test)
	fixture.childMarker = childTag.OwnershipName()
	base := runner.fake.RunFunc
	runner.fake.RunFunc = func(command exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if command.Kind == exec.KindHerdrMetadata && hasSubcommand(command, "tab", "rename") {
			if fixture.afterRenameTerminal != "" {
				fixture.childTerminal = fixture.afterRenameTerminal
			}
			if fixture.afterRenameServer {
				fixture.serverTurnedOver = true
			}
		}
		if command.Kind == exec.KindHerdrProjectTabCreate {
			fixture.childCreated = true
			return stdout(projectCreateJSON(fixture.childWorkspace, fixture.childTab, fixture.childPane, fixture.childTerminal)), nil
		}
		if command.Kind == exec.KindHerdrProjectTabProbe {
			if hasSubcommand(command, "workspace", "list") {
				return stdout(projectWorkspaceJSON(wsA, parent.OwnershipName())), nil
			}
			if hasSubcommand(command, "tab", "list") {
				rows := []map[string]any{{"tab_id": tabA, "workspace_id": wsA, "label": "parent", "pane_count": 1}}
				if fixture.childCreated {
					rows = append(rows, map[string]any{"tab_id": fixture.childTab, "workspace_id": wsA, "label": fixture.childMarker, "pane_count": 1})
				}
				return stdout(projectTabsJSON(rows...)), nil
			}
		}
		if command.Kind == exec.KindHerdrPaneStatus {
			if containsArg(command.Args, exec.Opaque(paneA)) {
				return stdout(projectPaneJSON(wsA, tabA, paneA, "term_1")), nil
			}
			return stdout(projectPaneJSON(fixture.childWorkspace, fixture.childTab, fixture.childPane, fixture.childTerminal)), nil
		}
		return base(command)
	}
	project, err := fixture.adapter.ForProjectTab(parent)
	if err != nil {
		test.Fatalf("ForProjectTab: %v", err)
	}
	childResult := project.Start(context.Background(), childSpec)
	child, ok := childResult.Ref()
	if !ok || childResult.Outcome() != backend.RefKnown {
		test.Fatalf("ProjectTab.Start = %v, want RefKnown", childResult)
	}
	fixture.runner = runner
	fixture.project = project
	fixture.child = child
	return fixture
}

func (fixture *projectThreadMetadataFixture) metadataCalls() []exec.SensitiveCommand {
	var metadata []exec.SensitiveCommand
	for _, command := range fixture.runner.calls() {
		if command.Kind == exec.KindHerdrMetadata {
			metadata = append(metadata, command)
		}
	}
	return metadata
}

func TestShowProjectThreadUsesChildPaneOnly(test *testing.T) {
	fixture := newProjectThreadMetadataFixture(test)
	if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread Beta"); err != nil {
		test.Fatalf("ShowProjectThread: %v", err)
	}
	var rename *exec.SensitiveCommand
	for index := range fixture.runner.calls() {
		call := fixture.runner.calls()[index]
		if call.Kind == exec.KindHerdrMetadata && hasSubcommand(call, "tab", "rename") {
			rename = &call
		}
	}
	if rename == nil {
		test.Fatal("ShowProjectThread did not rename the child tab")
	}
	label, err := projectLabelFor("Thread Beta", fixture.childMarker)
	if err != nil {
		test.Fatal(err)
	}
	assertArgv(test, *rename, []exec.Arg{
		exec.MustFixed("--session"), exec.Opaque(fixture.adapter.Session()),
		exec.MustFixed("tab"), exec.MustFixed("rename"), exec.Opaque(fixture.childTab), exec.Opaque(label),
	})
	metadata := fixture.metadataCalls()
	if len(metadata) != 2 {
		test.Fatalf("metadata calls = %d, want one tab rename and one child pane report", len(metadata))
	}
	if !hasSubcommand(metadata[0], "tab", "rename") {
		test.Fatalf("first metadata call = %#v, want tab rename", metadata[0])
	}
	if containsArg(metadata[1].Args, exec.Opaque(paneA)) || !containsArg(metadata[1].Args, exec.MustFixed("pane")) {
		test.Fatal("parent pane received child metadata")
	}
	assertArgv(test, metadata[1], []exec.Arg{
		exec.MustFixed("--session"), exec.Opaque(fixture.adapter.Session()),
		exec.MustFixed("pane"), exec.MustFixed("report-metadata"), exec.Opaque(fixture.childPane),
		exec.MustFixed("--source"), exec.MustFixed(MetadataSource),
		exec.MustFixed("--display-agent"), exec.Opaque("Thread Beta"),
	})
}

func TestShowProjectThreadRefusesChildIdentityMismatchBeforeMetadata(test *testing.T) {
	fixture := newProjectThreadMetadataFixture(test)
	fixture.childMarker = "other-thread"
	if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread Beta"); err == nil {
		test.Fatal("ShowProjectThread accepted a changed child marker")
	}
	if len(fixture.metadataCalls()) != 0 {
		test.Fatal("metadata was written after child identity mismatch")
	}
}

func TestShowProjectThreadRefusesIdentityDriftAfterTabRename(test *testing.T) {
	for name, configure := range map[string]func(*projectThreadMetadataFixture){
		"terminal": func(fixture *projectThreadMetadataFixture) {
			fixture.afterRenameTerminal = "term_changed"
		},
		"server": func(fixture *projectThreadMetadataFixture) {
			fixture.afterRenameServer = true
		},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newProjectThreadMetadataFixture(test)
			configure(fixture)
			if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread Beta"); err == nil {
				test.Fatal("ShowProjectThread accepted identity drift after tab rename")
			}
			metadata := fixture.metadataCalls()
			if len(metadata) != 1 || !hasSubcommand(metadata[0], "tab", "rename") {
				test.Fatalf("metadata calls = %#v, want only the tab rename", metadata)
			}
		})
	}
}

func TestShowProjectThreadRefusesServerTurnoverBeforeMetadata(test *testing.T) {
	fixture := newProjectThreadMetadataFixture(test)
	fixture.serverTurnedOver = true
	if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread Beta"); err == nil {
		test.Fatal("ShowProjectThread accepted a changed server incarnation")
	}
	if len(fixture.metadataCalls()) != 0 {
		test.Fatal("metadata was written after server turnover")
	}
}

func TestShowProjectThreadRefusesForeignChildPaneBeforeMetadata(test *testing.T) {
	fixture := newProjectThreadMetadataFixture(test)
	fixture.childWorkspace = "w9"
	if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread Beta"); err == nil {
		test.Fatal("ShowProjectThread accepted a pane in another workspace")
	}
	if len(fixture.metadataCalls()) != 0 {
		test.Fatal("metadata was written to a foreign child pane")
	}
}

func TestShowProjectThreadRefusesChangedChildTerminalBeforeMetadata(test *testing.T) {
	fixture := newProjectThreadMetadataFixture(test)
	fixture.childTerminal = "term_changed"
	if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread Beta"); err == nil {
		test.Fatal("ShowProjectThread accepted a changed child terminal")
	}
	if len(fixture.metadataCalls()) != 0 {
		test.Fatal("metadata was written after child terminal drift")
	}
}

func TestShowProjectThreadRefusesControlNameBeforeHerdrRead(test *testing.T) {
	fixture := newProjectThreadMetadataFixture(test)
	before := len(fixture.runner.calls())
	if err := fixture.project.ShowProjectThread(context.Background(), fixture.child, "Thread\x1bBeta"); !errors.Is(err, ErrBadToken) {
		test.Fatalf("err = %v, want ErrBadToken", err)
	}
	if len(fixture.runner.calls()) != before {
		test.Fatal("invalid thread name caused a herdr call")
	}
}
