//go:build !unix

package workflow

import "os"

// flockTryExclusive is a no-op on platforms without flock: it always reports the
// lock acquired. forgectl's supported hosts are macOS and Linux (both unix), so
// this stub exists only to keep `go build ./...` honest on other GOOS values —
// where the concurrent-run guard is simply unavailable.
func flockTryExclusive(*os.File) (bool, error) { return true, nil }

// flockUnlock is a no-op on platforms without flock.
func flockUnlock(*os.File) error { return nil }
