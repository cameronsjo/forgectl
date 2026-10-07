// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// addJSON runs `desk add FILE --json` with the flags given and decodes it.
func addJSON(t *testing.T, file string, extra ...string) (deskAddJSON, string, error) {
	t.Helper()
	args := append([]string{"add", file, "--what", "a test item", "--why", "a test", "--json"}, extra...)
	out, errOut, err := deskRun(t, deskDeps(), args...)
	var a deskAddJSON
	if err == nil {
		if jerr := json.Unmarshal([]byte(out), &a); jerr != nil {
			t.Fatalf("add --json: %v\n%s", jerr, out)
		}
	}
	return a, errOut, err
}

// pendingFiles lists the .sh files waiting in the desk.
func pendingFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "pending"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sh") {
			names = append(names, e.Name())
		}
	}
	return names
}

// A retry of `desk add` finds the item the first attempt queued instead of
// queueing a second (forgectl#1088): same name, exit 0, duplicate true.
func TestDeskAdd_RetryFindsTheQueuedItem(t *testing.T) {
	dir := newDeskDir(t)
	file := writeTemp(t, "x.sh", "echo hi\n")

	first, _, err := addJSON(t, file)
	if err != nil || first.Duplicate {
		t.Fatalf("first add = %+v err=%v, want a new item", first, err)
	}
	again, _, err := addJSON(t, file)
	wantExit(t, err, 0)
	if !again.Duplicate || again.Name != first.Name || again.SHA256 != first.SHA256 || again.Path != first.Path {
		t.Errorf("retry = %+v, want the first item %+v with duplicate true", again, first)
	}
	if got := pendingFiles(t, dir); !reflect.DeepEqual(got, []string{first.Name + ".sh"}) {
		t.Errorf("pending = %v, want the one item", got)
	}
}

func TestDeskAdd_DuplicateTextSaysSo(t *testing.T) {
	dir := newDeskDir(t)
	file := writeTemp(t, "x.sh", "echo hi\n")
	args := []string{"add", file, "--what", "w", "--why", "y"}
	first, _, err := deskRun(t, deskDeps(), args...)
	wantExit(t, err, 0)
	if strings.Contains(first, "duplicate") {
		t.Errorf("a new item's output mentions duplicate: %q", first)
	}

	out, errOut, err := deskRun(t, deskDeps(), args...)
	wantExit(t, err, 0)
	if out != first+"duplicate=true\n" {
		t.Errorf("duplicate output = %q, want the first output plus duplicate=true", out)
	}
	if !strings.Contains(errOut, "already waiting") || !strings.Contains(errOut, "--allow-duplicate") {
		t.Errorf("stderr = %q, want a note naming the escape", errOut)
	}
	if got := pendingFiles(t, dir); len(got) != 1 {
		t.Errorf("pending = %v, want one item", got)
	}
}

// --allow-duplicate queues anyway; a different --what is a different item; and
// an item that is no longer waiting does not block the same file.
func TestDeskAdd_WhatIsNotADuplicate(t *testing.T) {
	dir := newDeskDir(t)
	file := writeTemp(t, "x.sh", "echo hi\n")
	first, _, err := addJSON(t, file)
	if err != nil {
		t.Fatal(err)
	}

	forced, _, err := addJSON(t, file, "--allow-duplicate")
	if err != nil || forced.Duplicate || forced.Name == first.Name {
		t.Errorf("--allow-duplicate = %+v err=%v, want a second item", forced, err)
	}

	out, _, err := deskRun(t, deskDeps(), "add", file, "--what", "a different what", "--why", "a test", "--json")
	wantExit(t, err, 0)
	var other deskAddJSON
	if err := json.Unmarshal([]byte(out), &other); err != nil {
		t.Fatal(err)
	}
	if other.Duplicate || other.Name == first.Name {
		t.Errorf("a different --what = %+v, want a new item", other)
	}

	_, _, err = deskRun(t, deskDeps(), "skip", first.Name, "--reason", "done with it")
	wantExit(t, err, 0)
	_, _, err = deskRun(t, deskDeps(), "skip", forced.Name, "--reason", "done with it")
	wantExit(t, err, 0)
	again, _, err := addJSON(t, file)
	if err != nil || again.Duplicate {
		t.Errorf("after both were skipped: %+v err=%v, want a new item", again, err)
	}
	if got := pendingFiles(t, dir); len(got) != 2 {
		t.Errorf("pending = %v, want the different-what item and the new one", got)
	}
}

