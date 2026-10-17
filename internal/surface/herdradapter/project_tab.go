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

var (
	ErrProjectTabParent   = errors.New("herdradapter: invalid project tab parent")
	ErrProjectTabIdentity = errors.New("herdradapter: project tab identity mismatch")
)

type ProjectTabAdapter struct {
	adapter  *Adapter
	parent   backend.Ref
	parentID backend.HerdrIdentity
}

var _ backend.Adapter = (*ProjectTabAdapter)(nil)
var _ backend.Closer = (*ProjectTabAdapter)(nil)
var _ backend.Prober = (*ProjectTabAdapter)(nil)

func (a *Adapter) ForProjectTab(parent backend.Ref) (*ProjectTabAdapter, error) {
	if parent.Kind() != backend.KindHerdr || parent.Source() != backend.HerdrNamedSessionServer() || a.source != parent.Source() {
		return nil, fmt.Errorf("%w: project tabs require the adapter's named herdr session", ErrProjectTabParent)
	}
	identity, err := parent.HerdrIdentity()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProjectTabParent, err)
	}
	if identity.Tab() == "" || identity.Pane() == "" || identity.Terminal() == "" {
		return nil, fmt.Errorf("%w: parent reference has no complete root tab, pane, and terminal", ErrProjectTabParent)
	}
	if _, ok := parent.Endpoint(); !ok {
		return nil, fmt.Errorf("%w: parent reference has no endpoint", ErrProjectTabParent)
	}
	if err := parent.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProjectTabParent, err)
	}
	return &ProjectTabAdapter{adapter: a, parent: parent, parentID: identity}, nil
}

func (p *ProjectTabAdapter) Kind() backend.Kind { return backend.KindHerdr }

func (p *ProjectTabAdapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	if err := spec.Validate(); err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	if spec.Tag() == p.parent.Tag() {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureIdentityMismatch,
			fmt.Errorf("%w: child marker equals parent marker", ErrProjectTabIdentity)))
	}
	server, cause := p.adapter.readiness(ctx)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}
	if cause := p.sameServer(server); cause != nil {
		return backend.NewNotMutated(*cause)
	}
	before, cause := p.parentTabs(ctx, exec.KindHerdrProjectTabProbe)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}
	for _, row := range before {
		if projectLabelMatches(row.label, spec.OwnershipName()) {
			return backend.NewNotMutated(backend.NewStartCause(backend.FailureIdentityMismatch,
				fmt.Errorf("%w: child marker already exists", ErrProjectTabIdentity)))
		}
	}

	res, runErr := p.adapter.run.RunSensitive(ctx, p.adapter.command(exec.KindHerdrProjectTabCreate,
		exec.MustFixed("tab"),
		exec.MustFixed("create"),
		exec.MustFixed("--workspace"),
		exec.Opaque(p.parentID.Workspace()),
		exec.MustFixed("--no-focus"),
		exec.MustFixed("--label"),
		exec.Opaque(spec.OwnershipName()),
		exec.MustFixed("--cwd"),
		spec.CWD(),
	))
	if runErr != nil {
		return p.reconcile(ctx, spec, before, server, p.adapter.classifyRunError(runErr, res))
	}
	created, err := parseProjectTabCreated(res.Stdout)
	if err != nil {
		return p.reconcile(ctx, spec, before, server,
			backend.NewStartCause(backend.FailureMalformedResponse, err))
	}
	if created.WorkspaceID != p.parentID.Workspace() {
		return p.reconcile(ctx, spec, before, server,
			backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity))
	}
	ref, err := p.projectReference(created, spec.Tag(), server)
	if err != nil {
		return p.reconcile(ctx, spec, before, server,
			backend.NewStartCause(backend.FailureMalformedResponse, err))
	}
	if cause := p.verifyCreated(ctx, spec, created, server); cause != nil {
		return backend.NewRefKnownWithCause(ref, *cause)
	}
	if cause := p.adapter.rootPaneIdle(ctx, created.PaneID); cause != nil {
		return backend.NewRefKnownWithCause(ref, *cause)
	}
	// The idle wait is a real race window: the server, tab, pane, or terminal
	// may have turned over while process-info was polling. Re-read every
	// identity immediately before the only command that types into the pane.
	if cause := p.verifyCreated(ctx, spec, created, server); cause != nil {
		return backend.NewRefKnownWithCause(ref, *cause)
	}
	bootstrapResult, bootstrapErr := p.adapter.run.RunSensitive(ctx, p.adapter.command(exec.KindHerdrBootstrap,
		exec.MustFixed("pane"),
		exec.MustFixed("run"),
		exec.Opaque(created.PaneID),
		spec.Bootstrap().SensitiveArg(),
	))
	if bootstrapErr != nil {
		return backend.NewRefKnownWithCause(ref, p.adapter.classifyRunError(bootstrapErr, bootstrapResult))
	}
	return backend.NewRefKnown(ref)
}

