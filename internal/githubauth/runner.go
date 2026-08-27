// Package githubauth pins forgectl's GitHub inventory subprocesses to one
// deployment-configured GitHub host and resolves which account(s) those
// subprocesses enumerate.
//
// Two responsibilities, deliberately together: the owner set and the host are
// one identity. Resolving "whose repos" while letting an ambient GH_HOST point
// the query at a different GitHub instance would stamp rows as data from a
// host nobody chose — so the resolver and the host pin share a package and
// every GitHub inventory call goes through Runner. The pin is total on this
// path: the host may be configured per deployment ([github] host), but every
// gh subprocess still has GH_HOST force-set to that validated value, and an
// ambient GH_HOST never wins.
package githubauth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// DefaultHost is the GitHub host forgectl's inventory talks to when no
// [github] host is configured. A configured host replaces the value but not
// the mechanism: GH_HOST is still force-set on every gh subprocess, so an
// ambient GH_HOST selection is overridden rather than queried and mislabeled.
const DefaultHost = "github.com"

// MaxHostBytes caps a configured host value's length. Real hostnames are far
// shorter; the bound exists so a hostile config value cannot become an
// oversized env component (mirrors MaxOwnerBytes for argv).
const MaxHostBytes = 256

// reHost is the anchored hostname allowlist for a configured GitHub host:
// lowercase DNS labels only — no port, no scheme, no path. Excluding ':' and
// '/' is store-key integrity, not pedantry: Item.Key() embeds the host
// verbatim, and remote-URL matching compares host prefixes, so a host that
// could carry a port or path would fork the key space and the URL comparisons.
// This is deliberately stricter than review's reGiteaHost (which allows a
// port); the user-visible consequence is that a GitHub Enterprise host served
// on a nonstandard port is unconfigurable.
var reHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// ErrUnpinnableGhPath is returned when a `gh` command is routed through a
// Runner method that cannot carry the host pin. exec.Runner's stdin and
// interactive modes take no environment, so there is no way to force
// GH_HOST on them — and a gh subprocess that quietly ran without the pin
// is the exact failure this package exists to prevent. Refusing is therefore
// the fail-closed answer. If a stdin-fed or tty-driven gh leg is ever needed
// (real `gh api graphql --input -` pagination is the obvious candidate), the
// fix is an env-carrying variant on exec.Runner, not a bypass here.
var ErrUnpinnableGhPath = errors.New("githubauth: gh cannot be host-pinned on this runner path")

// ErrUnpinnableHost is returned by every gh path of a Runner constructed with
// a host that failed validation. A gh call that quietly ran pinned to a
// malformed or hostile host would be as wrong as one that ran unpinned, so
// the runner fails closed instead (mirrors ErrUnpinnableGhPath). The message
// is categorical by design: the rejected value is never rendered.
var ErrUnpinnableHost = errors.New("githubauth: configured github host failed validation; refusing to run gh")

// ResolveHost normalizes and validates a configured GitHub host. An empty or
// absent value resolves to DefaultHost; anything else is trimmed, lowercased
// (hostnames are case-insensitive, and the store keys embedding the host must
// have one spelling), byte-bounded, and matched against the anchored hostname
// allowlist. Errors are categorical: the unvalidated value is never rendered,
// because a hostile config line must not reach a terminal or a log via its
// own rejection.
func ResolveHost(configured string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(configured))
	if host == "" {
		return DefaultHost, nil
	}
	if len(host) > MaxHostBytes {
		return "", fmt.Errorf("configured github host is longer than %d bytes", MaxHostBytes)
	}
	if !reHost.MatchString(host) {
		return "", errors.New("configured github host is outside the allowed hostname charset (lowercase dns name, no port, no scheme)")
	}
	return host, nil
}

// tokenEnvVars are the credential variables gh consults for a host. gh sends
// GH_ENTERPRISE_TOKEN / GITHUB_ENTERPRISE_TOKEN to whatever non-default
// GH_HOST names, and GH_TOKEN / GITHUB_TOKEN to github.com and *.ghe.com —
// so once the pin's value is config-steerable, an ambient token plus one
// hostile config line becomes a credential-redirect primitive. On any
// non-default host the pinned runner scrubs all four (set empty, which gh
// treats as unset), forcing gh to the hosts.yml credential stored for that
// host by `gh auth login --hostname <host>`.
var tokenEnvVars = [4]string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// pinnedRunner wraps an exec.Runner so every `gh` invocation carries
// GH_HOST=host, and so any cancellation or deadline failure is converted to a
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
	// host is the validated pin value; empty means construction was handed an
	// invalid host and every gh path must refuse with ErrUnpinnableHost.
	host string
}

// Runner returns run wrapped so that `gh` subprocesses are pinned to host
// (resolved through ResolveHost, so "" means DefaultHost). Non-gh commands
// delegate to the underlying runner untouched — the pin is a GitHub-identity
// control, not a general environment override. A host that fails validation
// yields a fail-closed runner: every gh path returns ErrUnpinnableHost before
// any process is spawned.
func Runner(run exec.Runner, host string) exec.Runner {
	resolved, err := ResolveHost(host)
	if err != nil {
		return pinnedRunner{base: run, host: ""}
	}
	return pinnedRunner{base: run, host: resolved}
}

// pinEnv copies env (nil allowed) and applies the pin: on a non-default host
// the four token variables are scrubbed first, and GH_HOST is set LAST so it
// beats any caller-supplied value.
func (p pinnedRunner) pinEnv(env map[string]string) map[string]string {
	pinned := make(map[string]string, len(env)+len(tokenEnvVars)+1)
	for k, v := range env {
		pinned[k] = v
	}
	if p.host != DefaultHost {
		for _, k := range tokenEnvVars {
			pinned[k] = ""
		}
	}
	pinned["GH_HOST"] = p.host
	return pinned
}

// Run pins `gh` to the configured host via RunWithEnv (which merges over the
// process environment, so the supplied GH_HOST beats an ambient one) and
// normalizes context failures. Anything that is not `gh` delegates unchanged.
func (p pinnedRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name != "gh" {
		return p.base.Run(ctx, name, args...)
	}
	if p.host == "" {
		return "", ErrUnpinnableHost
	}
	out, err := p.base.RunWithEnv(ctx, p.pinEnv(nil), name, args...)
	if err != nil {
		return out, classifyContextFailure(ctx, err)
	}
	return out, nil
}

// RunWithEnv applies the same pin to the explicit-environment path. Without
// this override the wrapper would be asymmetric: a caller reaching for
// RunWithEnv — the very method used to control a child's environment — would
// silently escape the host pin, and could even supply its own GH_HOST or
// token variables. The pin (and, on a non-default host, the token scrub) is
// applied last so it beats anything in env.
func (p pinnedRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	if name != "gh" {
		return p.base.RunWithEnv(ctx, env, name, args...)
	}
	if p.host == "" {
		return "", ErrUnpinnableHost
	}
	out, err := p.base.RunWithEnv(ctx, p.pinEnv(env), name, args...)
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
		if p.host == "" {
			return "", ErrUnpinnableHost
		}
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
		if p.host == "" {
			return ErrUnpinnableHost
		}
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
