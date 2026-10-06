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

	// The log and events paths `desk status --json` prints end in .log and
	// .events; they name the same item.
	for _, ext := range []string{".sh", ".log", ".events"} {
		out, _, err = deskRun(t, deskDeps(), "show", doneName+ext)
		wantExit(t, err, 0)
		if !strings.Contains(out, doneName) {
			t.Errorf("show %s output = %q, want the run", ext, out)
		}
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
	if !strings.Contains(err.Error(), "item names carry no extension") || strings.Contains(err.Error(), "try ") {
		t.Errorf("error = %q, want the extension note and no `try` for a stem that does not exist", err)
	}

	_, _, err = deskRun(t, deskDeps(), "status", "66-none.sh")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "waiting: "+waiting) {
		t.Errorf("status error = %q, want it to list the waiting items", err)
	}

	_, _, err = deskRun(t, deskDeps(), "show", "66-none.sh")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "item names carry no extension") {
		t.Errorf("show error = %q, want the extension note", err)
	}
}

// `desk show` with neither a name nor --log used to say "not both"; the
// caller gave neither.
func TestDeskShow_NeitherNameNorLogNamesTheFix(t *testing.T) {
	newDeskDir(t)
	_, _, err := deskRun(t, deskDeps(), "show")
	wantExit(t, err, deskExitUsage)
	want := "give a run name or --log FILE; usage: forgectl desk show <name> [flags]"
	if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %v, want it to contain %q and not say \"not both\"", err, want)
	}
	_, _, err = deskRun(t, deskDeps(), "show", "01-x", "--log", "/tmp/none.jsonl")
	wantExit(t, err, deskExitUsage)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %v, want the both-given message kept", err)
	}
}

func TestDeskShow_NotFoundListsRuns(t *testing.T) {
	dir := newDeskDir(t)
	name, sha := queueItem(t, "ran.sh", "true\n")
	finishRun(t, openTestDesk(t, dir), name, sha, 0)
	_, _, err := deskRun(t, deskDeps(), "show", "66-none")
	wantExit(t, err, 1)
	if err == nil || !strings.Contains(err.Error(), "runs: "+name) {
		t.Errorf("error = %v, want it to list the known runs", err)
	}
}

// desk add takes one file, or "-" for stdin: the help must not say "FILE ...".
func TestDeskHelp_AddArity(t *testing.T) {
	cmd := newDeskCmd(deskDeps())
	if strings.Contains(cmd.Long, "add FILE ...") {
		t.Error("desk --help says `add FILE ...`, but desk add takes one file")
	}
	if !strings.Contains(cmd.Long, "desk add FILE|-") {
		t.Error("desk --help does not show `desk add FILE|-`")
	}
}

func TestTrimDeskExt(t *testing.T) {
	for in, want := range map[string]string{
		"01-x.sh": "01-x", "01-x.manifest": "01-x", "01-x": "", "01-x.sh.sh": "01-x.sh", ".sh": "", "01-x.log": "01-x", "01-x.events": "01-x",
	} {
		if got := trimDeskExt(in); got != want {
			t.Errorf("trimDeskExt(%q) = %q, want %q", in, got, want)
		}
	}
}