func (p *ProjectTabAdapter) Probe(ctx context.Context, ref backend.Ref) backend.ProbeResult {
	_, state, cause := p.locate(ctx, ref)
	switch state {
	case projectTabFound:
		return backend.NewProbePresent()
	case projectTabAbsent:
		return backend.NewProbeGone()
	case projectTabMismatch:
		return backend.NewProbeIdentityMismatch(cause)
	default:
		return backend.NewProbeUnreadable(cause)
	}
}

func (p *ProjectTabAdapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	child, state, cause := p.locate(ctx, ref)
	switch state {
	case projectTabAbsent:
		return backend.NewCloseAlreadyGone()
	case projectTabMismatch:
		return backend.NewCloseIdentityMismatch(cause)
	case projectTabUnreadable:
		return backend.NewCloseUnreadable(cause)
	case projectTabFound:
	default:
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureInternal, ErrProjectTabIdentity))
	}
	if len(child.tabs) <= 1 {
		return backend.NewCloseIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch,
			fmt.Errorf("%w: closing the last tab would close the project workspace", ErrProjectTabIdentity)))
	}
	if p.adapter.turnover(child.server) != turnoverNone {
		return backend.NewCloseIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch,
			fmt.Errorf("%w: herdr server changed before tab close", ErrProjectTabIdentity)))
	}
	res, runErr := p.adapter.run.RunSensitive(ctx, p.adapter.command(exec.KindHerdrProjectTabCleanup,
		exec.MustFixed("tab"),
		exec.MustFixed("close"),
		exec.Opaque(child.tab.tabID),
	))
	if runErr != nil {
		if errorCode(res.Stderr) == "tab_not_found" || errorCode(res.Stderr) == "workspace_not_found" || p.adapter.serverGone(child.server) {
			return backend.NewCloseAlreadyGone()
		}
		return backend.NewCloseFailed(p.adapter.classifyRunError(runErr, res))
	}
	if p.adapter.turnover(child.server) != turnoverNone {
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureIdentityMismatch,
			fmt.Errorf("%w: herdr server changed while closing the tab", ErrProjectTabIdentity)))
	}
	return backend.NewCloseClosed()
}

func (p *ProjectTabAdapter) Focus(ctx context.Context, ref backend.Ref) error {
	child, state, cause := p.locate(ctx, ref)
	if state != projectTabFound {
		return fmt.Errorf("%w: %v", ErrProjectTabIdentity, cause)
	}
	if child.tab.paneCount != 1 {
		return fmt.Errorf("%w: project tab has extra panes", ErrProjectTabIdentity)
	}
	if p.adapter.turnover(child.server) != turnoverNone {
		return fmt.Errorf("%w: herdr server changed before focus", ErrProjectTabIdentity)
	}
	res, runErr := p.adapter.run.RunSensitive(ctx, p.adapter.command(exec.KindHerdrProjectTabFocus,
		exec.MustFixed("tab"),
		exec.MustFixed("focus"),
		exec.Opaque(child.tab.tabID),
	))
	if runErr != nil {
		return fmt.Errorf("%w: tab focus: %v", ErrProjectTabIdentity, p.adapter.classifyRunError(runErr, res))
	}
	if p.adapter.turnover(child.server) != turnoverNone {
		return fmt.Errorf("%w: herdr server changed while focusing the tab", ErrProjectTabIdentity)
	}
	return nil
}

