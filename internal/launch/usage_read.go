package launch

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// AggregateUsage streams a JSONL usage store and reduces it to the pinned v1
// aggregate. days nil means all time.
//
// It never loads the store into memory and never raises its per-row bound: a
// row longer than MaxUsageRowBytes is discarded, not buffered, so a corrupted
// or hostile file cannot turn a stats run into an allocation.
//
// Damage is counted, never fatal. A malformed, oversized, crash-partial, or
// future-version row increments SkippedRows and the scan continues, because
// the alternative — refusing to report anything — hands one bad byte the power
// to hide every good row after it. A valid row that simply falls outside the
// window is not a skip.
func AggregateUsage(r io.Reader, days *int64, now time.Time) (UsageAggregateV1, error) {
	agg := newUsageAggregate()

	var cutoff time.Time
	windowed := days != nil
	if windowed {
		bound, err := UsageCutoff(now, *days)
		if err != nil {
			return UsageAggregateV1{}, err
		}
		cutoff = bound
		cutoffText := UsageTimestamp(cutoff)
		windowDays := *days
		agg.Window = UsageWindow{Days: &windowDays, Cutoff: &cutoffText}
	}

	// One extra kilobyte over the row bound so a row of exactly
	// MaxUsageRowBytes is readable in one slice rather than tripping the
	// oversize path it is legitimately inside.
	br := bufio.NewReaderSize(r, MaxUsageRowBytes+1024)
	for {
		line, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			agg.SkippedRows++
			if discardErr := discardUsageRow(br); discardErr != nil {
				return agg, nil
			}
			continue
		case errors.Is(err, io.EOF):
			if len(line) > 0 {
				// An unterminated final row is a crash-partial write: the
				// writer emits the newline as part of the same single write,
				// so its absence proves the row never completed.
				agg.SkippedRows++
			}
			return agg, nil
		case err != nil:
			// A read failure mid-scan leaves the aggregate provably partial,
			// so the caller must not print it as a complete report.
			return UsageAggregateV1{}, err
		}

		row := line[:len(line)-1]
		if len(line) > MaxUsageRowBytes || !accumulateUsageRow(&agg, row, windowed, cutoff) {
			agg.SkippedRows++
		}
	}
}

// discardUsageRow drops the remainder of an oversized row without retaining
// it, so the next row starts on a real boundary.
func discardUsageRow(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// accumulateUsageRow folds one complete row into the aggregate. It reports
// false for every row the reader cannot vouch for; an out-of-window valid row
// returns true because it was understood, merely not counted.
func accumulateUsageRow(agg *UsageAggregateV1, row []byte, windowed bool, cutoff time.Time) bool {
	var version struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(row, &version); err != nil {
		return false
	}
	if version.SchemaVersion != UsageSchemaVersion {
		// Either a future writer or a corrupted version field. Both are
		// countable rather than guessable: a v2 row may mean something
		// different by the same key names.
		return false
	}

	var ev UsageEventV1
	// Unknown keys are ignored by design — an additive v1 field written by a
	// newer forgectl must not invalidate a row this build can still count.
	if err := json.Unmarshal(row, &ev); err != nil {
		return false
	}
	if err := ev.Validate(); err != nil {
		return false
	}
	ts, err := time.Parse(time.RFC3339, ev.TS)
	if err != nil {
		return false
	}
	if windowed && ts.Before(cutoff) {
		return true
	}

	agg.TotalAttempts++
	agg.Counts.Harness[ev.Harness]++
	agg.Counts.Model[ev.Model]++
	agg.Counts.SessionMode[ev.SessionMode]++
	agg.Counts.Posture[ev.Posture]++
	if agg.FirstTS == nil || ev.TS < *agg.FirstTS {
		first := ev.TS
		agg.FirstTS = &first
	}
	if agg.LastTS == nil || ev.TS > *agg.LastTS {
		last := ev.TS
		agg.LastTS = &last
	}
	return true
}
