//go:build !unix

package launch

import "io"

// A platform without the openat/no-follow/flock suite gets no store at all.
// The alternative — a pathname-based fallback — cannot prove that the file it
// finally writes is the file it checked, and a privacy feature that silently
// appends somewhere unverified is worse than a feature that is simply absent.
//
// Collection stays behaviorally transparent here: RecordUsage returns the
// typed unsupported error, and every caller on the launch path discards it.
func init() {
	appendUsageRow = func(string, []byte) error { return ErrUsageUnsupported }
	openUsageReader = func(string) (io.ReadCloser, func(), error) { return nil, nil, ErrUsageUnsupported }
	inspectUsageStore = func(paths UsageStorePaths) (UsageStoreStatus, error) {
		return UsageStoreStatus{Paths: paths, Refusal: ErrUsageUnsupported}, nil
	}
}
