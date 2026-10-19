//go:build !unix

package sweep

import "os"

func openNoBlock(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: path is under a validated worker directory
}
