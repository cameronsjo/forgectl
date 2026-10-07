//go:build unix

package desk

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tree lists every path under root, so a test can prove a preview wrote
// nothing.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, strings.TrimPrefix(p, root))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// AddUnique queues a new item once and, for the same file with the same what
// and why, finds it again instead of queueing a second (forgectl#1088).
func TestAddUniqueFindsTheItemAlreadyWaiting(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "x.sh")
	writeFile(t, src, "echo hi\n", 0o600)

	first, dup, err := d.AddUnique(src, "what", "why", false)
	if err != nil || dup {
		t.Fatalf("first AddUnique = %+v dup=%v err=%v, want a new item", first, dup, err)
	}
	before := tree(t, d.Path())

	again, dup, err := d.AddUnique(src, "what", "why", false)
	if err != nil {
		t.Fatalf("second AddUnique: %v", err)
	}
	if !dup || again.Name != first.Name || again.SHA256 != first.SHA256 || again.Path != first.Path {
		t.Errorf("second AddUnique = %+v dup=%v, want the first item %+v with dup true", again, dup, first)
	}
	if after := tree(t, d.Path()); !reflect.DeepEqual(before, after) {
		t.Errorf("a duplicate add changed the desk:\nbefore %v\nafter  %v", before, after)
	}
}

// A different what, a different body, or a different kind is a different item.
func TestAddUniqueQueuesWhatIsNotIdentical(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "x.sh")
	writeFile(t, src, "echo hi\n", 0o600)
	first, _, err := d.AddUnique(src, "what", "why", false)
	if err != nil {
		t.Fatal(err)
	}
	other, dup, err := d.AddUnique(src, "another what", "why", false)
	if err != nil || dup || other.Name == first.Name {
		t.Errorf("a different --what: %+v dup=%v err=%v, want a new item", other, dup, err)
	}
	writeFile(t, src, "echo bye\n", 0o600)
	other, dup, err = d.AddUnique(src, "what", "why", false)
	if err != nil || dup || other.Name == first.Name {
		t.Errorf("a different body: %+v dup=%v err=%v, want a new item", other, dup, err)
	}
}

// Only an item that is still waiting counts. Once it is skipped, the same file
// is queued again; plain Add never looks.
func TestAddUniqueIgnoresItemsThatAreNotWaiting(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "x.sh")
	writeFile(t, src, "echo hi\n", 0o600)
	first, _, err := d.AddUnique(src, "what", "why", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SkipNoted(first.Name, "no longer needed"); err != nil {
		t.Fatal(err)
	}
	again, dup, err := d.AddUnique(src, "what", "why", false)
	if err != nil || dup || again.Name == first.Name {
		t.Errorf("after a skip: %+v dup=%v err=%v, want a new item", again, dup, err)
	}
	third, err := d.Add(src, "what", "why", false)
	if err != nil || third.Name == again.Name {
		t.Errorf("Add must queue even with an identical item waiting: %+v err=%v", third, err)
	}
}

// SkippedMeta tells an item in skipped/ from a name that never existed.
func TestSkippedMeta(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "x.sh")
	writeFile(t, src, "echo hi\n", 0o600)
	a, err := d.Add(src, "what", "why", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.SkippedMeta(a.Name); ok {
		t.Error("a waiting item reads as skipped")
	}
	if _, ok := d.SkippedMeta("99-none"); ok {
		t.Error("a name that never existed reads as skipped")
	}
	if _, ok := d.SkippedMeta("../escape"); ok {
		t.Error("an unsafe name reads as skipped")
	}
	if _, err := d.SkipNoted(a.Name, "why not"); err != nil {
		t.Fatal(err)
	}
	meta, ok := d.SkippedMeta(a.Name)
	if !ok || meta.SkipReason != SkipOperator || meta.SkipNote != "why not" {
		t.Errorf("SkippedMeta = %+v ok=%v, want the operator skip with its note", meta, ok)
	}
}

// PrunePlanAt lists exactly what Prune removes, in the same order, and removes
// nothing itself.
func TestPrunePlanAtMatchesPruneAndWritesNothing(t *testing.T) {
	d := openDesk(t)
	root := d.Path()
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, rel := range []string{"done/01-a.sh", "done/01-a.log", "done/02-b.log", "skipped/03-c.sh", "skipped/03-c.meta.json"} {
		p := filepath.Join(root, rel)
		writeFile(t, p, "x\n", 0o600)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "done/04-new.log"), "EXIT=0\n", 0o600)
	writeFile(t, filepath.Join(root, "pending/05-wait.sh"), "x\n", 0o600)
	before := tree(t, root)

	plan, err := PrunePlanAt(d.Path(), 30)
	if err != nil {
		t.Fatalf("PrunePlanAt: %v", err)
	}
	if after := tree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("PrunePlanAt changed the desk:\nbefore %v\nafter  %v", before, after)
	}
	var got []string
	for _, p := range plan {
		got = append(got, p.State+"/"+p.Name)
		if p.Newest.IsZero() {
			t.Errorf("%s/%s has no newest time", p.State, p.Name)
		}
	}
	if want := []string{"done/01-a", "done/02-b", "skipped/03-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("plan = %v, want %v", got, want)
	}

	n, err := d.Prune(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(plan) {
		t.Errorf("Prune removed %d items, the plan listed %d", n, len(plan))
	}
	if left, err := PrunePlanAt(d.Path(), 30); err != nil || len(left) != 0 {
		t.Errorf("after Prune the plan still lists %v (err %v)", left, err)
	}
	if _, err := PrunePlanAt(d.Path(), 0); err == nil {
		t.Error("PrunePlanAt(0) should refuse, as Prune does")
	}
}

