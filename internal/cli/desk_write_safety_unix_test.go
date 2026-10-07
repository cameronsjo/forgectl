// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
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
	if !strings.HasSuffix(first, "duplicate=false\n") {
		t.Errorf("a new item's output = %q, want it to end duplicate=false (stable keys)", first)
	}

	out, errOut, err := deskRun(t, deskDeps(), args...)
	wantExit(t, err, 0)
	if out != strings.TrimSuffix(first, "duplicate=false\n")+"duplicate=true\n" {
		t.Errorf("duplicate output = %q, want the first output with duplicate=true", out)
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
	if out != "skipped="+gone+" reason=operator note=\"first\"\n" || errOut != "" {
		t.Fatalf("first skip: out %q err %q", out, errOut)
	}

	out, errOut, err = deskRun(t, deskDeps(), "skip", gone, "--reason", "second try")
	wantExit(t, err, 0)
	if out != "skipped="+gone+" reason=operator note=\"first\" already=true\n" {
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
	if !strings.HasPrefix(out, "would_prune=2 days=30 found=true\n") || !strings.Contains(out, "done/01-old newest=") || !strings.Contains(out, "done/02-older newest=") || strings.Contains(out, "03-recent") {
		t.Errorf("dry-run output = %q", out)
	}
	if after := doneFiles(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("the dry run deleted files:\nbefore %v\nafter  %v", before, after)
	}

	jsonOut, _, err := deskRun(t, deskDeps(), "prune", "--dry-run", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(jsonOut)), []string{"days", "dry_run", "found", "items", "would_remove"}; !reflect.DeepEqual(got, want) {
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

// If the first attempt never finished signalling (it died after queueing, or
// a signal failed), the retry finds the item and sends the signal, so the item
// does not wait with nobody told. Once it has gone out, a retry sends nothing.
func TestDeskAdd_RetryResignalsUntilTheFirstSignalWentOut(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	failing := true
	deskMacNotify = func(context.Context, module.Deps, string, string) error {
		rig.mac++
		if failing {
			return errors.New("notification centre unavailable")
		}
		return nil
	}
	file := writeTemp(t, "merge.sh", "echo merge\n")
	args := []string{"add", file, "--what", "w", "--why", "y"}

	_, errOut, err := deskRun(t, rig.deps, args...)
	wantExit(t, err, 0)
	if !strings.Contains(errOut, "operator signal failed") || rig.osascript() != 1 {
		t.Fatalf("first add: stderr %q, %d macOS attempts; the control is wrong", errOut, rig.osascript())
	}

	failing = false
	_, errOut, err = deskRun(t, rig.deps, args...)
	wantExit(t, err, 0)
	if rig.osascript() != 2 {
		t.Errorf("the retry made %d macOS attempts in all, want 2: the unsignalled item was not re-signalled", rig.osascript())
	}
	if !strings.Contains(errOut, "already waiting") || !strings.Contains(errOut, "had not gone out") {
		t.Errorf("retry stderr = %q, want it to say the signal was sent now", errOut)
	}

	if _, errOut, err = deskRun(t, rig.deps, args...); err != nil {
		t.Fatal(err)
	}
	if rig.osascript() != 2 || strings.Contains(errOut, "had not gone out") {
		t.Errorf("a retry after the signal went out signalled again: %d attempts, stderr %q", rig.osascript(), errOut)
	}
}

// A dry run creates nothing, not even the desk directory every other verb
// makes, and says the desk is absent rather than printing what an empty desk
// prints (forgectl#1088).
func TestDeskPrune_DryRunCreatesNothingOnAnAbsentDir(t *testing.T) {
	dir := newDeskDir(t)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the control desk dir exists: %v", err)
	}

	out, errOut, err := deskRun(t, deskDeps(), "prune", "--dry-run")
	wantExit(t, err, 0)
	if _, serr := os.Lstat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the dry run created the desk dir: %v", serr)
	}
	if out != "would_prune=0 days=30 found=false\n" {
		t.Errorf("output = %q, want found=false", out)
	}
	if !strings.Contains(errOut, "desk not found at") {
		t.Errorf("stderr = %q, want a note that the desk was not found", errOut)
	}

	out, _, err = deskRun(t, deskDeps(), "prune", "--dry-run", "--json")
	wantExit(t, err, 0)
	var plan deskPrunePlanJSON
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Found || !plan.DryRun || plan.WouldRemove != 0 || plan.Items == nil {
		t.Errorf("plan = %+v, want found=false with an empty items array", plan)
	}
	if _, serr := os.Lstat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("--json dry run created the desk dir: %v", serr)
	}

	// An existing desk reads found=true, so the two cases differ.
	openTestDesk(t, dir)
	out, _, err = deskRun(t, deskDeps(), "prune", "--dry-run")
	wantExit(t, err, 0)
	if out != "would_prune=0 days=30 found=true\n" {
		t.Errorf("existing empty desk output = %q", out)
	}
}

