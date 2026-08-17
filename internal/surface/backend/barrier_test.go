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

// adapterSidePackages returns every package on the adapter side of the
// boundary: everything under internal/surface except the coordinator itself,
// which legitimately owns the invocation and the binary-provenance policy.
//
// The list is derived rather than written down. A literal slice would let a
// Phase 5 backend package that imports internal/launch pass the barrier by not
// being listed — no failure, no signal — which turns the package's
// load-bearing assertion into a convention someone has to remember.
func adapterSidePackages(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", modulePath+"/internal/surface/...").Output()
	if err != nil {
		t.Fatalf("go list internal/surface/...: %v", err)
	}
	var pkgs []string
	for _, pkg := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pkg != "" && pkg != surfacePackage {
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs
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
	pkgs := adapterSidePackages(t)
	if len(pkgs) < 2 {
		t.Fatalf("derived %d adapter-side packages, want at least the contract and one "+
			"adapter; the derivation is not finding them", len(pkgs))
	}
	for _, pkg := range pkgs {
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

	// The derivation must find the contract and the fake, and must exclude the
	// coordinator — otherwise the barrier either covers nothing or fails on a
	// package that is supposed to import launch.
	derived := adapterSidePackages(t)
	if !slices.Contains(derived, backendPackage) {
		t.Errorf("the derived adapter-side list omits %s", backendPackage)
	}
	if !slices.Contains(derived, modulePath+"/internal/surface/fake") {
		t.Error("the derived adapter-side list omits the fake adapter")
	}
	if slices.Contains(derived, surfacePackage) {
		t.Errorf("the derived list includes the coordinator %s, which owns the "+
			"invocation and must not be held to the barrier", surfacePackage)
	}
}