type projectTabState uint8

const (
	projectTabInvalid projectTabState = iota
	projectTabFound
	projectTabAbsent
	projectTabMismatch
	projectTabUnreadable
)

type projectTabSnapshot struct {
	server serverInfo
	tab    projectTabRow
	tabs   []projectTabRow
	pane   herdr.Pane
}

func (p *ProjectTabAdapter) locate(ctx context.Context, ref backend.Ref) (projectTabSnapshot, projectTabState, backend.StartCause) {
	identity, err := p.validateChild(ref)
	if err != nil {
		return projectTabSnapshot{}, projectTabMismatch, backend.NewStartCause(backend.FailureIdentityMismatch, err)
	}
	server, cause := p.adapter.readiness(ctx)
	if cause != nil {
		return projectTabSnapshot{}, projectTabUnreadable, *cause
	}
	identityCause := p.sameServer(server)
	if identityCause != nil || !ref.Server().Matches(server.incarnation) {
		if identityCause == nil {
			freshCause := backend.NewStartCause(backend.FailureIdentityMismatch,
				fmt.Errorf("%w: child reference is from another server incarnation", ErrProjectTabIdentity))
			identityCause = &freshCause
		}
		return projectTabSnapshot{}, projectTabMismatch, *identityCause
	}
	rows, cause := p.parentTabsAt(ctx, exec.KindHerdrProjectTabProbe)
	if cause != nil {
		return projectTabSnapshot{}, projectTabUnreadable, *cause
	}
	if len(rows) == 0 {
		return projectTabSnapshot{}, projectTabAbsent, backend.StartCause{}
	}
	var matched *projectTabRow
	for i := range rows {
		if projectLabelMatches(rows[i].label, ref.OwnershipName()) {
			if matched != nil || rows[i].tabID != identity.Tab() {
				return projectTabSnapshot{}, projectTabMismatch, backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity)
			}
			matched = &rows[i]
		}
		if rows[i].tabID == identity.Tab() && !projectLabelMatches(rows[i].label, ref.OwnershipName()) {
			return projectTabSnapshot{}, projectTabMismatch, backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity)
		}
	}
	if matched == nil {
		return projectTabSnapshot{}, projectTabAbsent, backend.StartCause{}
	}
	if matched.paneCount != 1 {
		return projectTabSnapshot{}, projectTabMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			fmt.Errorf("%w: project tab has %d panes", ErrProjectTabIdentity, matched.paneCount))
	}
	pane, err := p.adapter.paneRecord(ctx, identity.Pane(), identity.Workspace())
	if err != nil {
		return projectTabSnapshot{}, projectTabUnreadable, backend.NewStartCause(backend.FailureUnavailable, err)
	}
	if pane.TabID != identity.Tab() || pane.TerminalID != identity.Terminal() {
		return projectTabSnapshot{}, projectTabMismatch, backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity)
	}
	return projectTabSnapshot{server: server, tab: *matched, tabs: rows, pane: pane}, projectTabFound, backend.StartCause{}
}

func (p *ProjectTabAdapter) validateChild(ref backend.Ref) (backend.HerdrIdentity, error) {
	if ref.Kind() != backend.KindHerdr || ref.Source() != p.parent.Source() || ref.Server() != p.parent.Server() {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: child reference backend or server differs", ErrProjectTabIdentity)
	}
	parentEndpoint, parentOK := p.parent.Endpoint()
	childEndpoint, childOK := ref.Endpoint()
	if !parentOK || !childOK || !parentEndpoint.Matches(childEndpoint) {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: child reference endpoint differs", ErrProjectTabIdentity)
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		return backend.HerdrIdentity{}, err
	}
	if identity.Workspace() != p.parentID.Workspace() || identity.Tab() == "" || identity.Pane() == "" || identity.Terminal() == "" || ref.OwnershipName() == p.parent.OwnershipName() {
		return backend.HerdrIdentity{}, fmt.Errorf("%w: child reference is incomplete or uses the parent marker", ErrProjectTabIdentity)
	}
	return identity, nil
}

