// Package sockstat reads the platform facts that identify one incarnation of a
// backend's endpoint socket, and the uid that owns it.
//
// It exists because three adapters need the same two answers. The tmux adapter
// wrote them first; extracting them here at the second caller rather than the
// third is deliberate — #332 opened by warning that re-forking a hardened
// behaviour is how the surface layer accumulates divergent copies, and the tmux
// adapter had already proved the warning by re-forking four of internal/tmux's
// and getting two of them wrong.
//
// Both answers fail CLOSED where the platform cannot supply them, and the two
// failures are different on purpose:
//
// Fill leaving Inode zero makes backend.Fingerprint refuse outright, so no
// reference is built at all — which is strictly better than a digest that would
// still match after the server restarted on the same path.
//
// OwnerUID reporting ok=false makes a caller decline to ASSERT ownership rather
// than quietly pass the check. An ownership test that silently succeeds when it
// cannot see the owner is worse than no test, because it reads as one.
package sockstat

import (
	"os"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Fill copies the endpoint socket's device and inode into an incarnation input.
//
// The inode is the field that carries the weight: it turns over when a server
// restarts on the same path, which is precisely the event a fingerprint built
// from path and version alone would miss.
//
// ChangedAtUnixNano is deliberately left zero. Its field name differs by
// platform — Ctim on Linux, Ctimespec on Darwin — so reading it would mean
// another build-tagged pair, and backend documents it as optional for exactly
// that reason.
//
// How much a caller may lean on the inode alone varies by backend, and callers
// should say so where they build the input rather than assume it here: a tmux
// fingerprint also carries the server pid and start time, so it has three
// independent witnesses to a restart, while a backend that reports neither has
// only this one.
func Fill(in *backend.IncarnationInput, info os.FileInfo) { fill(in, info) }

// OwnerUID reports the uid owning the stat'ed object, and whether the platform
// could answer at all. A false ok means "cannot establish", never "ours".
func OwnerUID(info os.FileInfo) (int, bool) { return ownerUID(info) }
