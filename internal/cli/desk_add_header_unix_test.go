// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"encoding/json"
	"os"
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
