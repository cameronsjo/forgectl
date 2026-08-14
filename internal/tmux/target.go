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
// Every row above is a NAME operand — that is the spelling the grammar applies
// to. So the exact-match modifier `=` alone does NOT pin a session-only
// destination for new-window: without the delimiter the operand is still read
// as a window target and falls back to prefix resolution. Only the trailing
// colon makes a name session-qualified. (A native id needs neither modifier;
// see NewWindowSessionTarget.) One helper that emitted "the exact target"
// for every command would get this wrong for at least one of them, so each
// command's destination is built by a function that knows which command it is
// for.

// NewWindowSessionTarget renders a `new-window -t` destination naming a session
// by its native id.
//
// Passing a native id rather than a name is what makes this safe: tmux resolves
// `$N` by identity, with no exact, glob, or prefix step to fall through. The
// `=` modifier is not needed, and neither, strictly, is the colon — measured on
// tmux 3.7b, `new-window -t '$1'` lands in $1 without it.
//
// The colon is kept as defence in depth, not as a per-command asymmetry: it is
// load-bearing ONLY for the NAME spelling this function can no longer emit
// (`=forgectl` lands in the sibling, `=forgectl:` refuses). Emitting it anyway
// means the argv stays session-qualified even if a future edit reintroduces a
// name here. It is asserted by TestNewWindowSessionTargetKeepsTrailingColon
// plus the isolated real-tmux integration test.
func NewWindowSessionTarget(sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return sessionID + ":", nil
}
