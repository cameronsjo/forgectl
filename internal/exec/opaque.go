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
// *CommandError's Args and text. The child still receives the real argv.
func WithOpaqueArgs(ctx context.Context, from, n int) context.Context {
	if from < 0 || n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, opaqueKey{}, opaqueSpan{from: from, n: n})
}

// shownArgs is argv as the Runner may render it and keep on a
// *CommandError: masked assignments (WithMaskedAssignments) replaced, and a
// user span (WithOpaqueArgs) reduced to flag names. It copies only when
// something changes.
func shownArgs(ctx context.Context, args []string) []string {
	shown := maskFrom(ctx).args(args)
	span, ok := ctx.Value(opaqueKey{}).(opaqueSpan)
	if !ok || span.from >= len(shown) {
		return shown
	}
	end := len(shown)
	if span.n < len(shown)-span.from {
		end = span.from + span.n
	}
	out := make([]string, 0, len(shown))
	out = append(out, shown[:span.from]...)
	out = append(out, redact.UserArgs(shown[span.from:end])...)
	return append(out, shown[end:]...)
}
