// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter_test

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	rexadapterPackage = "github.com/cameronsjo/forgectl/internal/surface/rexadapter"
	launchPackage     = "github.com/cameronsjo/forgectl/internal/launch"
	surfacePackage    = "github.com/cameronsjo/forgectl/internal/surface"
)

// TestRexAdapterCannotSeeTheInvocation keeps the Rex adapter on the adapter
// side of the barrier internal/surface/backend's barrier_test.go draws: it
// must not reach internal/launch by any import path, so it cannot resolve a
// harness invocation for itself. backend's derived check covers every
// package under internal/surface; this one names the Rex adapter, so a move
// out of that tree cannot drop it from the check silently.
//
// The positive control is internal/surface, the coordinator, which does
// import launch: the same query must find that edge, or a clean result here
// proves nothing.
func TestRexAdapterCannotSeeTheInvocation(t *testing.T) {
	if !strings.HasPrefix(rexadapterPackage, surfacePackage+"/") {
		t.Fatalf("%s is outside %s, so backend's derived barrier no longer covers it", rexadapterPackage, surfacePackage)
	}
	if got := deps(t, surfacePackage); !slices.Contains(got, launchPackage) {
		t.Fatalf("go list -deps %s does not list %s; the query is not seeing real edges", surfacePackage, launchPackage)
	}
	if got := deps(t, rexadapterPackage); slices.Contains(got, launchPackage) {
		t.Errorf("%s transitively imports %s; an adapter must not be able to reach a harness invocation", rexadapterPackage, launchPackage)
	}
}

func deps(t *testing.T, pkg string) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", pkg) //nolint:gosec // G204: package paths from constants in this file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v: %s", pkg, err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Fatalf("go list -deps %s returned %d packages; the query did not run", pkg, len(lines))
	}
	return lines
}
