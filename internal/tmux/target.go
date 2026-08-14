package tmux

// There is deliberately no general ExactTarget(string) here, and its absence is
// the design.
//
// tmux's `-t` grammar is per-command. `kill-session` takes a target-session,
// `select-window` takes a target-window, `new-window` takes a target-WINDOW
// even when all you want to name is a session — and that last one is where
// forgectl#237 lives. Measured on tmux 3.7b against an isolated socket holding
// only a session named `forgectl-review`:
//
//	new-window -t forgectl     → exit 0, window created in forgectl-review
//	new-window -t forgectl:    → exit 0, window created in forgectl-review
//	new-window -t =forgectl    → exit 0, window created in forgectl-review
//	new-window -t =forgectl:   → exit 1, "can't find session: forgectl"
//
// So the exact-match modifier `=` alone does NOT pin a session-only
// destination for new-window: without the delimiter the operand is still read
// as a window target and falls back to prefix resolution. Only the trailing
// colon makes it session-qualified. One helper that emitted "the exact target"
// for every command would get this wrong for at least one of them, so each
// command's destination is built by a function that knows which command it is
// for.

// NewWindowSessionTarget renders a `new-window -t` destination naming a session
// by its native id.
//
// The trailing colon is load-bearing (see above) and is asserted by
// TestNewWindowSessionTargetKeepsTrailingColon plus the isolated real-tmux
// integration test — remove it and both fail.
//
// Passing a native id rather than a name means the exact-match `=` modifier is
// not needed at all: tmux resolves `$N` by identity, with no name, glob, or
// prefix step to fall through.
func NewWindowSessionTarget(sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return sessionID + ":", nil
}
