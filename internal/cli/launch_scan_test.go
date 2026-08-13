package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestReplaceLaunchSection_PreservesTrailingCommentSectionAndLeadingComments
// is the regression test for the corruption security review and code review
// both flagged: the old scanner recognized a table header only when a
// trimmed line both started with "[" AND ended with "]" — a header carrying
// a trailing comment ("[bench] # local collector only") never ends in "]",
// so the scanner never noticed the section boundary and kept treating every
// following line (including the unrelated [bench] section and its own
// leading comment block, misattributed to the wrong section) as still
// belonging to the preceding [launch] table — silently dropping all of it
// from the rewrite. This asserts the hardened scanner resolves the boundary
// correctly: the comment-block-then-commented-header [bench] section
// survives verbatim, and only the [launch] family is actually replaced.
func TestReplaceLaunchSection_PreservesTrailingCommentSectionAndLeadingComments(t *testing.T) {
	body := `[launch.defaults]
model = "opus"

[[launch.project]]
match = "~/Projects/minute"
model = "sonnet"

# local collector only — see docs/bench.md
[bench] # local collector only
hearth_dir = "~/Projects/hearth"
telemetry = true
`
	merged := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus"},
		Projects: []config.LaunchProject{{Match: "~/Projects/minute", Model: "haiku"}},
	}
	data, err := renderReplacedLaunch([]byte(body), merged, "/legacy/claunch.conf")
	if err != nil {
		t.Fatalf("renderReplacedLaunch: %v", err)
	}
	got := string(data)

	for _, want := range []string{
		"# local collector only — see docs/bench.md",
		"[bench] # local collector only",
		`hearth_dir = "~/Projects/hearth"`,
		"telemetry = true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten config.toml missing %q (the naive scanner drops everything after a header with a trailing comment); got:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "[bench]"); n != 1 {
		t.Errorf("rewritten config.toml has %d [bench] headers, want exactly 1 (no duplication, no drop-then-reappend); got:\n%s", n, got)
	}
	// The old project entry (model = sonnet) must be GONE — replaced by the
	// merged block's model = haiku — proving [launch] itself was still
	// correctly rewritten, not just accidentally preserved alongside [bench].
	if strings.Contains(got, "sonnet") {
		t.Errorf("rewritten config.toml still carries the pre-merge project (model = sonnet); want it replaced by the merged block; got:\n%s", got)
	}
	if !strings.Contains(got, "haiku") {
		t.Errorf("rewritten config.toml missing the merged project's model %q; got:\n%s", "haiku", got)
	}
}

func TestRenderers_RejectInvalidUTF8PathBeforeTOMLCommentInsertion(t *testing.T) {
	invalid := string([]byte{'/', 't', 'm', 'p', '/', 0xff})
	for name, render := range map[string]func() ([]byte, error){
		"import": func() ([]byte, error) { return renderImportedLaunch(nil, config.LaunchConfig{}, invalid) },
		"replace": func() ([]byte, error) {
			return renderReplacedLaunch([]byte("[launch]\n"), config.LaunchConfig{}, invalid)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := render(); !errors.Is(err, config.ErrLegacyPathControl) {
				t.Fatalf("error=%v, want invalid path refusal", err)
			}
		})
	}
}

// TestReplaceLaunchSection_AmbiguousScanRefusesAndLeavesFileUntouched covers
// the fail-safe contract security review required alongside the scanner
// hardening: when the scan can't unambiguously resolve the file's structure
// — here, config.toml ends mid multi-line array, the exact "unterminated
// multi-line string/array at EOF" example called out as the scanner's
// remaining gap — replaceLaunchSection MUST return an error and MUST NOT
// write anything, rather than guess at which lines belong to [launch].
func TestReplaceLaunchSection_AmbiguousScanRefusesAndLeavesFileUntouched(t *testing.T) {
	body := `[launch.defaults]
model = "opus"

[bench]
roots = [
  "a",
  "b",
`
	merged := config.LaunchConfig{Defaults: config.LaunchDefaults{Model: "haiku"}}
	if _, err := renderReplacedLaunch([]byte(body), merged, "/legacy/claunch.conf"); err == nil {
		t.Fatal("replaceLaunchSection succeeded against a file ending mid multi-line array, want a refusal error")
	}
}

// TestTomlLineScanner_HeaderDetection is a focused unit test on the scanner
// itself (launch_scan.go), covering the specific shapes the naive
// prefix/suffix check misread: a trailing comment on the header line, a "#"
// character living inside a quoted string value (must not be treated as a
// comment start), and a line that is itself inside a still-open multi-line
// array (must never be treated as a header, however it's shaped).
func TestTomlLineScanner_HeaderDetection(t *testing.T) {
	t.Run("trailing comment on header line", func(t *testing.T) {
		var s tomlLineScanner
		table, ok := s.scanLine(`[bench] # local collector only`)
		if !ok {
			t.Fatal("scanLine reported ambiguous for a plain trailing-comment header")
		}
		if table != "bench" {
			t.Errorf("table = %q, want %q", table, "bench")
		}
	})

	t.Run("hash inside a quoted value is not a comment", func(t *testing.T) {
		var s tomlLineScanner
		table, ok := s.scanLine(`match = "~/Projects/a#b"`)
		if !ok {
			t.Fatal("scanLine reported ambiguous for a quoted value containing #")
		}
		if table != "" {
			t.Errorf("table = %q, want \"\" (this is a key/value line, not a header)", table)
		}
	})

	t.Run("line inside a still-open multi-line array is never a header", func(t *testing.T) {
		var s tomlLineScanner
		if _, ok := s.scanLine(`roots = [`); !ok {
			t.Fatal("scanLine reported ambiguous for the array-opening line")
		}
		table, ok := s.scanLine(`  ["nested.looking.entry"],`)
		if !ok {
			t.Fatal("scanLine reported ambiguous for a line inside an open array")
		}
		if table != "" {
			t.Errorf("table = %q, want \"\" (mid-array line must never be read as a header)", table)
		}
		if _, ok := s.scanLine(`]`); !ok {
			t.Fatal("scanLine reported ambiguous for the array-closing line")
		}
		if s.pending() {
			t.Error("scanner still reports a pending array after the closing line")
		}
	})
}
