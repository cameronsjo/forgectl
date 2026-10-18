// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A script that carries its own "# WHAT:" and "# WHY:" lines must be
// queueable (#1278): the file's line is used when the flag is absent, and the
// queued bytes are the source bytes unchanged.
func TestDeskAdd_FileHeaderAndFlagCombinations(t *testing.T) {
	const body = "#!/bin/bash\n# WHAT: merge it\n# WHY: you own merges\necho hi\n"
	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantSame bool   // queued bytes equal the source file
		wantErr  string // substring of stderr on refusal
	}{
		{"file only", nil, 0, true, ""},
		{"file only, padded values", nil, 0, true, ""},
		{"flags match the file", []string{"--what", "merge it", "--why", "you own merges"}, 0, true, ""},
		{"what differs", []string{"--what", "something else"}, 1, false, "differs from --what"},
		{"why differs", []string{"--why", "another reason"}, 1, false, "differs from --why"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			newDeskDir(t)
			b := body
			if strings.Contains(c.name, "padded") {
				b = "#!/bin/bash\n# WHAT: merge it \n# WHY:\tyou own merges  \necho hi\n"
			}
			src := writeTemp(t, "m.sh", b)
			args := append([]string{"add", src, "--json"}, c.args...)
			out, errOut, err := deskRun(t, deskDeps(), args...)
			wantExit(t, err, c.wantExit)
			if c.wantExit != 0 {
				if !strings.Contains(errOut+errText(err), c.wantErr) {
					t.Errorf("refusal %q / %q lacks %q", errOut, errText(err), c.wantErr)
				}
				return
			}
			var a deskAddJSON
			if jerr := json.Unmarshal([]byte(out), &a); jerr != nil {
				t.Fatalf("add --json: %v\n%s", jerr, out)
			}
			got, rerr := os.ReadFile(a.Path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if c.wantSame && string(got) != b {
				t.Errorf("queued bytes differ from the source:\n%q\nwant\n%q", got, b)
			}
		})
	}
}

