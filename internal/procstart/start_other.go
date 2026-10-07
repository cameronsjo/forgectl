//go:build !darwin && !linux

package procstart

// procStart is unavailable here, so Matches always refuses.
func procStart(int) (int64, bool, error) { return 0, false, ErrUnsupported }
