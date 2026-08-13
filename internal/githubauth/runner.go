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

// ErrUnpinnableGhPath is returned when a `gh` command is routed through a
// Runner method that cannot carry the host pin. exec.Runner's stdin and
// interactive modes take no environment, so there is no way to force
// GH_HOST=Host on them — and a gh subprocess that quietly ran without the pin
// is the exact failure this package exists to prevent. Refusing is therefore
// the fail-closed answer. If a stdin-fed or tty-driven gh leg is ever needed
// (real `gh api graphql --input -` pagination is the obvious candidate), the
// fix is an env-carrying variant on exec.Runner, not a bypass here.
var ErrUnpinnableGhPath = errors.New("githubauth: gh cannot be host-pinned on this runner path")

// pinnedRunner wraps an exec.Runner so every `gh` invocation carries
// GH_HOST=Host, and so any cancellation or deadline failure is converted to a
// safe standard sentinel at the subprocess boundary rather than deeper in the
// call stack.
//
// The underlying runner is a NAMED field, deliberately not an embed. An embed
// supplies the interface's remaining methods automatically, so a `gh` call
// written against RunWithInput or RunInteractive — or against any method added
// to exec.Runner later — would reach the subprocess unpinned, with no compile
// error and no test failure. With a named field the compiler refuses the type
// until all four methods are written out, and each one has to state what it
// does with `gh`.
type pinnedRunner struct {
	base exec.Runner
}

// Runner returns run wrapped so that `gh` subprocesses are pinned to
// github.com. Non-gh commands delegate to the underlying runner untouched —
// the pin is a GitHub-identity control, not a general environment override.
func Runner(run exec.Runner) exec.Runner {
	return pinnedRunner{base: run}
}

// Run pins `gh` to Host via RunWithEnv (which merges over the process
// environment, so the supplied GH_HOST beats an ambient one) and normalizes
// context failures. Anything that is not `gh` delegates unchanged.
func (p pinnedRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name != "gh" {
		return p.base.Run(ctx, name, args...)
	}
	out, err := p.base.RunWithEnv(ctx, map[string]string{"GH_HOST": Host}, name, args...)
	if err != nil {
		return out, classifyContextFailure(ctx, err)
	}
	return out, nil
}

// RunWithEnv applies the same pin to the explicit-environment path. Without
// this override the wrapper would be asymmetric: a caller reaching for
// RunWithEnv — the very method used to control a child's environment — would
// silently escape the host pin, and could even supply its own GH_HOST. The
// pin is applied last so it beats any GH_HOST in env.
func (p pinnedRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	if name != "gh" {
		return p.base.RunWithEnv(ctx, env, name, args...)
	}
	pinned := make(map[string]string, len(env)+1)
	for k, v := range env {
		pinned[k] = v
	}
	pinned["GH_HOST"] = Host
	out, err := p.base.RunWithEnv(ctx, pinned, name, args...)
	if err != nil {
		return out, classifyContextFailure(ctx, err)
	}
	return out, nil
}

// RunWithInput refuses `gh` rather than running it unpinned: stdin mode carries
// no environment, so the host pin cannot ride along. Everything else delegates
// untouched — the pin is a GitHub-identity control, and pbcopy has no host.
func (p pinnedRunner) RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	if name == "gh" {
		return "", ErrUnpinnableGhPath
	}
	return p.base.RunWithInput(ctx, stdin, name, args...)
}

// RunInteractive refuses `gh` for the same reason RunWithInput does: the
// interactive mode takes no environment, so an interactive gh would reach the
// tty on whatever host an ambient GH_HOST named. Non-gh commands (tmux attach,
// `sesh connect`) delegate untouched.
func (p pinnedRunner) RunInteractive(ctx context.Context, name string, args ...string) error {
	if name == "gh" {
		return ErrUnpinnableGhPath
	}
	return p.base.RunInteractive(ctx, name, args...)
}

// classifyContextFailure converts a raw subprocess failure into a safe
// standard context sentinel when the failure is really a cancellation or an
// expired deadline, and otherwise returns the raw error for the in-process
// caller to classify categorically.
//
// The replacement is total: when a sentinel wins, the original error is
// dropped whole, including an *exec.CommandError's Stderr and Output fields.
// Nothing of the subprocess's own text survives into the returned error — which
// is the point, since that text is rendered — so no caller may treat the
// returned sentinel as still carrying the child's output.
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
