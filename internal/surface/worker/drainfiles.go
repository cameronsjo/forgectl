package worker

import (
	"errors"
	"fmt"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// `surface drain` keeps three files beside the queue, written through the
// same pinned directory and verified open: drain.lock (held with flock for
// the life of the drain process), drain.json (its status, replaced by an
// atomic rename on every tick), and drain-events.jsonl (appended, and renamed
// to drain-events.jsonl.1 when it would pass MaxDrainEventsBytes).

//
// `surface prune` adds two more: usage-daily.jsonl, the long-term cost
// record (appended, never rotated or pruned), and drain-prune-day, the UTC
// day the drain last ran its daily prune, so a restart does not run it again
// that day.

const (
	drainLockName        = "drain.lock"
	drainStatusName      = "drain.json"
	drainStatusTmpName   = "drain.json.tmp"
	drainEventsName      = "drain-events.jsonl"
	drainEventsOldName   = "drain-events.jsonl.1"
	usageDailyName       = "usage-daily.jsonl"
	drainPruneDayName    = "drain-prune-day"
	drainPruneDayTmpName = "drain-prune-day.tmp"
)

// UTCDayLayout is the day format of usage-daily.jsonl and drain-prune-day.
const UTCDayLayout = "2006-01-02"

// checkDay refuses a day that is not one UTCDayLayout date.
func checkDay(day string) error {
	if _, err := time.Parse(UTCDayLayout, day); err != nil || len(day) != len(UTCDayLayout) {
		return fmt.Errorf("worker: %q is not a YYYY-MM-DD day", day)
	}
	return nil
}

// MaxDrainEventsBytes is the size at which the events file is renamed to
// .1, replacing the previous .1. It is the read cap every file in the
// surface directory shares, so a reader always reads a whole file.
const MaxDrainEventsBytes = maxLedgerBytes

// ErrDrainLocked reports a drain lock another process holds.
var ErrDrainLocked = errors.New("worker: another surface drain holds drain.lock")

// DrainFiles is the drain's files under one state base.
type DrainFiles struct {
	stateBase string
}

// OpenDrainFiles returns the drain's files in $XDG_STATE_HOME/forgectl/surface,
// defaulting to ~/.local/state.
func OpenDrainFiles() (DrainFiles, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return DrainFiles{}, err
	}
	return DrainFiles{stateBase: base}, nil
}