// A flag still fills a header the file lacks, and a missing header with no
// flag stays a usage error.
func TestDeskAdd_FlagFillsMissingHeaderOnly(t *testing.T) {
	newDeskDir(t)
	src := writeTemp(t, "p.sh", "# WHAT: from the file\necho hi\n")
	_, _, err := deskRun(t, deskDeps(), "add", src)
	wantExit(t, err, deskExitUsage) // no WHY anywhere
	out, _, err := deskRun(t, deskDeps(), "add", src, "--why", "a reason", "--json")
	wantExit(t, err, 0)
	var a deskAddJSON
	if jerr := json.Unmarshal([]byte(out), &a); jerr != nil {
		t.Fatal(jerr)
	}
	got, _ := os.ReadFile(a.Path)
	if !strings.Contains(string(got), "# WHAT: from the file\n") || !strings.Contains(string(got), "# WHY: a reason\n") {
		t.Errorf("queued = %q", got)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// queuedEntries counts what is in the desk's pending/ dir, dot-named temp
// files aside; a refused add must leave it at zero.
func queuedEntries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "pending"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

// The header parser's edges, through the CLI: the first WHAT line wins, an
// indented line is no header, CRLF and a WHAT deep in the file still count, and
// a padded file value matches a trimmed flag. A refusal queues nothing.
func TestDeskAdd_HeaderParserEdges(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		args     []string
		wantExit int
		wantErr  string
	}{
		{"two WHATs, no flag uses the first", "# WHAT: first\n# WHAT: second\n# WHY: r\necho\n", nil, 0, ""},
		{"two WHATs, flag matches the first", "# WHAT: first\n# WHAT: second\n# WHY: r\necho\n", []string{"--what", "first"}, 0, ""},
		{"two WHATs, flag matches only the second", "# WHAT: first\n# WHAT: second\n# WHY: r\necho\n", []string{"--what", "second"}, 1, "differs from --what"},
		{"indented WHAT is no header", "  # WHAT: indented\n# WHY: r\necho\n", nil, deskExitUsage, "--what is required"},
		{"indented WHAT does not match a flag either", "  # WHAT: indented\n# WHY: r\necho\n", []string{"--what", "indented"}, 0, ""},
		{"CRLF, no flags", "#!/bin/bash\r\n# WHAT: merge it\r\n# WHY: r\r\necho\r\n", nil, 0, ""},
		{"CRLF, matching flags", "#!/bin/bash\r\n# WHAT: merge it\r\n# WHY: r\r\necho\r\n", []string{"--what", "merge it", "--why", "r"}, 0, ""},
		{"CRLF, differing flag", "#!/bin/bash\r\n# WHAT: merge it\r\n# WHY: r\r\necho\r\n", []string{"--what", "other"}, 1, "differs from --what"},
		{"WHAT not at the top, no flag", "#!/bin/bash\nset -e\necho start\n# WHAT: deep\n# WHY: r\necho end\n", nil, 0, ""},
		{"WHAT not at the top, differing flag", "#!/bin/bash\nset -e\necho start\n# WHAT: deep\n# WHY: r\necho end\n", []string{"--what", "shallow"}, 1, "differs from --what"},
		{"padded file values, matching flags", "# WHAT: merge it \t\n# WHY:\tyou own merges  \necho\n", []string{"--what", "merge it", "--why", "you own merges"}, 0, ""},
		{"padded file values, padded matching flags", "# WHAT: merge it \n# WHY: r\necho\n", []string{"--what", "  merge it  "}, 0, ""},
		{"padded file value, differing flag", "# WHAT: merge it \n# WHY: r\necho\n", []string{"--what", "merge"}, 1, "differs from --what"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newDeskDir(t)
			src := writeTemp(t, "e.sh", c.body)
			args := append([]string{"add", src, "--json"}, c.args...)
			out, errOut, err := deskRun(t, deskDeps(), args...)
			wantExit(t, err, c.wantExit)
			if c.wantExit != 0 {
				if !strings.Contains(errOut+errText(err), c.wantErr) {
					t.Errorf("refusal %q / %q lacks %q", errOut, errText(err), c.wantErr)
				}
				if n := queuedEntries(t, dir); n != 0 {
					t.Errorf("a refused add left %d entries in pending/", n)
				}
				return
			}
			var a deskAddJSON
			if jerr := json.Unmarshal([]byte(out), &a); jerr != nil {
				t.Fatalf("add --json: %v\n%s", jerr, out)
			}
			got, rerr := os.ReadFile(a.Path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if strings.Contains(c.name, "does not match") {
				return // the flag fills the missing header, so the bytes grow
			}
			if string(got) != c.body {
				t.Errorf("queued bytes differ from the source:\n%q\nwant\n%q", got, c.body)
			}
		})
	}
}

// A header with only whitespace after the colon names nothing: it is refused
// like a missing one, and a flag may fill it. Nothing is queued on refusal.
func TestDeskAdd_WhitespaceOnlyHeaderCountsAsEmpty(t *testing.T) {
	cases := []struct {
		name string
		body string
		args []string
		want string
	}{
		{"tab-only WHAT", "# WHAT:\t\n# WHY: r\necho\n", nil, "--what is required"},
		{"spaces-and-tab WHAT", "# WHAT: \t \n# WHY: r\necho\n", nil, "--what is required"},
		{"tab-only WHY", "# WHAT: w\n# WHY:\t\necho\n", nil, "--why is required"},
		{"CRLF tab-only WHY", "# WHAT: w\r\n# WHY:\t\r\necho\r\n", nil, "--why is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newDeskDir(t)
			src := writeTemp(t, "w.sh", c.body)
			_, errOut, err := deskRun(t, deskDeps(), append([]string{"add", src}, c.args...)...)
			wantExit(t, err, deskExitUsage)
			if !strings.Contains(errOut+errText(err), c.want) {
				t.Errorf("refusal %q / %q lacks %q", errOut, errText(err), c.want)
			}
			if n := queuedEntries(t, dir); n != 0 {
				t.Errorf("a refused add left %d entries in pending/", n)
			}
		})
	}

	// The flag fills a blank header; the blank line stays behind the new one.
	dir := newDeskDir(t)
	src := writeTemp(t, "f.sh", "# WHAT:\t\n# WHY: r\necho\n")
	out, _, err := deskRun(t, deskDeps(), "add", src, "--what", "filled", "--json")
	wantExit(t, err, 0)
	var a deskAddJSON
	if jerr := json.Unmarshal([]byte(out), &a); jerr != nil {
		t.Fatal(jerr)
	}
	got, _ := os.ReadFile(a.Path)
	if !strings.HasPrefix(string(got), "# WHAT: filled\n") {
		t.Errorf("queued = %q; want the flag's WHAT first", got)
	}
	if n := queuedEntries(t, dir); n == 0 {
		t.Error("the filled add queued nothing")
	}
}
