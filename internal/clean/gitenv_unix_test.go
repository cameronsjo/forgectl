//go:build unix

package clean

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// TestGitDirtyNeverLazyFetches is TestGitStatusNeverLazyFetches
// (internal/projects) for clean's dirty check (cameronsjo/forgectl#944). The
// refused fetch fails the check, which clean treats as dirty.
//
// Mutation: run `git status` in gitDirty through run.Run: the canary runs.
func TestGitDirtyNeverLazyFetches(t *testing.T) {
	gitenvtest.WithoutLazyFetchPin(t)
	gitenvtest.NewCanary(t).AssertLive(t)
	canary := gitenvtest.NewCanary(t)

	_, err := gitDirty(context.Background(), exec.OSRunner{}, canary.Dir)
	if canary.Ran(t) {
		t.Fatal("clean's dirty check ran the repository's transport command through a lazy fetch")
	}
	if err == nil {
		t.Error("gitDirty succeeded in a repository whose HEAD it cannot read")
	}
}

// clean's dirty check re-hashes stat-dirty files through the filter drivers
// the repository names; neither the clean nor the process filter runs (#977).
// Mutation: run gitDirty's status through gitenv.Run under Local instead of
// gitenv.RunUnfiltered: the canary runs.
func TestGitDirtyRunsNoFilterDriver(t *testing.T) {
	gitenvtest.NewFilterCanary(t).AssertLive(t)
	canary := gitenvtest.NewFilterCanary(t)

	dirty, err := gitDirty(context.Background(), exec.OSRunner{}, canary.Dir)
	if canary.Ran(t) {
		t.Fatal("clean's dirty check ran a filter driver the repository defines")
	}
	if err != nil || dirty {
		t.Errorf("gitDirty = %v, %v; want a clean tree: the files match their blobs", dirty, err)
	}
}

// #1005: a project whose HEAD is a FIFO, on which git itself blocks, is
// skipped at once as unknown, not reported dirty and never reclaimed.
// Mutations: drop gitenv's HEAD mode check (git blocks until the deadline,
// and the 10-second bound fails the test); give the failed check
// dirtyTreeSkipReason (the reason claims a dirty tree).
func TestApplyReportSkipsAFIFOHeadProjectAsUnknown(t *testing.T) {
	dir := gitenvtest.FIFOHeadRepo(t)
	target := filepath.Join(dir, "node_modules")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	report := Report{Targets: []Target{{Path: target, Kind: KindNode, ProjectRoot: dir}}}
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := New(exec.OSRunner{}).ApplyReport(t.Context(), dir, report, CleanOptions{Apply: true})
		done <- outcome{res, err}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ApplyReport did not return: git is blocked on the FIFO HEAD")
	}
	if got.err != nil {
		t.Fatalf("ApplyReport: %v", got.err)
	}
	if len(got.res.Items) != 1 || !got.res.Items[0].Skipped || got.res.Items[0].SkipReason != unknownTreeSkipReason {
		t.Errorf("items = %+v, want one skipped with %q", got.res.Items, unknownTreeSkipReason)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the target was reclaimed: %v", err)
	}
}
