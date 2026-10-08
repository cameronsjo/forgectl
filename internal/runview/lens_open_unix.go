// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"os"

	"golang.org/x/sys/unix"
)

// openLensFile opens a lens without blocking: a FIFO named like a lens must
// not hang the open. LoadLens then reads it only if it is a regular file. A
// symlink is followed: a person's dotfiles may link their lenses in.
func openLensFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0) //nolint:gosec // G304: the person names their own lens file
}
