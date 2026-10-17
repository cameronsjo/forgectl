package herdradapter

import (
	"context"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

const projectToken = "forgectl_project"

// ShowProjectWorkspace applies the cosmetic project name to the verified
// project workspace and its root pane. The ownership label is deliberately
// untouched: it remains the durable marker used by every project operation.
// Metadata is best effort for callers, but this method still refuses to issue
// a write when any part of the project identity cannot be re-proven.
func (adapter *Adapter) ShowProjectWorkspace(ctx context.Context, ref backend.Ref, projectName string) error {
	if err := CheckTokenValue(projectToken, projectName); err != nil {
		return err
	}

	identity, _, err := adapter.verifyProjectMetadataTarget(ctx, ref)
	if err != nil {
		return err
	}
	res, runErr := adapter.run.RunSensitive(ctx, adapter.command(exec.KindHerdrMetadata,
		exec.MustFixed("workspace"),
		exec.MustFixed("report-metadata"),
		exec.Opaque(identity.Workspace()),
		exec.MustFixed("--source"),
		exec.MustFixed(MetadataSource),
		exec.MustFixed("--token"),
		exec.Opaque(projectToken+"="+projectName),
	))
	if runErr != nil {
		return fmt.Errorf("%w: workspace report-metadata: %v", ErrMetadataFailed, adapter.classifyRunError(runErr, res))
	}

	identity, _, err = adapter.verifyProjectMetadataTarget(ctx, ref)
	if err != nil {
		return err
	}
	res, runErr = adapter.run.RunSensitive(ctx, adapter.command(exec.KindHerdrMetadata,
		exec.MustFixed("pane"),
		exec.MustFixed("report-metadata"),
		exec.Opaque(identity.Pane()),
		exec.MustFixed("--source"),
		exec.MustFixed(MetadataSource),
		exec.MustFixed("--display-agent"),
		exec.Opaque(projectName),
	))
	if runErr != nil {
		return fmt.Errorf("%w: pane report-metadata: %v", ErrMetadataFailed, adapter.classifyRunError(runErr, res))
	}
	return nil
}

// ShowProjectThread applies only the thread's display name to the child tab's
// root pane. It never changes the child tab label, which is the exact marker
// used to prove that the requested tab belongs to this project.
func (projectTab *ProjectTabAdapter) ShowProjectThread(ctx context.Context, childRef backend.Ref, threadName string) error {
	if err := CheckTokenValue("display_agent", threadName); err != nil {
		return err
	}

	child, err := projectTab.verifyProjectTabMetadataTarget(ctx, childRef)
	if err != nil {
		return err
	}
	res, runErr := projectTab.adapter.run.RunSensitive(ctx, projectTab.adapter.command(exec.KindHerdrMetadata,
		exec.MustFixed("pane"),
		exec.MustFixed("report-metadata"),
		exec.Opaque(child.pane.PaneID),
		exec.MustFixed("--source"),
		exec.MustFixed(MetadataSource),
		exec.MustFixed("--display-agent"),
		exec.Opaque(threadName),
	))
	if runErr != nil {
		return fmt.Errorf("%w: pane report-metadata: %v", ErrMetadataFailed, projectTab.adapter.classifyRunError(runErr, res))
	}
	return nil
}

// verifyProjectMetadataTarget performs the same complete project identity
// proof as focus, with an additional final probe after the fresh server read.
// The second proof matters because metadata is cosmetic but still writes to a
// pane named by a durable reference; a changed marker, tab, pane, terminal,
// or server must refuse before the write.
func (adapter *Adapter) verifyProjectMetadataTarget(ctx context.Context, ref backend.Ref) (backend.HerdrIdentity, serverInfo, error) {
	identity, err := adapter.validateProjectWorkspaceRef(ref)
	if err != nil {
		return backend.HerdrIdentity{}, serverInfo{}, err
	}
	server, cause := adapter.readiness(ctx)
	if cause != nil {
		return backend.HerdrIdentity{}, serverInfo{}, fmt.Errorf("%w: %v", ErrProjectWorkspace, cause)
	}
	endpoint, ok := ref.Endpoint()
	if !ok || !ref.Server().Matches(server.incarnation) || !endpoint.Matches(backend.EndpointOf(server.socket)) {
		return backend.HerdrIdentity{}, serverInfo{}, fmt.Errorf("%w: server incarnation or endpoint changed", ErrProjectWorkspace)
	}
	if cause := adapter.verifyProjectWorkspaceRef(ctx, ref, identity, server); cause != nil {
		return backend.HerdrIdentity{}, serverInfo{}, fmt.Errorf("%w: %v", ErrProjectWorkspace, cause)
	}
	fresh, freshCause := adapter.freshReadiness(ctx, server)
	if freshCause != nil {
		return backend.HerdrIdentity{}, serverInfo{}, fmt.Errorf("%w: %v", ErrProjectWorkspace, freshCause)
	}
	if !ref.Server().Matches(fresh.incarnation) || !endpoint.Matches(backend.EndpointOf(fresh.socket)) || adapter.turnover(fresh) != turnoverNone {
		return backend.HerdrIdentity{}, serverInfo{}, fmt.Errorf("%w: server incarnation or endpoint changed", ErrProjectWorkspace)
	}
	if cause := adapter.verifyProjectWorkspaceRef(ctx, ref, identity, fresh); cause != nil {
		return backend.HerdrIdentity{}, serverInfo{}, fmt.Errorf("%w: %v", ErrProjectWorkspace, cause)
	}
	return identity, fresh, nil
}

func (projectTab *ProjectTabAdapter) verifyProjectTabMetadataTarget(ctx context.Context, ref backend.Ref) (projectTabSnapshot, error) {
	child, state, cause := projectTab.locate(ctx, ref)
	if state != projectTabFound {
		return projectTabSnapshot{}, fmt.Errorf("%w: %v", ErrProjectTabIdentity, cause)
	}
	fresh, freshCause := projectTab.adapter.freshReadiness(ctx, child.server)
	if freshCause != nil {
		return projectTabSnapshot{}, fmt.Errorf("%w: %v", ErrProjectTabIdentity, freshCause)
	}
	if projectTab.sameServer(fresh) != nil || projectTab.adapter.turnover(fresh) != turnoverNone {
		return projectTabSnapshot{}, fmt.Errorf("%w: herdr server changed before metadata", ErrProjectTabIdentity)
	}
	child, state, cause = projectTab.locate(ctx, ref)
	if state != projectTabFound {
		return projectTabSnapshot{}, fmt.Errorf("%w: %v", ErrProjectTabIdentity, cause)
	}
	if !fresh.incarnation.Matches(child.server.incarnation) || !backend.EndpointOf(fresh.socket).Matches(backend.EndpointOf(child.server.socket)) {
		return projectTabSnapshot{}, fmt.Errorf("%w: herdr server changed before metadata", ErrProjectTabIdentity)
	}
	if projectTab.adapter.turnover(child.server) != turnoverNone {
		return projectTabSnapshot{}, fmt.Errorf("%w: herdr server changed before metadata", ErrProjectTabIdentity)
	}
	return child, nil
}
