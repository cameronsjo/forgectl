//go:build unix

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	state := t.TempDir()
	l, err := openAt(state, "/repo/one", "default")
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	return l, filepath.Join(state, "forgectl", "surface")
}

func TestLedgerBeginUpdateRows(t *testing.T) {
	l, dir := testLedger(t)
	start := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	if err := l.Begin(Row{Name: "w1", Branch: "feat/w1", StartedAt: start}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Update("w1", func(r *Row) {
		r.Stage = StageWorktree
		r.Worktree = "/repo/one/.claude/worktrees/w1"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rows, err := l.Rows()
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Stage != StageWorktree || rows[0].Worktree == "" || !rows[0].StartedAt.Equal(start) {
		t.Fatalf("rows = %+v", rows)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != ledgerFileMode {
			t.Errorf("%s mode = %v, want %v", e.Name(), info.Mode().Perm(), os.FileMode(ledgerFileMode))
		}
		if strings.Contains(e.Name(), "repo") {
			t.Errorf("file name %q carries path text; it must be a hash", e.Name())
		}
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("ledger dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
}

// TestLedgerBeginRefusesATakenName: a failed launch keeps its row because its
// worktree may hold work, so a new launch under the same name is refused.
func TestLedgerBeginRefusesATakenName(t *testing.T) {
	l, _ := testLedger(t)
	if err := l.Begin(Row{Name: "w1", Branch: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Update("w1", func(r *Row) {
		r.Stage = StageFailed
		r.Worktree = "/repo/one/.claude/worktrees/w1"
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin(Row{Name: "w1", Branch: "b"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
}

func TestLedgerUpdateUnknownName(t *testing.T) {
	l, _ := testLedger(t)
	if err := l.Update("nobody", func(*Row) {}); !errors.Is(err, ErrNoRow) {
		t.Fatalf("err = %v, want ErrNoRow", err)
	}
}

// TestLedgerSeparatesReposAndSessions: the key is the repo top plus the herdr
// session, so the same name in another repo or session is a different worker.
func TestLedgerSeparatesReposAndSessions(t *testing.T) {
	state := t.TempDir()
	for _, k := range [][2]string{{"/repo/one", "default"}, {"/repo/two", "default"}, {"/repo/one", "fleet"}} {
		l, err := openAt(state, k[0], k[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Begin(Row{Name: "w1", Branch: "x"}); err != nil {
			t.Fatalf("Begin in %v: %v", k, err)
		}
	}
}

// TestLedgerRefusesASymlinkedFile: a ledger name swapped for a symlink is
// refused rather than followed to wherever it points.
func TestLedgerRefusesASymlinkedFile(t *testing.T) {
	l, dir := testLedger(t)
	if err := l.Begin(Row{Name: "w1", Branch: "x"}); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, ledgerKey("/repo/one", "default")+".json")
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Rename(data, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, data); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Rows(); !errors.Is(err, ErrLedgerUnreadable) {
		t.Errorf("Rows through a symlink: err = %v, want ErrLedgerUnreadable", err)
	}
	if err := l.Begin(Row{Name: "w2", Branch: "y"}); !errors.Is(err, ErrLedgerUnreadable) {
		t.Errorf("Begin through a symlink: err = %v, want ErrLedgerUnreadable", err)
	}
}

// TestLedgerRefusesAnotherReposFile: a file whose recorded repo differs from
// the key's is refused, so a hash collision or a copied file cannot hand one
// repo another repo's rows.
func TestLedgerRefusesAnotherReposFile(t *testing.T) {
	state := t.TempDir()
	one, _ := openAt(state, "/repo/one", "default")
	if err := one.Begin(Row{Name: "w1", Branch: "x"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "forgectl", "surface")
	src := filepath.Join(dir, ledgerKey("/repo/one", "default")+".json")
	dst := filepath.Join(dir, ledgerKey("/repo/two", "default")+".json")
	//nolint:gosec // G304: reading back a ledger file this test just wrote under its own temp dir
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G703: dst is built from this test's temp dir and a hash, not from input
	if err := os.WriteFile(dst, raw, ledgerFileMode); err != nil {
		t.Fatal(err)
	}

	two, _ := openAt(state, "/repo/two", "default")
	if _, err := two.Rows(); !errors.Is(err, ErrLedgerUnreadable) {
		t.Fatalf("err = %v, want ErrLedgerUnreadable", err)
	}
}

func TestLedgerRowsOnAnAbsentLedgerIsEmpty(t *testing.T) {
	l, dir := testLedger(t)
	rows, err := l.Rows()
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %v, err = %v", rows, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("reading an absent ledger created its directory")
	}
}

// TestLedgerBeginReusesAFailedRowThatCreatedNothing: a launch that failed
// before git created anything leaves a row with nothing to protect, so a
// retry under the same name is allowed rather than stranded.
func TestLedgerBeginReusesAFailedRowThatCreatedNothing(t *testing.T) {
	l, _ := testLedger(t)
	if err := l.Begin(Row{Name: "w1", Branch: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Update("w1", func(r *Row) { r.Stage = StageFailed; r.Failure = "branch busy" }); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin(Row{Name: "w1", Branch: "b"}); err != nil {
		t.Fatalf("retry after a failure that created nothing: %v", err)
	}
	rows, err := l.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Branch != "b" || rows[0].Stage != StagePending {
		t.Errorf("rows = %+v, want the retry's pending row only", rows)
	}
}

// TestLedgerRefusesAHardlinkedFile: a second name for the ledger's inode means
// a write here also writes somewhere forgectl was never pointed at.
func TestLedgerRefusesAHardlinkedFile(t *testing.T) {
	l, dir := testLedger(t)
	if err := l.Begin(Row{Name: "w1", Branch: "x"}); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, ledgerKey("/repo/one", "default")+".json")
	if err := os.Link(data, filepath.Join(t.TempDir(), "other-name")); err != nil {
		t.Skipf("cannot hardlink across these dirs: %v", err)
	}
	if _, err := l.Rows(); !errors.Is(err, ErrLedgerUnreadable) {
		t.Errorf("Rows on a hardlinked ledger: err = %v, want ErrLedgerUnreadable", err)
	}
}

// TestLedgerConditionalChanges pins close's guard: RemoveIf and UpdateIf act
// only on the row that was read, never on a launch that reused the name.
func TestLedgerConditionalChanges(t *testing.T) {
	l, _ := testLedger(t)
	read := Row{Name: "w", Branch: "b", StartedAt: time.Unix(100, 0).UTC()}
	if err := l.Begin(read); err != nil {
		t.Fatal(err)
	}
	read.Stage = StagePending
	reused := read
	reused.StartedAt = time.Unix(200, 0).UTC()

	if err := l.UpdateIf("w", SameRow(reused), func(r *Row) { r.Stage = StageClosed }); !errors.Is(err, ErrRowChanged) {
		t.Fatalf("UpdateIf on a changed row: %v", err)
	}
	if err := l.RemoveIf("w", SameRow(reused)); !errors.Is(err, ErrRowChanged) {
		t.Fatalf("RemoveIf on a changed row: %v", err)
	}
	if err := l.RemoveIf("w", SameRow(read)); err != nil {
		t.Fatalf("RemoveIf: %v", err)
	}
	if rows, err := l.Rows(); err != nil || len(rows) != 0 {
		t.Fatalf("rows %+v, %v after removing the only row", rows, err)
	}
	if err := l.RemoveIf("w", SameRow(read)); !errors.Is(err, ErrNoRow) {
		t.Fatalf("RemoveIf on a missing row: %v", err)
	}
}
