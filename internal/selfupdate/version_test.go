package selfupdate

import (
	"strings"
	"testing"
)

// TestFindVersions pins the whole-token rule: a version counts only when the
// entire token is one, so no row yields a truncated piece of a longer token
// (#738: `\b` treated "." as a boundary, reading 0.9.0_1 as 0.9).
func TestFindVersions(t *testing.T) {
	for in, want := range map[string]string{
		"sops 3.13.3 (latest)":                       "3.13.3",
		"sops v3.13.3":                               "3.13.3",
		"forgectl 1.0.0-rc.1 available":              "1.0.0-rc.1",
		"build abc1.2.3def":                          "",
		"x 1234567.1":                                "",
		"sops 3.13.3-AAAAAAAAAAAAAAAAAAAAAAAAAAAA":   "",
		"forgectl 0.9.0_1 installed":                 "0.9.0_1",
		"x 1.2.3.4.5 y":                              "",
		"x 1.2.3.4 y":                                "1.2.3.4",
		"x 1.0.0+build.5 y":                          "1.0.0+build.5",
		"x 1.0.0-rc.1+abc y":                         "1.0.0-rc.1+abc",
		"now at 2.1.0.":                              "2.1.0",
		"cameronsjo/tap/forgectl (0.9.0) != 0.10.0":  "0.9.0,0.10.0",
		"cameronsjo/tap/forgectl (0.9.0_1) < 0.10.0": "0.9.0_1,0.10.0",
		"x 1.2.3\x1b[2J":                             "1.2.3",
		"x 1.2.3_" + strings.Repeat("9", 5):          "",
	} {
		got := strings.Join(FindVersions(in), ",")
		if got != want {
			t.Errorf("FindVersions(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOutdatedDetail: the detail is rebuilt from two version tokens, and any
// other brew output reads as the fixed category.
func TestOutdatedDetail(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // G101: version strings, not credentials
		"cameronsjo/tap/forgectl (0.9.0) != 0.10.0":                 "forgectl 0.9.0 installed, 0.10.0 available",
		"cameronsjo/tap/forgectl (0.9.0_1) < 0.10.0":                "forgectl 0.9.0_1 installed, 0.10.0 available",
		"cameronsjo/tap/forgectl":                                   "a newer forgectl is available",
		"\x1b]8;;https://evil\x07forgectl (0.9.0) != 0.10.0 SECRET": "forgectl 0.9.0 installed, 0.10.0 available",
		"forgectl (1.2.3.4.5) != 0.10.0":                            "a newer forgectl is available",
	} {
		if got := OutdatedDetail(in); got != want {
			t.Errorf("OutdatedDetail(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUpgradedVersions pins #761's success line: from and to come from version
// tokens on a line naming forgectl, the upgrade step's line wins over brew
// update's, and anything else reads as not-found.
func TestUpgradedVersions(t *testing.T) {
	for _, tc := range []struct {
		in       string
		from, to string
		ok       bool
	}{
		{"==> Upgrading 1 outdated package:\ncameronsjo/tap/forgectl 1.0.0 -> 1.1.0", "1.0.0", "1.1.0", true},
		{"Updated 2 taps.\nother/tap/thing 3.0.0 -> 4.0.0\n\ncameronsjo/tap/forgectl 1.0.0 -> 1.1.0", "1.0.0", "1.1.0", true},
		{"cameronsjo/tap/forgectl 9.9.9 -> 9.9.10\n\ncameronsjo/tap/forgectl 1.0.0 -> 1.1.0", "1.0.0", "1.1.0", true},
		{"\x1b]0;x\x07cameronsjo/tap/forgectl 1.0.0 -> 1.1.0 SECRET\x1b[2J", "1.0.0", "1.1.0", true},
		{"other/tap/thing 3.0.0 -> 4.0.0", "", "", false},
		{"Warning: cameronsjo/tap/forgectl 1.1.0 is already installed", "", "", false},
		{"", "", "", false},
	} {
		from, to, ok := UpgradedVersions(tc.in)
		if from != tc.from || to != tc.to || ok != tc.ok {
			t.Errorf("UpgradedVersions(%q) = %q, %q, %v; want %q, %q, %v", tc.in, from, to, ok, tc.from, tc.to, tc.ok)
		}
	}
}
