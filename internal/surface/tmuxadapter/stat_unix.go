package tmuxadapter

import (
	"os"
	"strings"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// fillStat copies the socket's device and inode into the incarnation input.
//
// The inode is the field that matters: it turns over when a server restarts on
// the same path, which is exactly the event a path-and-version fingerprint
// would miss. Fingerprint requires it to be non-zero, so a stat that yields no
// Stat_t leaves the input incomplete and fingerprinting fails closed rather
// than producing a digest that would match across a restart.
//
// ChangedAtUnixNano is deliberately left zero. Its field name differs by
// platform — Ctim on Linux, Ctimespec on Darwin — so reading it would mean a
// build-tagged file pair, and the field is documented as optional precisely
// because not every platform reports one. It buys nothing here: a tmux
// fingerprint already carries three independent witnesses to a restart (the
// socket inode, the server pid, and the server start time), and the two the
// socket does not supply are the stronger pair.
//
// Dev is int32 on Darwin and uint64 on Linux, so the conversion is written
// explicitly rather than leaning on an untyped constant.
func fillStat(in *backend.IncarnationInput, info os.FileInfo) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	in.Device = uint64(sys.Dev) //nolint:unconvert,gosec // G115: widening; Dev is int32 on Darwin, uint64 on Linux
	in.Inode = uint64(sys.Ino)  //nolint:unconvert // widening for the same reason
}

// hasPrefixTrimmed reports whether s, with surrounding whitespace removed,
// begins with prefix. tmux diagnostics arrive with a trailing newline and
// occasionally a leading one.
func hasPrefixTrimmed(s, prefix string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), prefix)
}
