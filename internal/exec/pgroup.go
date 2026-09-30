package exec

import (
	"context"
	"os/exec"
)

type processGroupKey struct{}

// WithProcessGroup returns a context under which OSRunner starts the child in
// a process group of its own and, when the context ends, kills that whole
// group rather than the child alone, so a child that forked helpers leaves
// none running after a timeout (forgectl#877). It is an opt-in because an
// interactive child (an editor, a pager, sops) needs the terminal's
// foreground group; only a non-interactive child may take it. Off unix it
// changes nothing.
func WithProcessGroup(ctx context.Context) context.Context {
	return context.WithValue(ctx, processGroupKey{}, true)
}

func wantsProcessGroup(ctx context.Context) bool {
	on, _ := ctx.Value(processGroupKey{}).(bool)
	return on
}

// applyProcessGroup configures cmd for WithProcessGroup; a no-op without it.
func applyProcessGroup(ctx context.Context, cmd *exec.Cmd) {
	if wantsProcessGroup(ctx) {
		setProcessGroup(cmd)
	}
}
