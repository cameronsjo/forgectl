// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"strings"
	"testing"
)

// The JSON `path` of a queued item ends in the file's extension while its
// `name` does not, so a caller that takes the path's basename holds
// "01-x.sh". Every verb that takes a name resolves it (forgectl#1087).
func TestDeskName_FileNameResolvesToTheItem(t *testing.T) {
	dir := newDeskDir(t)
	skipMe, _ := queueItem(t, "skipme.sh", "true\n")
	statusMe, _ := queueItem(t, "statusme.sh", "true\n")
	doneName, doneSHA := queueItem(t, "doneme.sh", "true\n")
	finishRun(t, openTestDesk(t, dir), doneName, doneSHA, 0)

	out, _, err := deskRun(t, deskDeps(), "skip", skipMe+".sh", "--reason", "r")
	wantExit(t, err, 0)
	if !strings.HasPrefix(out, "skipped="+skipMe+" ") {
		t.Errorf("skip output = %q, want the item's own name", out)
	}

	out, _, err = deskRun(t, deskDeps(), "status", statusMe+".sh")
	wantExit(t, err, 0)
	if !strings.HasPrefix(out, "name="+statusMe+"\n") {
		t.Errorf("status output = %q", out)
	}

	out, _, err = deskRun(t, deskDeps(), "show", doneName+".sh")
	wantExit(t, err, 0)
	if !strings.Contains(out, doneName) {
		t.Errorf("show output = %q, want the run", out)
	}
}

func TestDeskName_NotFoundNamesTheFix(t *testing.T) {
	newDeskDir(t)
	waiting, _ := queueItem(t, "waiter.sh", "true\n")

	_, _, err := deskRun(t, deskDeps(), "skip", "66-none", "--reason", "x")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "waiting: "+waiting) {
		t.Errorf("error = %q, want it to list the waiting items", err)
	}
	if strings.Contains(err.Error(), "carry no extension") {
		t.Errorf("error = %q hints at an extension the name never had", err)
	}

	_, _, err = deskRun(t, deskDeps(), "skip", "66-none.sh", "--reason", "x")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "item names carry no extension; try 66-none") {
		t.Errorf("error = %q, want the extension hint", err)
	}

	_, _, err = deskRun(t, deskDeps(), "status", "66-none.sh")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "waiting: "+waiting) {
		t.Errorf("status error = %q, want it to list the waiting items", err)
	}

	_, _, err = deskRun(t, deskDeps(), "show", "66-none.sh")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "try 66-none") {
		t.Errorf("show error = %q, want the extension hint", err)
	}
}

func TestTrimDeskExt(t *testing.T) {
	for in, want := range map[string]string{
		"01-x.sh": "01-x", "01-x.manifest": "01-x", "01-x": "", "01-x.sh.sh": "01-x.sh", ".sh": "",
	} {
		if got := trimDeskExt(in); got != want {
			t.Errorf("trimDeskExt(%q) = %q, want %q", in, got, want)
		}
	}
}
