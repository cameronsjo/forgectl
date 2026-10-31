package hcadapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

var (
	// ErrStillThere reports a reference whose terminal is still on the core:
	// the recorded address holds a terminal created at the recorded time,
	// live or finished. `surface close` handles it.
	ErrStillThere = errors.New("hc: the worker's terminal is still there")
	// ErrSelectionUnproven reports a changed core id that forget cannot call
	// a wiped core, because nothing shows the core selection is the one the
	// worker was launched under.
	ErrSelectionUnproven = errors.New("hc: the core differs, and nothing shows it is the same core selection the worker was launched under")
)

// Abandoned reports why ref provably names no terminal of forgectl's, so
// its ledger row can be dropped without asking hc to act on anything
// (forgectl#1386). launched is the SelectionDigest the launch recorded, or
// "" for a row from before forgectl recorded one. It runs locate's checks, in
// the same order, but tells their mismatches apart:
//
//   - The core's id differs from the one the reference was taken on, the
//     core selection (SelectionDigest) is the launch's, and `hc get` finds
//     nothing at the recorded address, or a terminal created at another
//     time. A new id alone proves nothing: a core restarted after its
//     core.json is deleted reports one while its terminals keep running
//     (hc 0.1.15), and a differing or unrecorded selection reaches a
//     different core while the worker's may still run. An unproven
//     selection refuses with ErrSelectionUnproven.
//   - `hc get` finds nothing at the recorded address on the matching core.
//   - The address holds a terminal with another creation time: hc handed the
//     address out again after `hc rm`, so the one forgectl started is gone.
//
// Labels prove nothing either way here: any hc client, the worker included,
// can rewrite a terminal's forgectl.surface label, so a terminal at the
// recorded address and creation time is still the worker whatever its label
// says (ErrStillThere). A reference taken under another HC_SESSION
// selection, or a core or terminal hc cannot read, wraps ErrUnreadable.
//
// It never kills, removes, or types anything. Like every hc call, its
// `hc server status` starts a core when none answers the selection
// (measured on hc 0.1.15; hc has no read that does not); that core inherits
// no launcher handles (command).
func (a *Adapter) Abandoned(ctx context.Context, ref backend.Ref, launched string) (string, error) {
	id, err := ref.HcIdentity()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	if ref.Source() != a.source {
		return "", fmt.Errorf("%w: the reference names a different hc session selection; run this with the HC_SESSION the worker was launched under", ErrUnreadable)
	}
	server, cause := a.readiness(ctx)
	if cause != nil {
		return "", fmt.Errorf("%w: hc server status: %v", ErrUnreadable, *cause)
	}
	coreChanged := !ref.Server().Matches(server)
	if coreChanged {
		switch {
		case launched == "":
			return "", fmt.Errorf("%w: the row records no core selection (it was launched before forgectl recorded one)", ErrSelectionUnproven)
		case launched != a.selection:
			return "", fmt.Errorf("%w: HC_SOCKET, the XDG paths or HOME differ from the launch's; run this with the environment the worker was launched under", ErrSelectionUnproven)
		}
	}
	// A new core id under the launch's own selection is still not proof: a
	// core whose core.json is deleted and then restarted reports a new id
	// while its terminals keep running at the same address and creation
	// time (measured on hc 0.1.15). So the address is read on this core
	// either way, and only what is there decides.
	t, state, gcause := a.get(ctx, id.Terminal())
	switch state {
	case getAbsent:
		if coreChanged {
			return "the hc core under the worker's own core selection has a new id and no terminal at the worker's recorded address: its state was wiped, and the worker's terminal with it", nil
		}
		return "hc has no terminal at the worker's recorded address", nil
	case getUnreadable:
		if gcause != nil {
			return "", fmt.Errorf("%w: hc get: %v", ErrUnreadable, *gcause)
		}
		return "", fmt.Errorf("%w: hc get failed", ErrUnreadable)
	case getFound:
	}
	if t.CreatedMS != id.CreatedMS() {
		return "the worker's recorded address now holds a terminal created at another time", nil
	}
	if t.Labels[ownerLabel] != ref.OwnershipName() || coreChanged {
		// close refuses this terminal too (its label or core does not match
		// the reference), so name the step that ends it.
		return "", fmt.Errorf("%w: its forgectl.surface label or core id no longer matches, so surface close refuses it as well; end it with hc kill %s and hc rm %s, then run forget again", ErrStillThere, t.Address, t.Address)
	}
	return "", fmt.Errorf("%w: run surface close", ErrStillThere)
}