// desk skip reports the recorded --reason text as note=, quoted, next to the
// reason category, and --json carries the same plus already (forgectl#1088).
func TestDeskSkip_ReportsTheNoteAndHasJSON(t *testing.T) {
	newDeskDir(t)
	a, _ := queueItem(t, "a.sh", "echo a\n")
	b, _ := queueItem(t, "b.sh", "echo b\n")

	out, _, err := deskRun(t, deskDeps(), "skip", a, "--reason", `needs "review" first`)
	wantExit(t, err, 0)
	if out != "skipped="+a+` reason=operator note="needs \"review\" first"`+"\n" {
		t.Errorf("text = %q", out)
	}

	out, _, err = deskRun(t, deskDeps(), "skip", b, "--reason", "superseded", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"already", "name", "note", "reason"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	var first deskSkipJSON
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatal(err)
	}
	if first != (deskSkipJSON{Name: b, Reason: "operator", Note: "superseded"}) {
		t.Errorf("first = %+v", first)
	}

	out, _, err = deskRun(t, deskDeps(), "skip", b, "--reason", "a different reason", "--json")
	wantExit(t, err, 0)
	var again deskSkipJSON
	if err := json.Unmarshal([]byte(out), &again); err != nil {
		t.Fatal(err)
	}
	if again != (deskSkipJSON{Name: b, Reason: "operator", Note: "superseded", Already: true}) {
		t.Errorf("retry = %+v, want the first skip's recorded note with already true", again)
	}
}

// A skip of 03-x after the item was queued again as 05-x points at 05-x.
func TestDeskSkip_NotFoundNamesTheSameItemUnderAnotherNumber(t *testing.T) {
	newDeskDir(t)
	a, _ := queueItem(t, "same.sh", "echo a\n")
	_, _, err := deskRun(t, deskDeps(), "skip", a, "--reason", "first")
	wantExit(t, err, 0)
	b, _ := queueItem(t, "same.sh", "echo b\n")
	if a == b {
		t.Fatalf("both items are %s", a)
	}

	_, _, err = deskRun(t, deskDeps(), "skip", "09-same", "--reason", "x")
	wantExit(t, err, 1)
	for _, want := range []string{"same name under another number", a + " (skipped)", b + " (waiting)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	_, _, err = deskRun(t, deskDeps(), "skip", "09-other", "--reason", "x")
	wantExit(t, err, 1)
	if strings.Contains(err.Error(), "another number") {
		t.Errorf("a name no item shares got a similar-name hint: %q", err)
	}
}

// treeState is the mode and mtime of dir and everything under it, so a test can
// prove a command wrote, created or chmodded nothing.
func treeState(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		lines = append(lines, strings.TrimPrefix(p, dir)+":"+info.Mode().String()+":"+info.ModTime().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// The dry run is read-only on an EXISTING directory too: a 755 directory that
// is not a desk keeps its mode, its entries and its mtimes, and reads
// found=false; a desk whose subdirectories are 755 keeps those modes while the
// plan lists the same items a real prune then removes (forgectl#1088).
func TestDeskPrune_DryRunIsReadOnlyOnAnExistingDir(t *testing.T) {
	dir := newDeskDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: the test needs a wide mode to see it left alone
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: as above
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("not a desk\n"), 0o644); err != nil { //nolint:gosec // G306: as above
		t.Fatal(err)
	}
	before := treeState(t, dir)

	out, errOut, err := deskRun(t, deskDeps(), "prune", "--dry-run")
	wantExit(t, err, 0)
	if out != "would_prune=0 days=30 found=false\n" || !strings.Contains(errOut, "desk not found at") {
		t.Errorf("non-desk dir: out %q err %q, want found=false and a note", out, errOut)
	}
	if after := treeState(t, dir); after != before {
		t.Fatalf("the dry run changed a non-desk directory:\nbefore\n%s\nafter\n%s", before, after)
	}

	// A desk with wide modes: the plan is the real prune's, and nothing changes.
	desk := t.TempDir()
	for _, sub := range []string{"pending", "running", "done", "skipped"} {
		if err := os.MkdirAll(filepath.Join(desk, sub), 0o755); err != nil { //nolint:gosec // G301: as above
			t.Fatal(err)
		}
	}
	plantOldDone(t, desk, "01-old", 40)
	plantOldDone(t, desk, "02-new", 1)
	before = treeState(t, desk)
	out, _, err = deskRun(t, deskDeps(), "prune", "--dry-run", "--dir", desk)
	wantExit(t, err, 0)
	if !strings.HasPrefix(out, "would_prune=1 days=30 found=true\n") || !strings.Contains(out, "done/01-old") || strings.Contains(out, "02-new") {
		t.Errorf("desk plan = %q", out)
	}
	if after := treeState(t, desk); after != before {
		t.Fatalf("the dry run changed a desk (modes tightened or files touched):\nbefore\n%s\nafter\n%s", before, after)
	}
	realOut, _, err := deskRun(t, deskDeps(), "prune", "--dir", desk)
	wantExit(t, err, 0)
	if realOut != "pruned=1 days=30\n" {
		t.Errorf("real prune = %q, want the one item the plan listed", realOut)
	}
}

// A real prune on a missing desk creates nothing and says so, with the exit
// code unchanged; --json carries found (forgectl#1088).
func TestDeskPrune_RealPruneNeverCreatesAMissingDesk(t *testing.T) {
	dir := newDeskDir(t)
	out, errOut, err := deskRun(t, deskDeps(), "prune")
	wantExit(t, err, 0)
	if out != "pruned=0 days=30\n" || !strings.Contains(errOut, "desk not found at") {
		t.Errorf("out %q err %q, want pruned=0 and a not-found note", out, errOut)
	}
	if _, serr := os.Lstat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("a real prune created the desk dir: %v", serr)
	}

	out, _, err = deskRun(t, deskDeps(), "prune", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"days", "found", "removed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	var res deskPruneJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Found || res.Removed != 0 {
		t.Errorf("result = %+v, want found=false removed=0", res)
	}
	if _, serr := os.Lstat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("--json prune created the desk dir: %v", serr)
	}

	// A directory that exists but is not a desk is left alone too.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "stray.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, other)
	_, _, err = deskRun(t, deskDeps(), "prune", "--dir", other)
	wantExit(t, err, 0)
	if after := treeState(t, other); after != before {
		t.Errorf("a real prune opened a non-desk dir:\nbefore\n%s\nafter\n%s", before, after)
	}

	// A real desk reads found=true.
	live := t.TempDir()
	openTestDesk(t, live)
	out, _, err = deskRun(t, deskDeps(), "prune", "--dir", live, "--json")
	wantExit(t, err, 0)
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Found {
		t.Errorf("real desk result = %s (err %v), want found=true", out, err)
	}
}

