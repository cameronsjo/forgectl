package launch

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// UsageWindow is the requested reporting window. Both fields are pointers so
// "all time" encodes as an explicit null pair rather than a zero day count an
// exporter would have to special-case.
type UsageWindow struct {
	Days   *int64  `json:"days"`
	Cutoff *string `json:"cutoff"`
}

// UsageCountsV1 holds the four count dimensions as raw string-keyed maps. They
// are always non-nil so an empty result encodes as {} and never as null — the
// schema types must not change shape between an empty and a populated store.
type UsageCountsV1 struct {
	Harness     map[string]int `json:"harness"`
	Model       map[string]int `json:"model"`
	SessionMode map[string]int `json:"session_mode"`
	Posture     map[string]int `json:"posture"`
}

// UsageAggregateV1 is the pinned machine contract of `launch stats --json`. It
// evolves additively only: a consumer may rely on every field below keeping
// its name, type, and null semantics.
type UsageAggregateV1 struct {
	SchemaVersion int           `json:"schema_version"`
	Window        UsageWindow   `json:"window"`
	TotalAttempts int           `json:"total_attempts"`
	FirstTS       *string       `json:"first_ts"`
	LastTS        *string       `json:"last_ts"`
	Counts        UsageCountsV1 `json:"counts"`
	// SkippedRows counts rows examined and not understood — malformed,
	// oversized, crash-partial, or written by a future schema version. It is
	// the operator's signal that the store has damage, and it is why a scan
	// with skips exits nonzero.
	SkippedRows int `json:"skipped_rows"`
}

func newUsageAggregate() UsageAggregateV1 {
	return UsageAggregateV1{
		SchemaVersion: UsageSchemaVersion,
		Counts: UsageCountsV1{
			Harness:     map[string]int{},
			Model:       map[string]int{},
			SessionMode: map[string]int{},
			Posture:     map[string]int{},
		},
	}
}

// ParseUsageDays accepts only a positive base-10 integer. Zero, negative,
// fractional, signed, and padded spellings are usage errors rather than
// silently-coerced windows: a stats run that quietly reported the wrong period
// is worse than one that refuses.
func ParseUsageDays(arg string) (int64, error) {
	days, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || strconv.FormatInt(days, 10) != arg {
		return 0, fmt.Errorf("days must be a positive whole number of days, got %q", arg)
	}
	if days <= 0 {
		return 0, fmt.Errorf("days must be greater than zero, got %d", days)
	}
	return days, nil
}

// UsageCutoff returns the inclusive lower bound of a days-long window ending
// at now. The window is an exact elapsed span of 24-hour days, not a run of
// local calendar days — a DST transition must not silently lengthen or shorten
// the period a reported number covers.
//
// The overflow check runs BEFORE the duration is built, because
// time.Duration(days)*24*time.Hour wraps silently and would produce a cutoff
// in the future that reports zero attempts as if the store were empty.
func UsageCutoff(now time.Time, days int64) (time.Time, error) {
	const maxDays = int64(math.MaxInt64) / int64(24*time.Hour)
	if days <= 0 || days > maxDays {
		return time.Time{}, fmt.Errorf("days must be between 1 and %d, got %d", maxDays, days)
	}
	return now.UTC().Truncate(time.Second).Add(-time.Duration(days) * 24 * time.Hour), nil
}

// UsageCount is one rendered count row.
type UsageCount struct {
	Key   string
	Count int
}

// SortUsageCounts orders a count map for human output: most frequent first,
// ties broken lexically so repeated runs over an unchanged store print
// identically. Map iteration order would otherwise make the report unstable.
func SortUsageCounts(counts map[string]int) []UsageCount {
	out := make([]UsageCount, 0, len(counts))
	for key, count := range counts {
		out = append(out, UsageCount{Key: key, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}
