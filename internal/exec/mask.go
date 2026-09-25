package exec

import (
	"context"
	"strings"
)

// minScrubLen is the shortest bare value scrubbed out of stderr. The whole
// KEY=VALUE entry is always scrubbed; a bare value shorter than this is not,
// because replacing a "1" or a "grpc" everywhere in a tmux error would mangle
// it without protecting anything worth protecting.
const minScrubLen = 8

type maskKey struct{}

// argMask is what WithMaskedAssignments stores: each marked KEY=VALUE argv
// element mapped to its display form, plus the bare values to scrub from
// stderr.
type argMask struct {
	shown  map[string]string
	values []string
}

// WithMaskedAssignments returns a context under which the Runner never
// renders the VALUE of any of these KEY=VALUE argv elements. It covers the
// three places a Runner writes argv down: the debug log, *CommandError's text,
// and the stderr it retains and logs. Each shows as KEY=[redacted].
//
// The child still receives the real value. What this does NOT cover is the
// process table: argv is readable through ps for the life of the process, so
// this is "will not be written down", not "safe for a secret" — the same
// limit the sensitive seam states. It exists for callers such as tmux
// new-window -e, whose values belong on argv but not in a log file (#529).
func WithMaskedAssignments(ctx context.Context, entries []string) context.Context {
	if len(entries) == 0 {
		return ctx
	}
	m := argMask{shown: make(map[string]string, len(entries))}
	for _, e := range entries {
		key, value, ok := strings.Cut(e, "=")
		if !ok || value == "" {
			continue
		}
		m.shown[e] = key + "=" + Redacted
		if len(value) >= minScrubLen {
			m.values = append(m.values, value)
		}
	}
	return context.WithValue(ctx, maskKey{}, m)
}

func maskFrom(ctx context.Context) argMask {
	m, _ := ctx.Value(maskKey{}).(argMask)
	return m
}

// args returns argv as it may be rendered. It copies only when something is
// masked, so the unmasked path renders exactly as before.
func (m argMask) args(args []string) []string {
	if len(m.shown) == 0 {
		return args
	}
	out := make([]string, len(args))
	for i, a := range args {
		if s, ok := m.shown[a]; ok {
			a = s
		}
		out[i] = a
	}
	return out
}

// text scrubs whole entries first, then bare values, so an echoed entry keeps
// its key: KEY=[redacted] rather than a bare [redacted].
func (m argMask) text(s string) string {
	for entry, shown := range m.shown {
		s = strings.ReplaceAll(s, entry, shown)
	}
	for _, v := range m.values {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	return s
}
