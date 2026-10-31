//go:build !unix

package workerwatch

import "os"

// openReport opens a brief-dir file for reading. The caller checks the
// handle is a regular file.
func openReport(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: a name reportNameRe matched, inside the brief dir
}
