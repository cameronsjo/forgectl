//go:build unix

package desk

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStepStateNamesMatchTheRunner pins the exported, cross-platform step
// states to the runner's own spellings.
func TestStepStateNamesMatchTheRunner(t *testing.T) {
	for exported, runner := range map[string]string{
		StepPending: stepPending, StepRunning: stepRunning, StepOK: stepOK,
		StepFailed: stepFailed, StepCancelled: stepCancelled, StepSkipped: stepSkipped,
	} {
		if exported != runner {
			t.Errorf("exported %q, runner %q", exported, runner)
		}
	}
}

func TestBatchStatusReadsTheRunnersFile(t *testing.T) {
	d := openDesk(t)
	if _, err := d.BatchStatus("01-batch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no status yet: err = %v, want ErrNotFound", err)
	}
	if _, err := d.BatchStatus("../x"); err == nil {
		t.Fatal("a bad name was accepted")
	}
	writeFile(t, filepath.Join(d.Path(), DirDone, "01-batch.d", "status.tsv"),
		"step\tstate\trc\tstart\tend\tdeps\na\tok\t0\t-\t-\t-\nb\trunning\t-\t-\t-\ta\n", 0o600)
	got, err := d.BatchStatus("01-batch")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].State != StepRunning {
		t.Errorf("BatchStatus = %+v", got)
	}
}

func TestRecordLooksInRunningThenDoneThenSkipped(t *testing.T) {
	d := openDesk(t)
	if _, _, err := d.Record("01-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	writeFile(t, filepath.Join(d.Path(), DirSkipped, "01-x.sh"), "skipped\n", 0o600)
	writeFile(t, filepath.Join(d.Path(), DirDone, "01-x.manifest"), "done\n", 0o600)
	data, kind, err := d.Record("01-x")
	if err != nil || string(data) != "done\n" || kind != KindBatch {
		t.Fatalf("Record = %q %s %v, want done/'s manifest", data, kind, err)
	}
	writeFile(t, filepath.Join(d.Path(), DirRunning, "01-x.sh"), "running\n", 0o600)
	if data, _, _ := d.Record("01-x"); string(data) != "running\n" {
		t.Errorf("Record = %q, want running/'s copy first", data)
	}
}

func TestLogTailKeepsTheEnd(t *testing.T) {
	d := openDesk(t)
	if _, _, err := d.LogTail("01-x", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	writeFile(t, filepath.Join(d.Path(), DirDone, "01-x.log"), strings.Repeat("a", 100)+"tail\n", 0o600)
	data, cut, err := d.LogTail("01-x", 5)
	if err != nil || string(data) != "tail\n" || !cut {
		t.Fatalf("LogTail = %q cut=%v err=%v", data, cut, err)
	}
	data, cut, err = d.LogTail("01-x", 1000)
	if err != nil || len(data) != 105 || cut {
		t.Fatalf("whole log: %d bytes cut=%v err=%v", len(data), cut, err)
	}
	// A symlink planted at the log name is not followed out of the desk.
	outside := filepath.Join(t.TempDir(), "secret")
	writeFile(t, outside, "secret\n", 0o600)
	if err := os.Symlink(outside, filepath.Join(d.Path(), DirDone, "02-y.log")); err != nil {
		t.Fatal(err)
	}
	if data, _, err := d.LogTail("02-y", 100); err == nil {
		t.Errorf("a symlink out of the desk was read: %q", data)
	}
}
