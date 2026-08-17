package backend_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	modulePath     = "github.com/cameronsjo/forgectl"
	launchPackage  = modulePath + "/internal/launch"
	execPackage    = modulePath + "/internal/exec"
	backendPackage = modulePath + "/internal/surface/backend"
	surfacePackage = modulePath + "/internal/surface"
)

// adapterSidePackages is every package an adapter may see: the contract itself
// and each adapter implementation. New backends (forgectl#332) join this list,
// and the barrier then covers them with no further wiring.
var adapterSidePackages = []string{
	backendPackage,
	modulePath + "/internal/surface/fake",
}

// deps returns the full transitive dependency list of a package.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	listed := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(listed) < 2 {
		t.Fatalf("go list -deps %s returned %d packages; the query did not run", pkg, len(listed))
	}
	return listed
}

// TestAdapterPackagesCannotSeeTheInvocation is the architecture's load-bearing
// assertion.
//
// An adapter receives a StartSpec, which by construction cannot hold a
// launch.Invocation. That stops an adapter from being handed one; it does not
// stop an adapter package from importing internal/launch and resolving one for
// itself. The barrier that cannot erode is the dependency graph: if
// internal/launch is absent from an adapter's transitive closure, an
// invocation is unreachable from it by any route, including one a future
// author adds without noticing what it costs.
//
// This is also why the contract lives in its own package. When the admission
// policy — which legitimately needs launch.BinarySource — shared a package
// with these types, every adapter inherited the dependency and this assertion
// could not be satisfied at all.
func TestAdapterPackagesCannotSeeTheInvocation(t *testing.T) {
	for _, pkg := range adapterSidePackages {
		t.Run(pkg, func(t *testing.T) {
			if got := deps(t, pkg); slices.Contains(got, launchPackage) {
				t.Errorf("%s transitively imports %s; nothing on the adapter side "+
					"may be able to reach a harness invocation", pkg, launchPackage)
			}
		})
	}
}

// TestBarrierWouldDetectAnImport is the anti-vacuity control.
//
// Without it the barrier above passes identically when `go list` returns
// nothing useful, when a package-path constant is wrong, or when the adapter
// list is empty — three ways to get a confident green from a check that never
// evaluated anything. This proves the same query finds an edge that does
// exist, and that the predicate fires on the one package that is supposed to
// trip it.
func TestBarrierWouldDetectAnImport(t *testing.T) {
	if !slices.Contains(deps(t, backendPackage), execPackage) {
		t.Errorf("go list -deps %s does not list %s, which it imports directly; "+
			"the dependency query is not seeing real edges", backendPackage, execPackage)
	}

	// internal/surface legitimately imports launch — the coordinator owns the
	// invocation and the binary-provenance policy. That makes it the positive
	// control: the predicate the adapter side is held to must fire here, or it
	// has not been shown to fire on anything.
	if !slices.Contains(deps(t, surfacePackage), launchPackage) {
		t.Errorf("%s does not list %s as a dependency; the barrier predicate "+
			"cannot be shown to fire on anything", surfacePackage, launchPackage)
	}

	if len(adapterSidePackages) < 2 {
		t.Errorf("adapterSidePackages has %d entries; the barrier covers the "+
			"contract and at least one adapter", len(adapterSidePackages))
	}
}