func (p *ProjectTabAdapter) sameServer(server serverInfo) *backend.StartCause {
	endpoint, ok := p.parent.Endpoint()
	if !ok || !endpoint.Matches(backend.EndpointOf(server.socket)) || !p.parent.Server().Matches(server.incarnation) {
		cause := backend.NewStartCause(backend.FailureIdentityMismatch,
			fmt.Errorf("%w: herdr endpoint or incarnation differs from the parent", ErrProjectTabIdentity))
		return &cause
	}
	return nil
}

func (p *ProjectTabAdapter) parentTabs(ctx context.Context, kind exec.CommandKind) ([]projectTabRow, *backend.StartCause) {
	return p.parentTabsAt(ctx, kind)
}

func (p *ProjectTabAdapter) parentTabsAt(ctx context.Context, kind exec.CommandKind) ([]projectTabRow, *backend.StartCause) {
	rows, cause := p.adapter.snapshot(ctx, kind)
	if cause != nil {
		return nil, cause
	}
	parent, ok := rows[p.parentID.Workspace()]
	if !ok {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, fmt.Errorf("%w: parent workspace is absent", ErrProjectTabParent))
		return nil, &c
	}
	if !projectLabelMatches(parent.marker, p.parent.OwnershipName()) {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, fmt.Errorf("%w: parent workspace marker changed", ErrProjectTabParent))
		return nil, &c
	}
	parentMatches := 0
	for id, row := range rows {
		if !projectLabelMatches(row.marker, p.parent.OwnershipName()) {
			continue
		}
		parentMatches++
		if id != p.parentID.Workspace() {
			c := backend.NewStartCause(backend.FailureIdentityMismatch, fmt.Errorf("%w: parent workspace marker is duplicated", ErrProjectTabParent))
			return nil, &c
		}
	}
	if parentMatches != 1 {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, fmt.Errorf("%w: parent workspace marker is duplicated", ErrProjectTabParent))
		return nil, &c
	}
	projectTabs, tabsCause := p.projectTabs(ctx, kind)
	if tabsCause != nil {
		return nil, tabsCause
	}
	parentTabMatches := 0
	for _, row := range projectTabs {
		if row.tabID != p.parentID.Tab() {
			continue
		}
		parentTabMatches++
		if row.workspaceID != p.parentID.Workspace() || row.paneCount != 1 {
			c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabParent)
			return nil, &c
		}
	}
	if parentTabMatches != 1 {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabParent)
		return nil, &c
	}
	parentPane, err := p.adapter.paneRecord(ctx, p.parentID.Pane(), p.parentID.Workspace())
	if err != nil {
		c := backend.NewStartCause(backend.FailureUnavailable, err)
		return nil, &c
	}
	if parentPane.TabID != p.parentID.Tab() || parentPane.TerminalID != p.parentID.Terminal() {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabParent)
		return nil, &c
	}
	return projectTabs, nil
}

