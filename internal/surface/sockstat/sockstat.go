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
	"strings"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Fill copies the endpoint socket's device and inode into an incarnation input.
//
// The inode is the field that carries the weight: it turns over when a server
// restarts on the same path, which is precisely the event a fingerprint built
// from path and version alone would miss.
//
// ChangedAtUnixNano is read too, and it used to be skipped for a reason that did
// not survive contact with what it costs: the field name differs by platform —
// Ctim on Linux, Ctimespec on Darwin — so reading it means another build-tagged
// trio, and backend documents it as optional.
//
// What that reasoning left out is who pays. tmux's fingerprint carries the
// server pid and start time as well; cmux and herdr expose neither, so before
// this the inode was the whole of their evidence (forgectl#344). A second
// witness for the two backends that had one is worth three small files.
//
// It is a WEAKER witness than a pid, and deliberately so in the safe direction.
// ctime moves on any metadata change, not only on creation, so it can differ
// when nothing restarted — which refuses a close that would have succeeded and
// costs a manual cleanup. The failure it prevents is the opposite one: a
// fingerprint that survives a restart is what lets a rollback close a stranger's
// workspace.
//
// Platforms that are neither Darwin nor Linux read no timestamp and keep exactly
// the witnesses they had, rather than gaining a selector this project cannot
// build or test.
func Fill(in *backend.IncarnationInput, info os.FileInfo) { fill(in, info) }

// OwnerUID reports the uid owning the stat'ed object, and whether the platform
// could answer at all. A false ok means "cannot establish", never "ours".
func OwnerUID(info os.FileInfo) (int, bool) { return ownerUID(info) }

// UnsafeDirectoryReason reports why a socket's containing directory cannot be
// called private. An empty result means no known concern: the directory is not
// group/world-writable and, on platforms that expose ownership, belongs to the
// current uid.
//
// Ownership that the platform cannot establish is deliberately not described
// as foreign. This check is advisory, so inventing a concern from an absent
// answer would produce a warning the operator cannot act on and would differ
// from the stated policy: warn about a directory KNOWN to be writable by or
// owned by another user.
func UnsafeDirectoryReason(info os.FileInfo, selfUID int) string {
	if info == nil || !info.IsDir() {
		return ""
	}
	var reasons []string
	if info.Mode().Perm()&0o022 != 0 {
		reasons = append(reasons, "group or world writable")
	}
	if owner, ok := OwnerUID(info); ok && owner != selfUID {
		reasons = append(reasons, "owned by another user")
	}
	return strings.Join(reasons, " and ")
}