// `desk skip` on an item already skipped says so and exits 0; a name that
// never existed is still an error that lists the waiting items.
func TestDeskSkip_AlreadySkippedIsNotAnError(t *testing.T) {
	newDeskDir(t)
	keep, _ := queueItem(t, "keep.sh", "echo keep\n")
	gone, _ := queueItem(t, "gone.sh", "echo gone\n")

	out, errOut, err := deskRun(t, deskDeps(), "skip", gone, "--reason", "first")
	wantExit(t, err, 0)
	if out != "skipped="+gone+" reason=operator\n" || errOut != "" {
		t.Fatalf("first skip: out %q err %q", out, errOut)
	}

	out, errOut, err = deskRun(t, deskDeps(), "skip", gone, "--reason", "second try")
	wantExit(t, err, 0)
	if out != "skipped="+gone+" reason=operator already=true\n" {
		t.Errorf("retry output = %q, want already=true", out)
	}
	if !strings.Contains(errOut, "was already skipped") {
		t.Errorf("retry stderr = %q", errOut)
	}
	// The first skip's note stands: a retry does not overwrite it.
	detail, _, err := deskRun(t, deskDeps(), "status", gone)
	wantExit(t, err, 0)
	if !strings.Contains(detail, "skip_note=first\n") || strings.Contains(detail, "second try") {
		t.Errorf("the retry changed the recorded skip: %q", detail)
	}
	// The file name a caller took from a JSON path resolves to the skipped item too.
	out, _, err = deskRun(t, deskDeps(), "skip", gone+".sh", "--reason", "third")
	wantExit(t, err, 0)
	if !strings.Contains(out, "already=true") {
		t.Errorf("skip %s.sh = %q, want already=true", gone, out)
	}

	_, _, err = deskRun(t, deskDeps(), "skip", "66-nosuch", "--reason", "x")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "no waiting item or lost run named 66-nosuch") || !strings.Contains(err.Error(), "waiting: "+keep) {
		t.Errorf("a name that never existed: %v", err)
	}
}

// plantOldDone makes a done/ item whose newest file is days old.
func plantOldDone(t *testing.T, dir, name string, days int) {
	t.Helper()
	old := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	for _, ext := range []string{".sh", ".log"} {
		p := filepath.Join(dir, "done", name+ext)
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func doneFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "done"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// `desk prune --dry-run` lists what a prune would delete and deletes nothing;
// the real prune then removes exactly that.
func TestDeskPrune_DryRunListsAndDeletesNothing(t *testing.T) {
	dir := newDeskDir(t)
	openTestDesk(t, dir)
	plantOldDone(t, dir, "01-old", 40)
	plantOldDone(t, dir, "02-older", 90)
	plantOldDone(t, dir, "03-recent", 2)
	before := doneFiles(t, dir)

	out, _, err := deskRun(t, deskDeps(), "prune", "--dry-run")
	wantExit(t, err, 0)
	if !strings.HasPrefix(out, "would_prune=2 days=30\n") || !strings.Contains(out, "done/01-old newest=") || !strings.Contains(out, "done/02-older newest=") || strings.Contains(out, "03-recent") {
		t.Errorf("dry-run output = %q", out)
	}
	if after := doneFiles(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("the dry run deleted files:\nbefore %v\nafter  %v", before, after)
	}

	jsonOut, _, err := deskRun(t, deskDeps(), "prune", "--dry-run", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(jsonOut)), []string{"days", "dry_run", "items", "would_remove"}; !reflect.DeepEqual(got, want) {
		t.Errorf("--dry-run --json keys = %v, want %v", got, want)
	}
	var plan deskPrunePlanJSON
	if err := json.Unmarshal([]byte(jsonOut), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.WouldRemove != 2 || len(plan.Items) != 2 || plan.Items[0].State != "done" || plan.Items[0].Name != "01-old" || plan.Items[0].Newest.IsZero() {
		t.Errorf("plan = %+v", plan)
	}

	realOut, _, err := deskRun(t, deskDeps(), "prune")
	wantExit(t, err, 0)
	if realOut != "pruned=2 days=30\n" {
		t.Errorf("real prune = %q, want the count the plan listed", realOut)
	}
	if left := doneFiles(t, dir); !reflect.DeepEqual(left, []string{"03-recent.log", "03-recent.sh"}) {
		t.Errorf("after prune: %v", left)
	}

	// A usage error is a usage error with or without the preview.
	_, _, err = deskRun(t, deskDeps(), "prune", "--dry-run", "--days", "0")
	wantExit(t, err, deskExitUsage)
}

// A duplicate add queued nothing, so it tells the operator nothing: no herdr
// notification, no pane state, no macOS notification. The first add signals
// once; the retry adds no second ping to the operator's day.
func TestDeskAdd_DuplicateSignalsNothing(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	file := writeTemp(t, "merge.sh", "echo merge\n")
	args := []string{"add", file, "--what", "w", "--why", "y"}

	if _, _, err := deskRun(t, rig.deps, args...); err != nil {
		t.Fatal(err)
	}
	if got := rig.kinds(); got != "herdr.notification-show herdr.pane-agent" || rig.osascript() != 1 {
		t.Fatalf("first add signalled %q and %d macOS notifications; the control is wrong", got, rig.osascript())
	}
	callsAfterFirst := len(rig.herdr.Calls())

	if _, _, err := deskRun(t, rig.deps, args...); err != nil {
		t.Fatal(err)
	}
	if got := len(rig.herdr.Calls()); got != callsAfterFirst || rig.osascript() != 1 {
		t.Errorf("the duplicate add signalled: %d herdr calls (was %d), %d macOS notifications (was 1)", got, callsAfterFirst, rig.osascript())
	}

	// --allow-duplicate queues a second item, and that one does signal.
	if _, _, err := deskRun(t, rig.deps, append(args, "--allow-duplicate")...); err != nil {
		t.Fatal(err)
	}
	if rig.osascript() != 2 {
		t.Errorf("--allow-duplicate sent %d macOS notifications, want 2 in all", rig.osascript())
	}
}
