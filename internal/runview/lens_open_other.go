// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package runview

import "os"

// openLensFile opens a lens. Off Unix there is no log source to use it with.
func openLensFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the person names their own lens file
}
