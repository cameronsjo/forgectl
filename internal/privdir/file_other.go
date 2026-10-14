//go:build !unix

package privdir

import (
	"io/fs"
	"os"
)

// OpenFile is unavailable off Unix, for the reason Pin is; see the unix file.
func OpenFile(int, string, int, fs.FileMode) (*os.File, error) { return nil, ErrUnsupported }
