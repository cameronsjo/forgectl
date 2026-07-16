package cli

import "github.com/cameronsjo/forgectl/internal/module"

// allModules is the explicit module registry (ADR-0005): one Manifest per
// command group, aggregated here rather than self-registered via init(), so
// the roster is deterministic, greppable, and a constructor drift is a
// compile error. Manifest instances live next to their command files
// (netModule in net.go, yModule in y.go, …).
//
// Growth policy: a new module enters as module.TierExtension and edits the
// completeness pins in modules_test.go in the same diff — that test is the
// gate, this comment is just the pointer (ADR-0005 §Tier policy).
func allModules() []module.Manifest {
	return []module.Manifest{
		tmuxModule,
		projectsModule,
		configModule,
		launchModule,
		workflowModule(),
		prModule,
		netModule,
		yModule,
		pipModule,
		branchModule,
		cleanModule,
		dockerModule,
		benchModule,
		sessionsModule,
		reviewModule,
		quarantineModule,
		envModule,
		docsModule,
	}
}
