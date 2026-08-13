// Package githubauth pins forgectl's GitHub inventory subprocesses to
// github.com and resolves which account(s) those subprocesses enumerate.
//
// Two responsibilities, deliberately together: the owner set and the host are
// one identity. Resolving "whose repos" while letting an ambient GH_HOST point
// the query at a GitHub Enterprise instance would stamp enterprise rows as
// github.com data — so the resolver and the host pin share a package and every
// GitHub inventory call goes through Runner.
package githubauth

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Host is the only GitHub host forgectl's inventory talks to. GitHub
// Enterprise is explicitly not supported: an ambient GH_HOST selection is
// overridden rather than queried and mislabeled.
const Host = "github.com"

// pinnedRunner wraps an exec.Runner so every `gh` invocation routed through
// Run carries GH_HOST=github.com, and so any cancellation or deadline failure
// is converted to a safe standard sentinel at the subprocess boundary rather
// than deeper in the call stack.
//
// The embedded Runner supplies RunInteractive/RunWithInput/RunWithEnv
// unchanged. Only Run is overridden, because Run is the method every GitHub
// inventory path (including the shared pr.SearchPRs leg, which accepts a full
// exec.Runner but calls only Run) actually uses.
type pinnedRunner struct {
	exec.Runner
}

// Runner returns run wrapped so that `gh` subprocesses are pinned to
// github.com. Non-gh commands delegate to the underlying runner untouched —
// the pin is a GitHub-identity control, not a general environment override.
func Runner(run exec.Runner) exec.Runner {
	return pinnedRunner{Runner: run}
}

// Run pins `gh` to Host via RunWithEnv (which merges over the process
// environment, so the supplied GH_HOST beats an ambient one) and normalizes
// context failures. Anything that is not `gh` delegates unchanged.
func (p pinnedRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name != "gh" {
		return p.Runner.Run(ctx, name, args...)
	}
	out, err := p.Runner.RunWithEnv(ctx, map[string]string{"GH_HOST": Host}, name, args...)
	if err != nil {
		return out, classifyContextFailure(ctx, err)
	}
	return out, nil
}

// classifyContextFailure converts a raw subprocess failure into a safe
// standard context sentinel when the failure is really a cancellation or an
// expired deadline, and otherwise returns the raw error for the in-process
// caller to classify categorically.
//
// ctx.Err() is consulted FIRST and wins. os/exec commonly reports a killed
// child as an *os/exec.ExitError ("signal: killed") rather than as
// context.Canceled — Cmd.Wait prefers the process exit error over its
// watcher's context error — so errors.Is on the raw error alone is not a
// production guarantee. Checking later, during result folding, is also wrong:
// a cancellation arriving after an ordinary failure would misclassify it.
func classifyContextFailure(ctx context.Context, rawErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			return context.DeadlineExceeded
		case errors.Is(ctxErr, context.Canceled):
			return context.Canceled
		}
	}
	if errors.Is(rawErr, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(rawErr, context.Canceled) {
		return context.Canceled
	}
	return rawErr
}

// SafeContextSentinel reports whether err is one of the two standard context
// sentinels the pinned runner converts to — the signal to callers that the
// error is already safe to join into a public aggregate, as opposed to a raw
// runner cause that must be reduced to a categorical note first.
func SafeContextSentinel(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
