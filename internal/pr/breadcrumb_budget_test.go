package pr

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// TestSettleDrainFailure_AnEscapeDenseErrorStillParks is #963 A: a drain
// failure whose error text is escape-dense must still park the record once
// its attempts are exhausted. Before the fix, LastError and the
// exhausted-attempts RepairReason each kept 1280 runes of '<', which
// json.MarshalIndent writes as six bytes apiece, so the record passed
// maxBreadcrumbRecordBytes, the park returned claim-failure, and the record
// stayed in preparing.
//
// Mutation that turns it red: make breadcrumbText return recordText(s).
func TestSettleDrainFailure_AnEscapeDenseErrorStillParks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
	}{
		{"html-escaped", strings.Repeat("<", 5000)},
		{"four-byte emoji", strings.Repeat("\U0001F600", 5000)},
		{"control escapes", strings.Repeat("\x1b", 5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ref := testRef(6)
			c := drainClient(t, dir, drainLaunchRunner(nil)) // no window
			path := seedPhaseRecord(t, c, ref, PhasePreparing, "")

			outcome, toPhase := c.settleDrainFailure(context.Background(), ref, path, 2, 3, errors.New(tc.cause))
			if outcome != drainOutcomeNeedsRepair || toPhase != string(PhaseNeedsRepair) {
				t.Fatalf("outcome = %q/%q, want needs-repair: an escape-dense error must not overflow the record", outcome, toPhase)
			}
			bc := readRecord(t, path)
			if bc.Phase != PhaseNeedsRepair || bc.Attempts != 3 {
				t.Fatalf("record = phase %q attempts %d, want needs-repair/3", bc.Phase, bc.Attempts)
			}
			if !strings.HasPrefix(bc.RepairReason, "drain: 3 attempts, last: ") {
				t.Errorf("repairReason = %.60q…; want the attempts summary kept", bc.RepairReason)
			}
		})
	}
}

// TestBreadcrumbWorstCaseFitsTheRecordLimit is #963 A's bound: with every
// string field at once at its worst — both free-text fields fed each hostile
// shape through breadcrumbText, and each structured field at the longest
// value this build writes (a PATH_MAX workspace, a 253-byte host, the longest
// ref and window id) — the encoded record still fits
// maxBreadcrumbRecordBytes. The structured fields are written by forgectl
// itself, so they are long here but plain; only the free-text fields carry
// arbitrary text.
//
// Mutations that turn it red: raise breadcrumbTextMaxBytes to 2048; make
// breadcrumbText return recordText(s); count '<' as one byte in termsafe's
// jsonStringBytes.
func TestBreadcrumbWorstCaseFitsTheRecordLimit(t *testing.T) {
	const maxInt = "9223372036854775807"
	hostile := map[string]string{
		"html-escaped":    strings.Repeat("<", 5000),
		"four-byte emoji": strings.Repeat("\U0001F600", 5000),
		"control escapes": strings.Repeat("\x1b\u202e", 5000),
		"quotes":          strings.Repeat("\"\\", 5000),
		"invalid bytes":   strings.Repeat("\xff", 5000),
		"mixed":           strings.Repeat("a<\U0001F600\x1b&\u2028\"\\\xff", 2000),
	}
	for name, text := range hostile {
		t.Run(name, func(t *testing.T) {
			bc := Breadcrumb{
				Workspace:    "/" + strings.Repeat("w", 4094),
				Ref:          strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100) + "#" + maxInt,
				Host:         strings.Repeat("h", 253),
				Agent:        strings.Repeat("a", 64),
				CreatedAt:    time.Now().UTC(),
				Provenance:   "operator-authored",
				Version:      breadcrumbVersion,
				Phase:        PhaseNeedsRepair,
				Revision:     1 << 62,
				WindowID:     maxInt + tmux.FieldSep + maxInt + tmux.FieldSep + "@" + maxInt,
				RepairReason: breadcrumbText("drain: " + strconv.Itoa(1<<30) + " attempts, last: " + breadcrumbText(text)),
				Attempts:     1 << 62,
				LastError:    breadcrumbText(text),
				LastAttempt:  time.Now().UTC(),
			}
			data, err := encodeBreadcrumb(bc)
			if err != nil {
				t.Fatalf("encode the worst-case record: %v (a record must never outgrow %d bytes)", err, maxBreadcrumbRecordBytes)
			}
			if len(data) > maxBreadcrumbRecordBytes {
				t.Fatalf("record is %d bytes, over %d", len(data), maxBreadcrumbRecordBytes)
			}
		})
	}
}

// TestBreadcrumbTextNeverCutsToEmpty is #974 item 9: SafeLineMaxJSON cuts a
// value to "" when its byte cap is below TruncatedMarker's encoded size, and
// an empty RepairReason fails a needs-repair record's validation. So a cut
// breadcrumb text must keep the marker at least.
//
// Mutation that turns it red: set breadcrumbTextMaxBytes to 15, one byte
// under the marker.
func TestBreadcrumbTextNeverCutsToEmpty(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 5000), strings.Repeat("<", 5000), strings.Repeat("\U0001F600", 5000)} {
		if got := breadcrumbText(text); !strings.HasSuffix(got, termsafe.TruncatedMarker) {
			t.Errorf("breadcrumbText(%.10q…) = %.40q; want a cut value ending in the marker, never empty", text, got)
		}
	}
}
