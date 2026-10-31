package hcadapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// ErrStillThere reports a reference whose terminal is still on the core and
// still forgectl's, live or finished. `surface close` handles it.
var ErrStillThere = errors.New("hc: the worker's terminal is still there")

// Abandoned reports why ref provably names no terminal of forgectl's on the
// core this process selects, so its ledger row can be dropped without asking
// hc to act on anything (forgectl#1386). It runs locate's checks, in the same
// order, but tells their mismatches apart:
//
//   - The core's id differs from the one the reference was taken on. The id
//     survives `hc server restart` and `hc update` and changes only when the
//     core's state is wiped, and a terminal does not outlive its core's
//     state, so the worker's terminal is gone.
//   - `hc get` finds nothing at the recorded address on the matching core.
//   - The address holds a terminal with another creation time or label: hc
//     handed the address out again, so the one forgectl started is gone.
//
// Anything else refuses: a reference taken under another HC_SESSION
// selection, or a core or terminal hc cannot read, wraps ErrUnreadable, and
// forgectl's own terminal still at its address wraps ErrStillThere. It never
// kills, removes, or types anything.
func (a *Adapter) Abandoned(ctx context.Context, ref backend.Ref) (string, error) {
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
	if !ref.Server().Matches(server) {
		return "the hc core is not the one the worker was started on: its state was wiped, and the worker's terminal with it", nil
	}
	t, state, gcause := a.get(ctx, id.Terminal())
	switch state {
	case getAbsent:
		return "hc has no terminal at the worker's recorded address", nil
	case getUnreadable:
		if gcause != nil {
			return "", fmt.Errorf("%w: hc get: %v", ErrUnreadable, *gcause)
		}
		return "", fmt.Errorf("%w: hc get failed", ErrUnreadable)
	case getFound:
	}
	if t.Address != id.Terminal() || t.CreatedMS != id.CreatedMS() || t.Labels[ownerLabel] != ref.OwnershipName() {
		return "the worker's recorded address now holds a terminal forgectl did not start", nil
	}
	return "", ErrStillThere
}
