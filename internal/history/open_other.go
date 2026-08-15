//go:build !unix

package history

import "os"

// openHistory opens the history file. There is no non-blocking open to reach
// for off unix, so this is a plain open.
//
// Known residual, unverified: on Windows a named pipe can block in os.Open,
// and os.Stat on that path is itself a CreateFile, so neither the pre-open
// kind check nor this open is guaranteed non-blocking there. The unix build
// closes that window with O_NONBLOCK; this one does not. Recorded rather than
// assumed closed — zsh history on Windows is exotic enough that no Windows
// host was available to test it.
func openHistory(path string) (*os.File, error) {
	return os.Open(path)
}
