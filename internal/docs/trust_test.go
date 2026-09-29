package docs

// Test plan for trust.go
//
// evalTrust (Classification: pure function)
//   [x] Unhappy: stale_after yesterday is stale; tomorrow is not
//   [x] Unhappy: stale_after exactly now is stale (>=, not >)
//   [x] Unhappy: a +02:00 offset is compared as an instant, not as a string
//   [x] Happy: date-only, offset-less, garbage, and empty values are never stale
//   [x] Happy: values time.Parse accepts but RFC 3339's grammar rejects (comma
//       fraction, +24:00, offset minute 60, one-digit hour) are never stale;
//       year 0000 is grammatical and is
//   [x] Unhappy: only the exact lowercase "deprecated" is deprecated
//
// frontmatterTrust (Classification: parser)
//   [x] Happy: a TOML (+++) block yields nothing
//   [x] Happy: date-only and quoted values come back as raw authored text
//   [x] Happy: a non-scalar or alias value yields nothing

import (
	"testing"
	"time"
)

// withTrustNow pins trustNow for one test. It is the only writer of trustNow
// in the package's tests; no test here runs in parallel.
func withTrustNow(t *testing.T, now time.Time) {
	t.Helper()
	prev := trustNow
	trustNow = func() time.Time { return now }
	t.Cleanup(func() { trustNow = prev })
}

var trustTestNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func TestEvalTrust_StaleAfter(t *testing.T) {
	cases := []struct {
		name, staleAfter string
		want             bool
	}{
		{"yesterday", "2026-09-28T12:00:00Z", true},
		{"tomorrow", "2026-09-30T12:00:00Z", false},
		{"boundary", "2026-09-29T12:00:00Z", true},
		// Offsets compare as instants. A naive string compare gets both
		// of these backwards: 13:30+02:00 is 11:30Z (stale) and
		// 14:30+02:00 is 12:30Z (fresh).
		{"offset_past", "2026-09-29T13:30:00+02:00", true},
		{"offset_future", "2026-09-29T14:30:00+02:00", false},
		{"fractional", "2026-09-28T12:00:00.5Z", true},
		{"date_only_past", "2026-01-01", false},
		{"no_offset_past", "2026-01-01T00:00:00", false},
		{"garbage", "garbage", false},
		// time.Parse accepts these; RFC 3339's grammar does not.
		{"comma_fraction", "2026-09-28T12:00:00,5Z", false},
		{"offset_24h", "2026-09-28T12:00:00+24:00", false},
		{"offset_minute_60", "2026-09-28T12:00:00+00:60", false},
		{"one_digit_hour", "2026-09-28T1:00:00Z", false},
		{"lowercase_t", "2026-09-28t12:00:00Z", false},
		// date-fullyear is 4DIGIT, so year 0000 is valid, and long past.
		{"year_0000", "0000-01-01T00:00:00Z", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalTrust("", tc.staleAfter, trustTestNow)
			if got.Stale != tc.want {
				t.Errorf("evalTrust(%q).Stale = %v, want %v", tc.staleAfter, got.Stale, tc.want)
			}
			if got.Stale && got.StaleAfter != tc.staleAfter {
				t.Errorf("StaleAfter = %q, want the raw %q", got.StaleAfter, tc.staleAfter)
			}
			if !got.Stale && got.StaleAfter != "" {
				t.Errorf("StaleAfter = %q on a fresh doc, want empty", got.StaleAfter)
			}
		})
	}
}

func TestEvalTrust_Status(t *testing.T) {
	for status, want := range map[string]bool{
		"deprecated": true,
		"Deprecated": false,
		"DEPRECATED": false,
		"stable":     false,
		"draft":      false,
		"":           false,
	} {
		if got := evalTrust(status, "", trustTestNow).Deprecated; got != want {
			t.Errorf("evalTrust(status %q).Deprecated = %v, want %v", status, got, want)
		}
	}
}

func TestFrontmatterTrust(t *testing.T) {
	cases := []struct {
		name, src          string
		status, staleAfter string
	}{
		{"yaml", "---\nstatus: deprecated\nstale_after: 2026-09-28T00:00:00Z\n---\n# T\n", "deprecated", "2026-09-28T00:00:00Z"},
		{"date_only_raw", "---\nstale_after: 2026-01-01\n---\n# T\n", "", "2026-01-01"},
		{"quoted_raw", "---\nstale_after: '2026-09-28T00:00:00Z'\n---\n# T\n", "", "2026-09-28T00:00:00Z"},
		// The +++ body is YAML-shaped on purpose: only the delimiter
		// scoping, not a parse failure, may be what drops it.
		{"toml", "+++\nstatus: deprecated\nstale_after: 2026-09-28T00:00:00Z\n+++\n# T\n", "", ""},
		{"non_scalar", "---\nstatus: [deprecated]\nstale_after: {at: x}\n---\n# T\n", "", ""},
		// An alias node's Value is the anchor NAME, never the value.
		{"alias", "---\na: &deprecated stable\nstatus: *deprecated\n---\n# T\n", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm, ok := splitFrontmatter([]byte(tc.src))
			if !ok {
				t.Fatalf("splitFrontmatter found no block in %q", tc.src)
			}
			status, staleAfter := frontmatterTrust(fm)
			if status != tc.status || staleAfter != tc.staleAfter {
				t.Errorf("frontmatterTrust = (%q, %q), want (%q, %q)", status, staleAfter, tc.status, tc.staleAfter)
			}
		})
	}
}
