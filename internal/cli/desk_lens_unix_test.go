// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testLens = `
about  = "nightly backup"
format = "text"
[text]
pattern = '^(?P<time>\S+) (?P<level>\w+) (?P<event>.*)$'
[[rule]]
action = "ignore"
match  = '^heartbeat'
[[rule]]
action = "start"
match  = '^backing up (?P<step>\S+)'
[[rule]]
action = "close"
match  = '^backed up (?P<step>\S+)'
[[rule]]
action = "fail"
match  = '^(?P<step>\S+) failed'
[[rule]]
action = "end"
match  = '^done rc=(?P<exit>\d+)'
[[rule]]
action = "skip"
match  = '^never'
step   = "x"
`

const testLensLog = `2026-10-07T01:00:00Z INFO starting
2026-10-07T01:00:01Z INFO backing up photos
2026-10-07T01:00:02Z DEBUG heartbeat
2026-10-07T01:03:00Z INFO backed up photos
2026-10-07T01:03:01Z INFO backing up docs
2026-10-07T01:03:05Z WARN slow upload
2026-10-07T01:03:06Z WARN slow upload
2026-10-07T01:04:00Z ERROR docs failed: reset
2026-10-07T01:04:01Z INFO done rc=1
`

// lensHome points the lenses directory at a temp config dir holding name,
// and returns the directory.
func lensHome(t *testing.T, name, body string) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg) // macOS reads ~/Library/Application Support
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "forgectl", "lenses")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDeskShow_LensGivesALogSteps(t *testing.T) {
	newDeskDir(t)
	lensHome(t, "backup", testLens)
	p := writeTemp(t, "backup.log", testLensLog)
	out, _, err := deskRunASCII(t, "show", "--log", p, "--lens", "backup")
	wantExit(t, err, 0)
	for _, want := range []string{"log/backup.log · exit 1", "+ photos  done · 2m59s", "x docs    failed · 59s", "8 events · 1 line ignored"} {
		if !strings.Contains(out, want) {
			t.Errorf("show --lens is missing %q:\n%s", want, out)
		}
	}

	out, _, err = deskRun(t, deskDeps(), "show", "--log", p, "--lens", "backup", "--json")
	wantExit(t, err, 0)
	var js deskShowJSON
	if err := json.Unmarshal([]byte(out), &js); err != nil {
		t.Fatal(err)
	}
	if js.Live != "ended" || js.Exit == nil || *js.Exit != 1 || len(js.Steps) != 2 || js.Counts.Ignored != 1 {
		t.Errorf("show --lens --json = live %s exit %v steps %d ignored %d", js.Live, js.Exit, len(js.Steps), js.Counts.Ignored)
	}

	out, _, err = deskRunASCII(t, "runs", "--log", p, "--lens", "backup")
	wantExit(t, err, 0)
	if !strings.Contains(out, "log:backup.log") || !strings.Contains(out, "1/2 steps, 1 failed") {
		t.Errorf("runs --lens should count the lens's steps:\n%s", out)
	}
}

func TestDeskShow_LensUsage(t *testing.T) {
	newDeskDir(t)
	lensHome(t, "backup", testLens)
	p := writeTemp(t, "backup.log", testLensLog)
	for _, args := range [][]string{
		{"show", "01-x", "--lens", "backup"},
		{"show", "--log", p, "--lens", "backup", "--event-key", "kind"},
		{"show", "--log", p, "--lens", ""},
		{"show", "--log", p, "--lens", "missing"},
		{"show", "--log", p, "--lens", "backup", "--live", "--json"},
		{"show", "--log", p, "--lens", "backup", "--live"}, // no terminal in a test
		{"lens", "check", "backup"},
	} {
		_, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, 2)
	}
	bad := writeTemp(t, "bad.toml", "format='text'\n[[rule]]\naction='start'\nmatch='('\n")
	_, _, err := deskRun(t, deskDeps(), "show", "--log", p, "--lens", bad)
	wantExit(t, err, 2)
	if err == nil || !strings.Contains(err.Error(), "[[rule]] 1: match") {
		t.Errorf("a bad lens should name the rule to fix: %v", err)
	}
}

func TestDeskLensCheck(t *testing.T) {
	newDeskDir(t)
	lensHome(t, "backup", testLens)
	p := writeTemp(t, "backup.log", testLensLog)
	out, _, err := deskRun(t, deskDeps(), "lens", "check", "backup", "--log", p)
	wantExit(t, err, 0)
	for _, want := range []string{
		"lens backup · text · 6 rules",
		"read: 8 events · 1 ignored",
		"skip   '^never' step=x  · never matched",
		"steps: photos, docs",
		"2×  slow upload",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lens check is missing %q:\n%s", want, out)
		}
	}

	out, _, err = deskRun(t, deskDeps(), "lens", "check", "backup", "--log", p, "--json")
	wantExit(t, err, 0)
	var c lensCheckJSON
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.Unmatched != 3 || c.Rules[0].Hits != 1 || c.Rules[1].Hits != 2 || c.Rules[5].Hits != 0 || c.UnmatchedTop[0].Count != 2 {
		t.Errorf("lens check --json = %+v", c)
	}
}

func TestDeskLensList(t *testing.T) {
	newDeskDir(t)
	dir := lensHome(t, "backup", testLens)
	out, _, err := deskRun(t, deskDeps(), "lens", "list")
	wantExit(t, err, 0)
	for _, want := range []string{"lenses: " + dir, "backup  text  6 rules  nightly backup", "events  json  0 rules"} {
		if !strings.Contains(out, want) {
			t.Errorf("lens list is missing %q:\n%s", want, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.toml"), []byte("format='x'"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = deskRun(t, deskDeps(), "lens", "list")
	wantExit(t, err, 1)
}

// The built-in events lens reads a translator's output: the line's own
// action and exit fold the run.
func TestDeskShow_EventsLens(t *testing.T) {
	newDeskDir(t)
	lensHome(t, "other", testLens)
	p := writeTemp(t, "run.jsonl", strings.Join([]string{
		`{"event":"fetching","step":"fetch","action":"start"}`,
		`{"event":"fetched","step":"fetch","action":"close"}`,
		`{"event":"building","step":"build","action":"start"}`,
		`{"event":"boom","step":"build","action":"fail"}`,
		`{"event":"done","action":"end","exit":2}`,
	}, "\n")+"\n")
	out, _, err := deskRunASCII(t, "show", "--log", p, "--lens", "events")
	wantExit(t, err, 0)
	for _, want := range []string{"· exit 2", "+ fetch  done", "x build  failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("show --lens events is missing %q:\n%s", want, out)
		}
	}
}

// A pattern that splits no line is the silent failure check must not pass
// over; a lens with no rules is how a pattern is checked first.
func TestDeskLensCheck_NamesAPatternThatSplitsNothing(t *testing.T) {
	newDeskDir(t)
	lensHome(t, "wrong", "format = 'text'\n[text]\npattern = '^(?P<time>\\S+) (?P<event>.*)$'\n")
	p := writeTemp(t, "a.log", "2026-10-07 09:00:00.001 INFO user 17 logged in\n\n2026-10-07 09:00:01.002 INFO user 23 logged in\n")
	lensHome(t, "spaced", "format = 'text'\n[text]\npattern = '^(?P<time>\\S+ \\S+) (?P<level>\\w+) (?P<event>.*)$'\n")
	out, _, err := deskRun(t, deskDeps(), "lens", "check", "spaced", "--log", p)
	wantExit(t, err, 0)
	for _, want := range []string{"format: 2 of 2 lines the pattern split", "2×  user # logged in"} {
		if !strings.Contains(out, want) {
			t.Errorf("lens check is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning:") {
		t.Errorf("a pattern that splits every line is no warning:\n%s", out)
	}

	bad := writeTemp(t, "wrong.toml", "format = 'text'\n[text]\npattern = '^(?P<level>[a-z]+):'\n")
	out, _, err = deskRun(t, deskDeps(), "lens", "check", bad, "--log", p)
	wantExit(t, err, 0)
	if !strings.Contains(out, "format: 0 of 2 lines the pattern split") || !strings.Contains(out, "warning: no line fit the format") {
		t.Errorf("a pattern that splits nothing should say so:\n%s", out)
	}
}
