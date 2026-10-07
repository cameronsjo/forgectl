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

// PrunePlan lists exactly what Prune removes, in the same order, and removes
// nothing itself.
func TestPrunePlanMatchesPruneAndWritesNothing(t *testing.T) {
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

	plan, err := d.PrunePlan(30)
	if err != nil {
		t.Fatalf("PrunePlan: %v", err)
	}
	if after := tree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("PrunePlan changed the desk:\nbefore %v\nafter  %v", before, after)
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
	if left, err := d.PrunePlan(30); err != nil || len(left) != 0 {
		t.Errorf("after Prune the plan still lists %v (err %v)", left, err)
	}
	if _, err := d.PrunePlan(0); err == nil {
		t.Error("PrunePlan(0) should refuse, as Prune does")
	}
}
