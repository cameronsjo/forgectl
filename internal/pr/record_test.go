package pr

// Test plan for record.go (forgectl#299 Task 1)
//
// writeRecordAtomic (Classification: crash-safe writer, fault-injected seam)
//   [x] Happy: writes the record; the success path calls SyncDir on the
//       directory (asserted through the fake's call log, not inferred)
//   [x] Each injected failure — open, short write, sync, rename, dirsync —
//       leaves no temp file behind and never replaces the destination
//   [x] Compare-and-write: create-only refuses an existing destination;
//       expectRevision>0 refuses a mismatched on-disk revision; an ABSENT
//       destination with expectRevision>0 is a mismatch, not a create
//   [x] A destination that is a symlink is refused before any write

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func v2Record(t *testing.T, rev int) []byte {
	t.Helper()
	bc := Breadcrumb{
		Workspace: "/tmp/forgectl-workflow-x", Ref: "o/r#1", Agent: "claude",
		CreatedAt: fixedTime(), Version: 2, Phase: PhasePrepared, Revision: rev,
	}
	data, err := encodeBreadcrumb(bc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

func TestWriteRecordAtomic_HappyPathSyncsDirectory(t *testing.T) {
	dir := t.TempDir()
	fake := newFaultFS(t, "")
	if err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 1), expectNewRecord); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.json")) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(got), `"revision": 1`) {
		t.Errorf("record on disk lacks revision: %s", got)
	}
	if !fake.called("SyncDir") {
		t.Errorf("SyncDir never called on the success path; calls = %v", fake.calls)
	}
	if leftovers := tempFiles(t, dir); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestWriteRecordAtomic_InjectedFailuresLeaveNoTemp(t *testing.T) {
	for _, step := range []string{"OpenExclusive", "Write", "Sync", "Rename", "SyncDir"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			// Seed a destination so a failed rename can be shown not to clobber it.
			seed := v2Record(t, 1)
			if err := os.WriteFile(filepath.Join(dir, "a.json"), seed, 0o600); err != nil {
				t.Fatal(err)
			}
			fake := newFaultFS(t, step)
			err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 2), 1)
			if step == "SyncDir" {
				// After a successful rename the new revision is authoritative even
				// when the directory sync reports uncertainty; the error is still
				// surfaced so the caller can log it.
				if err == nil {
					t.Fatalf("expected the dirsync error to surface")
				}
			} else if err == nil {
				t.Fatalf("expected failure at %s", step)
			}
			if leftovers := tempFiles(t, dir); len(leftovers) != 0 {
				t.Errorf("temp files left behind after %s failure: %v", step, leftovers)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "a.json")) //nolint:gosec // test-owned temp dir
			if step != "SyncDir" && string(got) != string(seed) {
				t.Errorf("destination changed after %s failure", step)
			}
		})
	}
}

func TestWriteRecordAtomic_CompareAndWrite(t *testing.T) {
	dir := t.TempDir()
	fake := newFaultFS(t, "")
	path := filepath.Join(dir, "a.json")

	if err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 1), expectNewRecord); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 1), expectNewRecord); !errors.Is(err, errRecordRevisionMismatch) {
		t.Errorf("create over existing = %v, want errRecordRevisionMismatch", err)
	}
	if err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 3), 2); !errors.Is(err, errRecordRevisionMismatch) {
		t.Errorf("expect 2 against on-disk 1 = %v, want errRecordRevisionMismatch", err)
	}
	if err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 2), 1); err != nil {
		t.Fatalf("expect 1 against on-disk 1: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 3), 2); !errors.Is(err, errRecordRevisionMismatch) {
		t.Errorf("absent destination with expectRevision 2 = %v, want errRecordRevisionMismatch (a torn-down record must not resurrect)", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent destination was recreated by a mismatched write")
	}
}

func TestWriteRecordAtomic_RefusesSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, v2Record(t, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "a.json")); err != nil {
		t.Fatal(err)
	}
	fake := newFaultFS(t, "")
	err := writeRecordAtomic(fake, dir, "a.json", v2Record(t, 2), 1)
	if err == nil {
		t.Fatal("expected refusal on a symlinked destination")
	}
	if fake.called("Rename") {
		t.Error("rename issued against a symlinked destination")
	}
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}