func (p *ProjectTabAdapter) projectTabs(ctx context.Context, kind exec.CommandKind) ([]projectTabRow, *backend.StartCause) {
	res, runErr := p.adapter.run.RunSensitive(ctx, p.adapter.command(kind,
		exec.MustFixed("tab"), exec.MustFixed("list"), exec.MustFixed("--workspace"), exec.Opaque(p.parentID.Workspace())))
	if runErr != nil {
		cause := p.adapter.classifyRunError(runErr, res)
		return nil, &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project tab list was truncated"))
		return nil, &cause
	}
	decoded, err := wire.DecodeResult[struct {
		Tabs *[]herdr.Tab `json:"tabs"`
	}](raw)
	if errors.Is(err, wire.ErrNoResult) || decoded.Tabs == nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project tab list had no tabs member"))
		return nil, &cause
	}
	if err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project tab list was not readable JSON"))
		return nil, &cause
	}
	rows := make([]projectTabRow, 0, len(*decoded.Tabs))
	for _, tab := range *decoded.Tabs {
		if tab.WorkspaceID != p.parentID.Workspace() || tab.TabID == "" {
			cause := backend.NewStartCause(backend.FailureMalformedResponse, ErrProjectTabIdentity)
			return nil, &cause
		}
		if _, err := backend.NewHerdrIdentity(tab.WorkspaceID, tab.TabID, ""); err != nil {
			cause := backend.NewStartCause(backend.FailureMalformedResponse, err)
			return nil, &cause
		}
		rows = append(rows, projectTabRow{tabID: tab.TabID, workspaceID: tab.WorkspaceID, label: tab.Label, paneCount: tab.PaneCount})
	}
	return rows, nil
}

type projectTabRow struct {
	tabID       string
	workspaceID string
	label       string
	paneCount   int
}

type projectTabCreated struct {
	WorkspaceID string
	TabID       string
	PaneID      string
	TerminalID  string
}

func parseProjectTabCreated(out exec.BoundedOutput) (projectTabCreated, error) {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return projectTabCreated{}, errors.New("the herdr project tab create reply was truncated")
	}
	decoded, err := wire.DecodeResult[struct {
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
	}](raw)
	if errors.Is(err, wire.ErrNoResult) {
		return projectTabCreated{}, errors.New("the herdr project tab create reply had no result")
	}
	if err != nil {
		return projectTabCreated{}, errors.New("the herdr project tab create reply was not readable JSON")
	}
	created := projectTabCreated{
		WorkspaceID: decoded.Tab.WorkspaceID,
		TabID:       decoded.Tab.TabID,
		PaneID:      decoded.RootPane.PaneID,
		TerminalID:  decoded.RootPane.TerminalID,
	}
	if created.WorkspaceID == "" || created.TabID == "" || created.PaneID == "" || created.TerminalID == "" || decoded.RootPane.WorkspaceID != created.WorkspaceID || decoded.RootPane.TabID != created.TabID {
		return projectTabCreated{}, errors.New("the herdr project tab create reply named no complete matching tab, pane, and terminal")
	}
	if _, err := backend.NewHerdrIdentity(created.WorkspaceID, created.TabID, created.PaneID); err != nil {
		return projectTabCreated{}, err
	}
	return created, nil
}

func (p *ProjectTabAdapter) projectReference(created projectTabCreated, tag backend.RecoveryTag, server serverInfo) (backend.Ref, error) {
	info, err := p.adapter.lstat(server.socket)
	if err != nil {
		return backend.Ref{}, fmt.Errorf("stat herdr socket: %w", err)
	}
	now, err := p.adapter.fingerprint(server.socket, info, server.version)
	if err != nil || !server.incarnation.Matches(now) {
		return backend.Ref{}, errors.New("the herdr server incarnation changed while the project tab was created")
	}
	identity, err := backend.NewHerdrIdentity(created.WorkspaceID, created.TabID, created.PaneID)
	if err != nil {
		return backend.Ref{}, err
	}
	identity, err = identity.WithTerminal(created.TerminalID)
	if err != nil {
		return backend.Ref{}, err
	}
	ref, err := backend.NewHerdrRef(p.adapter.source, now, tag, identity)
	if err != nil {
		return backend.Ref{}, err
	}
	return ref.WithEndpoint(backend.EndpointOf(server.socket))
}

