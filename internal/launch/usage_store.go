package launch

import (
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// The store's names and modes are fixed, not configurable. A configurable
// destination would turn a privacy promise ("exactly these two files, here")
// into a per-machine question, and it would hand a hostile config the ability
// to aim an append at a path forgectl was trusted to leave alone.
const (
	usageLeafDir  = "forgectl"
	usageDataName = "launch-usage.jsonl"
	usageLockName = "launch-usage.jsonl.lock"

	usageLeafMode = 0o700
	usageFileMode = 0o600
	// Directories forgectl has to create on the way to the state base but does
	// not own — a missing ~/.local, say. They get the conventional mode, since
	// a statistics opt-in has no business deciding the permissions of a
	// general-purpose parent other tools will share.
	usageAncestorMode = 0o755
)

// UsageStorePaths names every file collection can touch, so documentation,
// `launch doctor`, and the deletion instructions all read from one source.
type UsageStorePaths struct {
	Base string
	Leaf string
	Data string
	Lock string
}

// UsageStoreStatus is the inspect-only view `launch doctor` reports. It never
// carries file contents — only whether each name is present and whether the
// namespace was safe to open.
type UsageStoreStatus struct {
	Paths       UsageStorePaths
	LeafPresent bool
	DataPresent bool
	LockPresent bool
	// Refusal is set when the namespace exists but forgectl declined to use
	// it. Disabled collection with no namespace is healthy, not a refusal.
	Refusal error
	// Narrowed lists the paths whose permissions were broader than the store's
	// own modes and were tightened during this inspection. The narrowing is the
	// shared opener's, not doctor's: inspection cannot reach the store without
	// it. Reporting the paths is what keeps the correction from erasing the
	// evidence an operator called doctor to see.
	Narrowed []string
}

// usageBase is a seam: tests point the whole store at a scratch directory
// without setting process-wide environment variables.
var usageBase = config.LaunchUsageBase

// UsagePaths resolves the fixed store layout under the current state base.
func UsagePaths() (UsageStorePaths, error) {
	base, err := usageBase()
	if err != nil {
		return UsageStorePaths{}, err
	}
	leaf := filepath.Join(base, usageLeafDir)
	return UsageStorePaths{
		Base: base,
		Leaf: leaf,
		Data: filepath.Join(leaf, usageDataName),
		Lock: filepath.Join(leaf, usageLockName),
	}, nil
}

// RecordUsage appends one accepted-attempt row when collection is enabled.
//
// Callers on the launch and resume paths discard its error entirely — see
// recordLaunchUsage in internal/cli. The typed errors exist for `launch
// stats` and `launch doctor`, which the operator asked for.
//
// Order matters: the row is validated and bounded BEFORE the state base is
// resolved or anything is opened, so a disabled or malformed call touches the
// filesystem zero times.
func RecordUsage(enabled bool, ev UsageEventV1) error {
	if !enabled {
		return ErrUsageDisabled
	}
	row, err := EncodeUsageEvent(ev)
	if err != nil {
		return err
	}
	base, err := usageBase()
	if err != nil {
		return err
	}
	return appendUsageRow(base, row)
}

// ReadUsage aggregates the store for a window. days nil means all time. An
// absent store is a valid empty report, not an error: a machine that has never
// launched is indistinguishable from one that has never enabled collection,
// and neither is a fault worth an exit code.
func ReadUsage(days *int64, now time.Time) (UsageAggregateV1, error) {
	base, err := usageBase()
	if err != nil {
		return UsageAggregateV1{}, err
	}
	reader, release, err := openUsageReader(base)
	if err != nil {
		return UsageAggregateV1{}, err
	}
	if reader == nil {
		return AggregateUsage(strings.NewReader(""), days, now)
	}
	defer release()
	return AggregateUsage(reader, days, now)
}

// InspectUsage reports store health without creating or repairing anything.
func InspectUsage() (UsageStoreStatus, error) {
	paths, err := UsagePaths()
	if err != nil {
		return UsageStoreStatus{}, err
	}
	return inspectUsageStore(paths)
}

// The platform seam, assigned in usage_store_unix.go and usage_store_other.go.
// A shipped platform gets the pinned-descriptor implementation; anything else
// refuses rather than falling back to pathname lookups it cannot make safe.
var (
	appendUsageRow    func(base string, row []byte) error
	openUsageReader   func(base string) (io.ReadCloser, func(), error)
	inspectUsageStore func(paths UsageStorePaths) (UsageStoreStatus, error)
)
