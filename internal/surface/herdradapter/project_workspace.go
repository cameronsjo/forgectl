package herdradapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/herdr/wire"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

var ErrProjectWorkspace = errors.New("herdradapter: project workspace identity mismatch")

type projectWorkspaceCreateResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	Tab struct {
		TabID       string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"tab"`
	RootPane struct {
		PaneID      string `json:"pane_id"`
		TabID       string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
		TerminalID  string `json:"terminal_id"`
	} `json:"root_pane"`
}

func parseProjectWorkspaceCreated(out exec.BoundedOutput) (createdWorkspace, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return createdWorkspace{}, errors.New("the herdr project workspace create reply was truncated")
	}
	reply, err := wire.DecodeResult[projectWorkspaceCreateResult](raw)
	if errors.Is(err, wire.ErrNoResult) {
		return createdWorkspace{}, errors.New("the herdr project workspace create reply had no result")
	}
	if err != nil {
		return createdWorkspace{}, errors.New("the herdr project workspace create reply was not readable JSON")
	}
	created := createdWorkspace{
		WorkspaceID: reply.Workspace.WorkspaceID,
		TabID:       reply.Tab.TabID,
		PaneID:      reply.RootPane.PaneID,
		TerminalID:  reply.RootPane.TerminalID,
	}
	if created.WorkspaceID == "" || created.TabID == "" || created.PaneID == "" || created.TerminalID == "" ||
		reply.Tab.WorkspaceID != created.WorkspaceID ||
		reply.RootPane.WorkspaceID != created.WorkspaceID ||
		reply.RootPane.TabID != created.TabID {
		return createdWorkspace{}, errors.New("the herdr project workspace create reply named no complete matching workspace, tab, pane, and terminal")
	}
	identity, err := backend.NewHerdrIdentity(created.WorkspaceID, created.TabID, created.PaneID)
	if err != nil {
		return createdWorkspace{}, err
	}
	if _, err := identity.WithTerminal(created.TerminalID); err != nil {
		return createdWorkspace{}, err
	}
	return created, nil
}

func (a *Adapter) verifyProjectWorkspace(
	ctx context.Context,
	spec backend.StartSpec,
	created createdWorkspace,
	server serverInfo,
	before map[string]workspaceRow,
) *backend.StartCause {
	if _, existed := before[created.WorkspaceID]; existed {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	rows, cause := a.snapshot(ctx, exec.KindHerdrProjectTabProbe)
	if cause != nil {
		return cause
	}
	row, ok := rows[created.WorkspaceID]
	markerMatches := 0
	for id, candidate := range rows {
		if !projectLabelMatches(candidate.marker, spec.OwnershipName()) {
			continue
		}
		markerMatches++
		if id != created.WorkspaceID {
			c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
			return &c
		}
	}
	if !ok || !projectLabelMatches(row.marker, spec.OwnershipName()) || markerMatches != 1 {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	tabs, cause := a.projectWorkspaceTabs(ctx, exec.KindHerdrProjectTabProbe, created.WorkspaceID)
	if cause != nil {
		return cause
	}
	foundTab := 0
	for _, tab := range tabs {
		if tab.TabID == created.TabID {
			foundTab++
			if tab.WorkspaceID != created.WorkspaceID || tab.PaneCount != 1 {
				c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
				return &c
			}
		}
	}
	if foundTab != 1 {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	pane, err := a.paneRecord(ctx, created.PaneID, created.WorkspaceID)
	if err != nil {
		c := backend.NewStartCause(backend.FailureUnavailable, err)
		return &c
	}
	if pane.TabID != created.TabID || pane.TerminalID != created.TerminalID {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	fresh, freshCause := a.freshReadiness(ctx, server)
	if freshCause != nil {
		return freshCause
	}
	if !server.incarnation.Matches(fresh.incarnation) ||
		!backend.EndpointOf(server.socket).Matches(backend.EndpointOf(fresh.socket)) {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	return nil
}

func (a *Adapter) reconcileProjectWorkspace(
	ctx context.Context,
	spec backend.StartSpec,
	before map[string]workspaceRow,
	server serverInfo,
	cause backend.StartCause,
) backend.StartResult {
	rows, reconcileCause := a.snapshot(ctx, exec.KindHerdrProjectTabReconcile)
	if reconcileCause != nil {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	var matches []string
	for id, row := range rows {
		if projectLabelMatches(row.marker, spec.OwnershipName()) {
			if _, existed := before[id]; !existed {
				matches = append(matches, id)
			}
		}
	}
	if len(matches) == 0 {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	if len(matches) != 1 {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	tabs, tabsCause := a.projectWorkspaceTabs(ctx, exec.KindHerdrProjectTabReconcile, matches[0])
	if tabsCause != nil || len(tabs) != 1 || tabs[0].PaneCount != 1 {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	panes, panesCause := a.projectWorkspacePanes(ctx, exec.KindHerdrProjectTabReconcile, matches[0])
	if panesCause != nil {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	var root *herdr.Pane
	for i := range panes {
		if panes[i].TabID != tabs[0].TabID {
			continue
		}
		if root != nil || panes[i].TerminalID == "" {
			return backend.NewOutcomeUnknown(spec.Tag(), cause)
		}
		root = &panes[i]
	}
	if root == nil {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	fresh, freshCause := a.freshReadiness(ctx, server)
	if freshCause != nil || !server.incarnation.Matches(fresh.incarnation) {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	created := createdWorkspace{
		WorkspaceID: matches[0],
		TabID:       tabs[0].TabID,
		PaneID:      root.PaneID,
		TerminalID:  root.TerminalID,
	}
	ref, err := a.referenceWithTerminal(created, spec.Tag(), fresh, true)
	if err != nil {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	return backend.NewRefKnownWithCause(ref, cause)
}

func (a *Adapter) projectWorkspaceTabs(ctx context.Context, kind exec.CommandKind, workspace string) ([]herdr.Tab, *backend.StartCause) {
	res, runErr := a.run.RunSensitive(ctx, a.command(kind,
		exec.MustFixed("tab"), exec.MustFixed("list"), exec.MustFixed("--workspace"), exec.Opaque(workspace)))
	if runErr != nil {
		cause := a.classifyRunError(runErr, res)
		return nil, &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project workspace tab list was truncated"))
		return nil, &cause
	}
	decoded, err := wire.DecodeResult[struct {
		Tabs *[]herdr.Tab `json:"tabs"`
	}](raw)
	if err != nil || decoded.Tabs == nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project workspace tab list was not readable JSON"))
		return nil, &cause
	}
	for _, tab := range *decoded.Tabs {
		if tab.WorkspaceID != workspace || tab.TabID == "" {
			cause := backend.NewStartCause(backend.FailureMalformedResponse, ErrProjectWorkspace)
			return nil, &cause
		}
		if _, err := backend.NewHerdrIdentity(workspace, tab.TabID, ""); err != nil {
			cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
			return nil, &cause
		}
	}
	return *decoded.Tabs, nil
}

func (a *Adapter) projectWorkspacePanes(ctx context.Context, kind exec.CommandKind, workspace string) ([]herdr.Pane, *backend.StartCause) {
	res, runErr := a.run.RunSensitive(ctx, a.command(kind,
		exec.MustFixed("pane"), exec.MustFixed("list"), exec.MustFixed("--workspace"), exec.Opaque(workspace)))
	if runErr != nil {
		cause := a.classifyRunError(runErr, res)
		return nil, &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project workspace pane list was truncated"))
		return nil, &cause
	}
	decoded, err := wire.DecodeResult[struct {
		Panes *[]herdr.Pane `json:"panes"`
	}](raw)
	if err != nil || decoded.Panes == nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project workspace pane list was not readable JSON"))
		return nil, &cause
	}
	for _, pane := range *decoded.Panes {
		if pane.WorkspaceID != workspace || pane.PaneID == "" || pane.TabID == "" {
			cause := backend.NewStartCause(backend.FailureMalformedResponse, ErrProjectWorkspace)
			return nil, &cause
		}
	}
	return *decoded.Panes, nil
}

func (a *Adapter) FocusProjectWorkspace(ctx context.Context, ref backend.Ref) error {
	identity, err := a.validateProjectWorkspaceRef(ref)
	if err != nil {
		return err
	}
	server, cause := a.readiness(ctx)
	if cause != nil {
		return fmt.Errorf("%w: %v", ErrProjectWorkspace, cause)
	}
	endpoint, _ := ref.Endpoint()
	if !ref.Server().Matches(server.incarnation) || !endpoint.Matches(backend.EndpointOf(server.socket)) {
		return fmt.Errorf("%w: server incarnation or endpoint changed", ErrProjectWorkspace)
	}
	if cause := a.verifyProjectWorkspaceRef(ctx, ref, identity, server); cause != nil {
		return fmt.Errorf("%w: %v", ErrProjectWorkspace, cause)
	}
	fresh, freshCause := a.freshReadiness(ctx, server)
	if freshCause != nil {
		return fmt.Errorf("%w: %v", ErrProjectWorkspace, freshCause)
	}
	if !ref.Server().Matches(fresh.incarnation) || !endpoint.Matches(backend.EndpointOf(fresh.socket)) {
		return fmt.Errorf("%w: server incarnation or endpoint changed", ErrProjectWorkspace)
	}
	server = fresh
	if a.turnover(server) != turnoverNone {
		return fmt.Errorf("%w: herdr server changed before focus", ErrProjectWorkspace)
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrProjectTabFocus,
		exec.MustFixed("tab"), exec.MustFixed("focus"), exec.Opaque(identity.Tab())))
	if runErr != nil {
		return fmt.Errorf("%w: tab focus: %v", ErrProjectWorkspace, a.classifyRunError(runErr, res))
	}
	if a.turnover(server) != turnoverNone {
		return fmt.Errorf("%w: herdr server changed while focusing", ErrProjectWorkspace)
	}
	return nil
}

func (a *Adapter) validateProjectWorkspaceRef(ref backend.Ref) (backend.HerdrIdentity, error) {
	if a.source != backend.HerdrNamedSessionServer() || ref.Kind() != backend.KindHerdr ||
		ref.Source() != backend.HerdrNamedSessionServer() || ref.Source() != a.source {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: focus requires the adapter's named herdr session", ErrProjectWorkspace)
	}
	if err := ref.Validate(); err != nil {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: %w", ErrProjectWorkspace, err)
	}
	identity, err := ref.HerdrIdentity()
	if err != nil || identity.Tab() == "" || identity.Pane() == "" || identity.Terminal() == "" {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: reference has no complete root tab, pane, and terminal", ErrProjectWorkspace)
	}
	if _, ok := ref.Endpoint(); !ok {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: reference has no endpoint", ErrProjectWorkspace)
	}
	return identity, nil
}

func (a *Adapter) verifyProjectWorkspaceRef(ctx context.Context, ref backend.Ref, identity backend.HerdrIdentity, server serverInfo) *backend.StartCause {
	rows, cause := a.snapshot(ctx, exec.KindHerdrProjectTabProbe)
	if cause != nil {
		return cause
	}
	row, ok := rows[identity.Workspace()]
	markerMatches := 0
	for id, candidate := range rows {
		if !projectLabelMatches(candidate.marker, ref.OwnershipName()) {
			continue
		}
		markerMatches++
		if id != identity.Workspace() {
			c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
			return &c
		}
	}
	if !ok || !projectLabelMatches(row.marker, ref.OwnershipName()) || markerMatches != 1 {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	tabs, cause := a.projectWorkspaceTabs(ctx, exec.KindHerdrProjectTabProbe, identity.Workspace())
	if cause != nil {
		return cause
	}
	found := false
	for _, tab := range tabs {
		if tab.TabID == identity.Tab() {
			if found || tab.PaneCount != 1 {
				c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
				return &c
			}
			found = true
		}
	}
	if !found {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	pane, err := a.paneRecord(ctx, identity.Pane(), identity.Workspace())
	if err != nil {
		c := backend.NewStartCause(backend.FailureUnavailable, err)
		return &c
	}
	if pane.TabID != identity.Tab() || pane.TerminalID != identity.Terminal() {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	if !ref.Server().Matches(server.incarnation) {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectWorkspace)
		return &c
	}
	return nil
}