func (p *ProjectTabAdapter) verifyCreated(ctx context.Context, spec backend.StartSpec, created projectTabCreated, server serverInfo) *backend.StartCause {
	rows, cause := p.parentTabsAt(ctx, exec.KindHerdrProjectTabProbe)
	if cause != nil {
		return cause
	}
	found := 0
	for _, row := range rows {
		if projectLabelMatches(row.label, spec.OwnershipName()) {
			found++
			if row.tabID != created.TabID || row.workspaceID != created.WorkspaceID || row.paneCount != 1 {
				c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity)
				return &c
			}
		}
	}
	if found != 1 {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity)
		return &c
	}
	pane, err := p.adapter.paneRecord(ctx, created.PaneID, created.WorkspaceID)
	if err != nil {
		c := backend.NewStartCause(backend.FailureUnavailable, err)
		return &c
	}
	if pane.TabID != created.TabID || pane.TerminalID != created.TerminalID {
		c := backend.NewStartCause(backend.FailureIdentityMismatch, ErrProjectTabIdentity)
		return &c
	}
	fresh, cause := p.adapter.freshReadiness(ctx, server)
	if cause != nil {
		return cause
	}
	return p.sameServer(fresh)
}

func (p *ProjectTabAdapter) reconcile(ctx context.Context, spec backend.StartSpec, before []projectTabRow, server serverInfo, cause backend.StartCause) backend.StartResult {
	fresh, freshCause := p.adapter.freshReadiness(ctx, server)
	if freshCause != nil || p.sameServer(fresh) != nil {
		if freshCause != nil {
			return backend.NewOutcomeUnknown(spec.Tag(), *freshCause)
		}
		return backend.NewOutcomeUnknown(spec.Tag(), *p.sameServer(fresh))
	}
	rows, currentCause := p.parentTabsAt(ctx, exec.KindHerdrProjectTabReconcile)
	if currentCause != nil {
		return backend.NewOutcomeUnknown(spec.Tag(), *currentCause)
	}
	seen := make(map[string]struct{}, len(before))
	for _, row := range before {
		seen[row.tabID] = struct{}{}
	}
	candidates := make([]projectTabRow, 0, 1)
	for _, row := range rows {
		if projectLabelMatches(row.label, spec.OwnershipName()) {
			if _, existed := seen[row.tabID]; !existed {
				candidates = append(candidates, row)
			}
		}
	}
	if len(candidates) != 1 || candidates[0].paneCount != 1 {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	panes, paneCause := p.projectPanes(ctx, exec.KindHerdrProjectTabReconcile)
	if paneCause != nil {
		return backend.NewOutcomeUnknown(spec.Tag(), *paneCause)
	}
	var match *herdr.Pane
	for i := range panes {
		if panes[i].TabID == candidates[0].tabID {
			if match != nil || panes[i].WorkspaceID != p.parentID.Workspace() {
				return backend.NewOutcomeUnknown(spec.Tag(), cause)
			}
			match = &panes[i]
		}
	}
	if match == nil || match.PaneID == "" || match.TerminalID == "" {
		return backend.NewOutcomeUnknown(spec.Tag(), cause)
	}
	created := projectTabCreated{WorkspaceID: match.WorkspaceID, TabID: match.TabID, PaneID: match.PaneID, TerminalID: match.TerminalID}
	ref, err := p.projectReference(created, spec.Tag(), fresh)
	if err != nil {
		return backend.NewOutcomeUnknown(spec.Tag(), backend.NewStartCause(backend.FailureIdentityMismatch, err))
	}
	return backend.NewRefKnownWithCause(ref, cause)
}

func (p *ProjectTabAdapter) projectPanes(ctx context.Context, kind exec.CommandKind) ([]herdr.Pane, *backend.StartCause) {
	res, runErr := p.adapter.run.RunSensitive(ctx, p.adapter.command(kind,
		exec.MustFixed("pane"), exec.MustFixed("list"), exec.MustFixed("--workspace"), exec.Opaque(p.parentID.Workspace())))
	if runErr != nil {
		cause := p.adapter.classifyRunError(runErr, res)
		return nil, &cause
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project pane list was truncated"))
		return nil, &cause
	}
	decoded, err := wire.DecodeResult[struct {
		Panes *[]herdr.Pane `json:"panes"`
	}](raw)
	if errors.Is(err, wire.ErrNoResult) || decoded.Panes == nil || err != nil {
		cause := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("the herdr project pane list was not readable JSON"))
		return nil, &cause
	}
	return *decoded.Panes, nil
}
