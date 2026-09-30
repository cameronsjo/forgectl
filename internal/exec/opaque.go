package exec

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/redact"
)

type opaqueKey struct{}

// opaqueSpan is the run of argv elements, args[from:from+n], that a user
// wrote rather than forgectl.
type opaqueSpan struct{ from, n int }

// WithOpaqueArgs returns a context under which the Runner renders
// args[from:from+n] (argv after the command name) as flag names only,
// through redact.UserArgs: every user-written value and positional shows as
// [user-arg] (#749). It is for the sites that pass argv a user wrote through
// to a command (a workflow run step, docker's pass-through arguments), where
// no rule can tell which element is a credential. The elements forgectl
// built itself still render through redact.Args. It covers every place a
// Runner writes argv down: the debug log, the interactive debug log, and
// *CommandError's Args and text. It also scrubs the withheld values from
// the stderr and failure-path stdout a Runner captures (maskFor, #782),
// under WithMaskedAssignments's rules. The child still receives the real
// argv.
func WithOpaqueArgs(ctx context.Context, from, n int) context.Context {
	if from < 0 || n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, opaqueKey{}, opaqueSpan{from: from, n: n})
}

// spanFor is ctx's user span clamped to an argv of argc elements, or the
// zero span (n == 0) when there is none or it lies past the end.
func spanFor(ctx context.Context, argc int) opaqueSpan {
	span, ok := ctx.Value(opaqueKey{}).(opaqueSpan)
	if !ok || span.from >= argc {
		return opaqueSpan{}
	}
	return opaqueSpan{from: span.from, n: min(span.n, argc-span.from)}
}

// shownArgs is argv as the Runner may render it and keep on a
// *CommandError: masked assignments (WithMaskedAssignments) replaced, and a
// user span (WithOpaqueArgs) reduced to flag names. It copies only when
// something changes.
func shownArgs(ctx context.Context, args []string) []string {
	shown := maskFrom(ctx).args(args)
	span := spanFor(ctx, len(shown))
	if span.n == 0 {
		return shown
	}
	end := span.from + span.n
	out := make([]string, 0, len(shown))
	out = append(out, shown[:span.from]...)
	out = append(out, redact.UserArgs(shown[span.from:end])...)
	return append(out, shown[end:]...)
}

// maskFor is the scrub the Runner applies to captured stderr and
// failure-path stdout: WithMaskedAssignments's entries and values, plus every
// value the user span's rendering withholds (redact.UserArgValues). Without
// the second part a child that echoes its own argument (a usage error quoting
// it, curl -v printing its -u) would put back into CommandError.Stderr, and
// so into Error() and the failure log, exactly what the argv rendering
// withheld (#782). The cost is diagnostic: a word of the user's argv that
// the child's stderr also uses reads as [redacted] there, under the same
// whole-word rule as a masked value.
func maskFor(ctx context.Context, args []string) argMask {
	m := maskFrom(ctx)
	span := spanFor(ctx, len(args))
	if span.n == 0 {
		return m
	}
	return m.withValues(redact.UserArgValues(args[span.from : span.from+span.n]))
}

// renderArgs is shown (already through shownArgs) as text may carry it:
// redact.Args over the parts before, inside, and after span separately
// (#782). One pass over the whole argv let a user span that ends in a
// credential-bearing flag name (docker build … --secret) withhold the
// forgectl-built elements after it, losing the diagnostic for nothing: the
// span's values are already withheld. Each part still gets every Args rule.
func renderArgs(shown []string, span opaqueSpan) []string {
	if span.n <= 0 || span.from >= len(shown) {
		return redact.Args(shown)
	}
	end := min(span.from+span.n, len(shown))
	out := make([]string, 0, len(shown))
	out = append(out, redact.Args(shown[:span.from])...)
	out = append(out, redact.Args(shown[span.from:end])...)
	return append(out, redact.Args(shown[end:])...)
}
