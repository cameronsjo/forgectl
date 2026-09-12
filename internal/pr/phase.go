package pr

// Phase is the durable lifecycle state of one review-session record. It is
// what the record SAYS; whether a tmux window actually exists is observed
// separately (WindowsLive) and rendered beside it, never folded into it.
//
// A legacy record (no version field) has no phase. Every consumer that needs
// one treats it as active with an unknown window — conservative, and it
// costs nothing because a legacy record never occupies a slot.
type Phase string

const (
	// PhaseQueued is intent only: no workspace, no slot, waiting for a drainer.
	PhaseQueued Phase = "queued"
	// PhasePreparing is a reserved slot with the clone in flight.
	PhasePreparing Phase = "preparing"
	// PhasePrepared has a live workspace and no launch yet.
	PhasePrepared Phase = "prepared"
	// PhaseLaunching is fsynced BEFORE tmux new-window, so a crash between the
	// two leaves a record that says exactly where it died.
	PhaseLaunching Phase = "launching"
	// PhaseActive carries a validated native window identity.
	PhaseActive Phase = "active"
	// PhaseNeedsRepair is written when a transition could not prove its own
	// outcome; it always carries a reason.
	PhaseNeedsRepair Phase = "needs-repair"
)

// valid reports whether p is one of the six phases this build knows. An
// unknown phase on a version-2 record is refused at validation rather than
// mapped to anything, because a phase this build cannot name is one it cannot
// reason about.
func (p Phase) valid() bool {
	switch p {
	case PhaseQueued, PhasePreparing, PhasePrepared, PhaseLaunching, PhaseActive, PhaseNeedsRepair:
		return true
	}
	return false
}

// allowsEmptyWorkspace reports whether a record in phase p may carry no
// workspace: nothing has been cloned yet for a queued or preparing session.
func (p Phase) allowsEmptyWorkspace() bool {
	return p == PhaseQueued || p == PhasePreparing
}
