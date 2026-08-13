package launch

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rowFor(t *testing.T, ts, harness, model, mode, posture string) string {
	t.Helper()
	row, err := EncodeUsageEvent(UsageEventV1{
		SchemaVersion: UsageSchemaVersion,
		TS:            ts,
		Event:         UsageEventExecAttempt,
		Harness:       harness,
		Model:         model,
		SessionMode:   mode,
		Posture:       posture,
	})
	if err != nil {
		t.Fatalf("EncodeUsageEvent: %v", err)
	}
	return string(row)
}

func TestParseUsageDays_PositiveOnlyOverflowSafe(t *testing.T) {
	for _, bad := range []string{"0", "-1", "1.5", "seven", "", " 7", "+7", "0x7", "99999999999999999999"} {
		if _, err := ParseUsageDays(bad); err == nil {
			t.Fatalf("ParseUsageDays(%q) accepted an invalid window", bad)
		}
	}
	got, err := ParseUsageDays("7")
	if err != nil || got != 7 {
		t.Fatalf("ParseUsageDays(\"7\") = %d, %v", got, err)
	}
	// A day count whose nanosecond duration overflows int64 is a usage error,
	// not a panic and not a cutoff that wrapped into the future.
	if _, err := UsageCutoff(time.Now(), int64(1)<<40); err == nil {
		t.Fatal("UsageCutoff accepted an overflowing window")
	}
}

func TestUsageCutoff_InclusiveUTCSecond(t *testing.T) {
	now := mustTime(t, "2026-08-13T19:42:17Z").Add(750 * time.Millisecond).In(time.FixedZone("PDT", -7*3600))
	cutoff, err := UsageCutoff(now, 7)
	if err != nil {
		t.Fatalf("UsageCutoff: %v", err)
	}
	if got := cutoff.Format(time.RFC3339); got != "2026-08-06T19:42:17Z" {
		t.Fatalf("cutoff = %s, want 2026-08-06T19:42:17Z", got)
	}
	if cutoff.Location() != time.UTC {
		t.Fatalf("cutoff location = %v, want UTC", cutoff.Location())
	}
}