// pendingHas reports whether pending/ holds name.
func pendingHas(t *testing.T, d *Desk, name string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(d.Path(), DirPending, name)) //nolint:gosec // G703: a fixture path under t.TempDir
	return err == nil
}

// An item is never visible in pending/ without its meta: Add writes the meta
// first. A retry that finds an item with no meta may then read "no meta" as a
// hand-dropped file. The hooks read pending/ at both edges of the link.
func TestAddWritesMetaBeforeTheItemIsVisible(t *testing.T) {
	d := openDesk(t)
	var seen []string
	beforeLink = func(file string) {
		seen = append(seen, "before:item="+boolStr(pendingHas(t, d, file))+",meta="+boolStr(pendingHas(t, d, strings.TrimSuffix(file, ".sh")+".meta.json")))
	}
	afterLink = func(file string) {
		seen = append(seen, "after:item="+boolStr(pendingHas(t, d, file))+",meta="+boolStr(pendingHas(t, d, strings.TrimSuffix(file, ".sh")+".meta.json")))
	}
	t.Cleanup(func() { beforeLink = func(string) {}; afterLink = func(string) {} })

	addScript(t, d, "job.sh", "echo one\n")
	want := []string{"before:item=no,meta=yes", "after:item=yes,meta=yes"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("pending/ at the link edges = %v, want %v", seen, want)
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// A crash between the meta and the link leaves a meta and no item. That is
// harmless: the item is not visible, a retry queues it normally under the next
// number, and the retry's item reads as not yet signalled.
func TestAddCrashBeforeTheLinkLeavesNoItemAndTheRetryWorks(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "job.sh")
	writeFile(t, src, "echo hi\n", 0o600)
	crash := true
	beforeLink = func(string) {
		if crash {
			crash = false
			panic("simulated death between the meta and the link")
		}
	}
	t.Cleanup(func() { beforeLink = func(string) {} })

	func() {
		defer func() { _ = recover() }()
		_, _, _ = d.AddUnique(src, "what", "why", false)
	}()
	if crash {
		t.Fatal("the crash hook never ran")
	}
	if pendingHas(t, d, "01-job.sh") {
		t.Fatal("an item became visible although the add died before the link")
	}
	if !pendingHas(t, d, "01-job.meta.json") {
		t.Fatal("the control is wrong: the crash left no meta behind, so the test does not see the window")
	}
	if _, err := d.Scan(); err != nil {
		t.Fatalf("a stray meta broke Scan: %v", err)
	}

	a, dup, err := d.AddUnique(src, "what", "why", false)
	if err != nil || dup {
		t.Fatalf("retry = %+v dup=%v err=%v, want a new item", a, dup, err)
	}
	if a.Name != "02-job" {
		t.Errorf("retry queued %s, want 02-job (the stray meta keeps 01-job taken)", a.Name)
	}
	if d.Signalled(a.Name) {
		t.Error("a freshly queued item reads as signalled")
	}
	if err := d.MarkSignalled(a.Name); err != nil {
		t.Fatal(err)
	}
	if !d.Signalled(a.Name) {
		t.Error("MarkSignalled did not stick")
	}
}

// A hand-dropped file has no meta and reads as signalled, so a retry never
// starts pinging for it.
func TestSignalledReadsAHandDroppedFileAsSignalled(t *testing.T) {
	d := openDesk(t)
	writeFile(t, filepath.Join(d.Path(), DirPending, "07-hand.sh"), "echo hand\n", 0o600)
	if !d.Signalled("07-hand") {
		t.Error("a hand-dropped item with no meta reads as unsignalled")
	}
	if err := d.MarkSignalled("07-hand"); err != nil {
		t.Fatal(err)
	}
	if pendingHas(t, d, "07-hand.meta.json") {
		t.Error("MarkSignalled gave a hand-dropped item a partial meta")
	}
}

// Exists is true for a desk and false for a directory that is not one, and it
// writes nothing either way.
func TestExistsAndPrunePlanAtWriteNothingInANonDeskDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: the test needs a wide mode to see it left alone
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "stray.txt"), "keep\n", 0o644)
	if Exists(dir) {
		t.Fatal("a non-desk directory reads as a desk")
	}
	before := dirState(t, dir)
	plan, err := PrunePlanAt(dir, 30)
	if err != nil || len(plan) != 0 {
		t.Fatalf("PrunePlanAt on a non-desk dir = %v, %v", plan, err)
	}
	if after := dirState(t, dir); after != before {
		t.Errorf("PrunePlanAt changed a non-desk dir:\nbefore %s\nafter  %s", before, after)
	}

	d := openDesk(t)
	if !Exists(d.Path()) {
		t.Error("an open desk does not read as a desk")
	}
}

// dirState is the mode, the entry names and every entry's mtime.
func dirState(t *testing.T, dir string) string {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := fi.Mode().String() + "|" + fi.ModTime().String()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out += "|" + e.Name() + ":" + info.Mode().String() + ":" + info.ModTime().String()
	}
	return out
}