// When the retry's own signal fails, the note says the item was already queued
// and the signal was not sent. It never says "sent now" next to a failure.
func TestDeskAdd_DuplicateWithAFailingSignalSaysSoPlainly(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p9")
	rig := newSignalRig(t, config.DeskConfig{})
	deskMacNotify = func(context.Context, module.Deps, string, string) error {
		rig.mac++
		return errors.New("notification centre unavailable")
	}
	args := []string{"add", writeTemp(t, "merge.sh", "echo merge\n"), "--what", "w", "--why", "y"}
	if _, _, err := deskRun(t, rig.deps, args...); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := deskRun(t, rig.deps, args...)
	wantExit(t, err, 0)
	if !strings.Contains(errOut, "already queued; signal not sent") || strings.Contains(errOut, "sent now") || strings.Contains(errOut, "nothing was queued, and") {
		t.Errorf("stderr = %q, want 'already queued; signal not sent' and no 'sent now'", errOut)
	}
	// The failure is reported once, as the warning line, not again in the note.
	if n := strings.Count(errOut, "macOS notification failed"); n != 1 {
		t.Errorf("the signal failure is printed %d times in %q, want once", n, errOut)
	}
}

// The help says what the code does: a duplicate re-sends the operator signal
// when the first attempt never finished signalling, and otherwise signals
// nothing.
func TestDeskAddHelpDescribesTheRetrySignal(t *testing.T) {
	newDeskDir(t)
	out, _, err := deskRun(t, deskDeps(), "add", "--help")
	wantExit(t, err, 0)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"only if the first attempt never finished signalling", "signalled_at", "otherwise signals nothing"} {
		if !strings.Contains(flat, want) {
			t.Errorf("desk add --help lacks %q", want)
		}
	}
	if strings.Contains(flat, "add queues nothing, signals nothing") {
		t.Error("desk add --help still says a duplicate signals nothing")
	}
}

// A protocol directory that is a symlink is refused by a real prune (Open
// refuses it), and the preview refuses it the same way instead of reading it
// as empty.
func TestDeskPrune_DryRunRefusesASymlinkedProtocolDirLikePrune(t *testing.T) {
	desk := t.TempDir()
	for _, sub := range []string{"pending", "running", "skipped"} {
		if err := os.MkdirAll(filepath.Join(desk, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(desk, "done")); err != nil {
		t.Fatal(err)
	}

	_, _, planErr := deskRun(t, deskDeps(), "prune", "--dry-run", "--dir", desk)
	_, _, realErr := deskRun(t, deskDeps(), "prune", "--dir", desk)
	if planErr == nil || realErr == nil {
		t.Fatalf("plan err = %v, real err = %v; both must refuse", planErr, realErr)
	}
	if !strings.Contains(planErr.Error(), "not a directory") || !strings.Contains(realErr.Error(), "not a directory") {
		t.Errorf("plan err %q, real err %q, want both to say not a directory", planErr, realErr)
	}
}
