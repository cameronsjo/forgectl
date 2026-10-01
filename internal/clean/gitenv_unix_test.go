//go:build unix

package clean

import (
	"context"
	"testing"

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