func TestSortUsageCounts_CountDescendingThenKeyLexical(t *testing.T) {
	got := SortUsageCounts(map[string]int{"zeta": 2, "alpha": 2, "mid": 5, "solo": 1})
	want := []UsageCount{{Key: "mid", Count: 5}, {Key: "alpha", Count: 2}, {Key: "zeta", Count: 2}, {Key: "solo", Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SortUsageCounts = %v, want %v", got, want)
	}
}

func TestReadUsage_StreamsMalformedFuturePartialOversizedAt64KiB(t *testing.T) {
	now := mustTime(t, "2026-08-13T19:42:17Z")
	oversized := `{"schema_version":1,"ts":"2026-08-13T19:00:00Z","event":"exec_attempt","harness":"claude","model":"` +
		strings.Repeat("m", MaxUsageRowBytes) + `","session_mode":"new","posture":"default"}` + "\n"

	log := rowFor(t, "2026-08-13T10:00:00Z", "claude", "opus", "new", "default") +
		"\n" + // empty line
		`{"schema_version":9,"ts":"2026-08-13T11:00:00Z"}` + "\n" + // unsupported future version
		"{broken\n" + // malformed
		oversized +
		rowFor(t, "2026-08-13T12:00:00Z", "codex", "", "unknown", "builder") +
		`{"schema_version":1,"ts":"2026-08-13T13:00:00Z","event":"exec_at` // crash-partial tail

	agg, err := AggregateUsage(strings.NewReader(log), nil, now)
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if agg.TotalAttempts != 2 {
		t.Fatalf("total = %d, want the 2 valid rows around the invalid ones", agg.TotalAttempts)
	}
	if agg.SkippedRows != 5 {
		t.Fatalf("skipped = %d, want 5 (empty, future, malformed, oversized, partial)", agg.SkippedRows)
	}
	if agg.Counts.Harness["codex"] != 1 || agg.Counts.Harness["claude"] != 1 {
		t.Fatalf("harness counts = %v, want one each", agg.Counts.Harness)
	}
}

func TestAggregateV1_CutoffBoundaryIsInclusive(t *testing.T) {
	now := mustTime(t, "2026-08-13T19:42:17Z")
	days := int64(7)
	log := rowFor(t, "2026-08-06T19:42:17Z", "claude", "opus", "new", "default") +
		rowFor(t, "2026-08-06T19:42:16Z", "claude", "opus", "new", "default")
	agg, err := AggregateUsage(strings.NewReader(log), &days, now)
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if agg.TotalAttempts != 1 {
		t.Fatalf("total = %d, want 1 (cutoff inclusive, one second earlier excluded)", agg.TotalAttempts)
	}
	if agg.SkippedRows != 0 {
		t.Fatalf("skipped = %d, want 0 — an out-of-window valid row is not a skip", agg.SkippedRows)
	}
}

func TestReadUsage_UnknownV1FieldsAdditive(t *testing.T) {
	now := mustTime(t, "2026-08-13T19:42:17Z")
	log := `{"schema_version":1,"ts":"2026-08-13T10:00:00Z","event":"exec_attempt","harness":"claude",` +
		`"model":"opus","session_mode":"new","posture":"default","future_key":"ignored"}` + "\n"
	agg, err := AggregateUsage(strings.NewReader(log), nil, now)
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	if agg.TotalAttempts != 1 || agg.SkippedRows != 0 {
		t.Fatalf("total=%d skipped=%d, want 1 and 0 — unknown v1 fields are additive", agg.TotalAttempts, agg.SkippedRows)
	}
}

func TestAggregateV1_ExactJSON_AllTimeWindowedAndEmpty(t *testing.T) {
	now := mustTime(t, "2026-08-13T19:42:17Z")
	log := rowFor(t, "2026-08-10T13:00:00Z", "claude", "opus", "new", "default") +
		rowFor(t, "2026-08-13T19:40:00Z", "claude", "opus", "fork", "default") +
		rowFor(t, "2026-08-12T09:00:00Z", "codex", "", "new", "default") +
		"{not json}\n"

	days := int64(7)
	agg, err := AggregateUsage(strings.NewReader(log), &days, now)
	if err != nil {
		t.Fatalf("AggregateUsage: %v", err)
	}
	got, err := json.Marshal(agg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"schema_version":1,"window":{"days":7,"cutoff":"2026-08-06T19:42:17Z"},` +
		`"total_attempts":3,"first_ts":"2026-08-10T13:00:00Z","last_ts":"2026-08-13T19:40:00Z",` +
		`"counts":{"harness":{"claude":2,"codex":1},"model":{"":1,"opus":2},` +
		`"session_mode":{"fork":1,"new":2},"posture":{"default":3}},"skipped_rows":1}`
	if string(got) != want {
		t.Fatalf("windowed aggregate =\n%s\nwant\n%s", got, want)
	}

	allTime, err := AggregateUsage(strings.NewReader(log), nil, now)
	if err != nil {
		t.Fatalf("AggregateUsage all time: %v", err)
	}
	rawAll, err := json.Marshal(allTime)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(rawAll), `"window":{"days":null,"cutoff":null}`) {
		t.Fatalf("all-time window is not null-shaped: %s", rawAll)
	}

	empty, err := AggregateUsage(strings.NewReader(""), nil, now)
	if err != nil {
		t.Fatalf("AggregateUsage empty: %v", err)
	}
	rawEmpty, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantEmpty := `{"schema_version":1,"window":{"days":null,"cutoff":null},"total_attempts":0,` +
		`"first_ts":null,"last_ts":null,"counts":{"harness":{},"model":{},"session_mode":{},"posture":{}},` +
		`"skipped_rows":0}`
	if string(rawEmpty) != wantEmpty {
		t.Fatalf("empty aggregate =\n%s\nwant\n%s", rawEmpty, wantEmpty)
	}
}
