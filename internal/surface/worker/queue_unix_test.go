//go:build unix

package worker

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var queueNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func testQueue(t *testing.T) (*Queue, string) {
	t.Helper()
	state := t.TempDir()
	return openQueueAt(state), filepath.Join(state, "forgectl", "surface")
}

func mustEnqueue(t *testing.T, q *Queue, name, brief string) QueueRow {
	t.Helper()
	row, added, err := q.Enqueue(name, "/repo/one", brief, "", queueNow)
	if err != nil || !added {
		t.Fatalf("Enqueue(%s): added %v, err %v", name, added, err)
	}
	return row
}

func TestQueueEnqueueIdempotency(t *testing.T) {
	cases := map[string]struct {
		repo, brief string
		wantErr     error
		wantIn      []string
	}{
		"same brief is a no-op":    {"/repo/one", "fix the thing", nil, nil},
		"different brief refused":  {"/repo/one", "fix another thing", ErrQueueNameTaken, []string{BriefSHA256("fix the thing"), BriefSHA256("fix another thing")}},
		"same name, other repo":    {"/repo/two", "fix the thing", ErrQueueNameTaken, []string{"/repo/one"}},
		"leading @ refused":        {"/repo/one", "@brief.md", ErrInvalidBrief, []string{"'@'"}},
		"leading @ after newlines": {"/repo/one", "\n @brief.md", ErrInvalidBrief, []string{"'@'"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			q, dir := testQueue(t)
			first := mustEnqueue(t, q, "w1", "fix the thing")
			//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
			before, err := os.ReadFile(filepath.Join(dir, "queue.json"))
			if err != nil {
				t.Fatal(err)
			}
			row, added, err := q.Enqueue("w1", c.repo, c.brief, "", queueNow.Add(time.Hour))
			if added {
				t.Fatal("a second enqueue of one name added a row")
			}
			if c.wantErr == nil {
				if err != nil || row.State != QueueQueued || row.BriefSHA256 != first.BriefSHA256 || !row.EnqueuedAt.Equal(queueNow) {
					t.Fatalf("no-op: row %+v, err %v", row, err)
				}
			} else if !errors.Is(err, c.wantErr) {
				t.Fatalf("err %v, want %v", err, c.wantErr)
			}
			for _, w := range c.wantIn {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
			//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
			after, err := os.ReadFile(filepath.Join(dir, "queue.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the queue file changed")
			}
		})
	}
}

func TestQueueEnqueueRefusesBadInput(t *testing.T) {
	q, _ := testQueue(t)
	cases := map[string]struct{ name, repo, brief, batch string }{
		"bad name":      {"W1", "/r", "x", ""},
		"relative repo": {"w1", "r", "x", ""},
		"unclean repo":  {"w1", "/r/../s", "x", ""},
		"bad batch":     {"w1", "/r", "x", "Batch!"},
		"blank brief":   {"w1", "/r", "  \n", ""},
		"control char":  {"w1", "/r", "x\x1b[2J", ""},
		"brief too big": {"w1", "/r", strings.Repeat("a", MaxLaunchBrief+1), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := q.Enqueue(c.name, c.repo, c.brief, c.batch, queueNow); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if rows, err := q.Rows(); err != nil || len(rows) != 0 {
		t.Fatalf("rows %+v, err %v", rows, err)
	}
}

// TestQueueSizeCapRefusedBeforeWrite: an enqueue that would push the document
// past MaxQueueBytes is refused, names the current size, and writes nothing.
func TestQueueSizeCapRefusedBeforeWrite(t *testing.T) {
	q, dir := testQueue(t)
	brief := strings.Repeat("b", MaxLaunchBrief-100)
	var i int
	for i = 0; ; i++ {
		_, _, err := q.Enqueue("w"+strconv.Itoa(i), "/repo/one", brief, "", queueNow)
		if errors.Is(err, ErrQueueFull) {
			break
		}
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if i > 20 {
			t.Fatal("the cap never applied")
		}
	}
	//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
	before, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = q.Enqueue("wz", "/repo/one", brief, "", queueNow)
	if !errors.Is(err, ErrQueueFull) || !strings.Contains(err.Error(), "is "+strconv.Itoa(len(before))+" bytes") {
		t.Fatalf("err %v, want ErrQueueFull naming the current size %d", err, len(before))
	}
	//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
	after, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(after) > MaxQueueBytes {
		t.Fatalf("the refused enqueue changed the file (%d -> %d bytes)", len(before), len(after))
	}
	rows, err := q.Rows()
	if err != nil || len(rows) != i {
		t.Fatalf("rows %d, err %v; want %d", len(rows), err, i)
	}

	// The drain's writes are not bound by the enqueue cap: on a queue the
	// operator filled to within a few bytes of it, a claim (which grows the
	// row) and a failure with a long error still go through, and the error is
	// cut to maxLastError. Fill the gap: measure one row's overhead with a
	// one-byte brief, remove it, then enqueue a brief that leaves 8 bytes.
	size := func() int {
		//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
		b, err := os.ReadFile(filepath.Join(dir, "queue.json"))
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	base := size()
	mustEnqueue(t, q, "probe", "x")
	overhead := size() - base - 1
	if _, err := q.Dequeue("probe"); err != nil {
		t.Fatal(err)
	}
	mustEnqueue(t, q, "gap", strings.Repeat("g", MaxQueueBytes-size()-overhead-8))
	if room := MaxQueueBytes - size(); room < 0 || room > 64 {
		t.Fatalf("the fill left %d bytes under the cap, want a few", room)
	}
	claimed, err := q.Claim("w0", "launch-0123456789abcdef0123456789abcdef", queueNow)
	if err != nil {
		t.Fatalf("claim on a full queue: %v", err)
	}
	failed, err := q.UpdateIf("w0", SameRead(claimed), queueNow, func(r *QueueRow) {
		r.State = QueueFailed
		r.LastError = strings.Repeat("é", maxLastError)
	})
	if err != nil {
		t.Fatalf("record a failure on a full queue: %v", err)
	}
	if len(failed.LastError) > maxLastError || !utf8.ValidString(failed.LastError) {
		t.Fatalf("last_error is %d bytes (valid UTF-8 %v), want at most %d", len(failed.LastError), utf8.ValidString(failed.LastError), maxLastError)
	}
}

func TestQueueDequeue(t *testing.T) {
	for _, s := range []QueueState{QueueClaimed, QueueLaunched, QueueNeedsYou} {
		t.Run("live "+string(s)+" refused", func(t *testing.T) {
			q, _ := testQueue(t)
			mustEnqueue(t, q, "w1", "brief")
			if _, err := q.UpdateIf("w1", func(QueueRow) bool { return true }, queueNow, func(r *QueueRow) { r.State = s }); err != nil {
				t.Fatal(err)
			}
			_, err := q.Dequeue("w1")
			if !errors.Is(err, ErrQueueRowLive) || !strings.Contains(err.Error(), "run surface close first") {
				t.Fatalf("err %v, want ErrQueueRowLive", err)
			}
			if rows, _ := q.Rows(); len(rows) != 1 {
				t.Fatal("the live row was removed")
			}
		})
	}

	t.Run("a failed row can be dequeued and enqueued again", func(t *testing.T) {
		q, _ := testQueue(t)
		mustEnqueue(t, q, "w1", "first brief")
		if _, err := q.UpdateIf("w1", func(QueueRow) bool { return true }, queueNow, func(r *QueueRow) {
			r.State, r.LastError = QueueFailed, "boom"
		}); err != nil {
			t.Fatal(err)
		}
		// A different brief is still refused while the failed row is there.
		if _, _, err := q.Enqueue("w1", "/repo/one", "second brief", "", queueNow); !errors.Is(err, ErrQueueNameTaken) {
			t.Fatalf("err %v, want ErrQueueNameTaken", err)
		}
		gone, err := q.Dequeue("w1")
		if err != nil || gone.State != QueueFailed {
			t.Fatalf("Dequeue: %+v, %v", gone, err)
		}
		row, added, err := q.Enqueue("w1", "/repo/one", "second brief", "", queueNow)
		if err != nil || !added || row.State != QueueQueued || row.Attempts != 0 || row.LastError != "" {
			t.Fatalf("re-enqueue: %+v, added %v, err %v", row, added, err)
		}
	})

	t.Run("no such row", func(t *testing.T) {
		q, _ := testQueue(t)
		if _, err := q.Dequeue("nope"); !errors.Is(err, ErrQueueNoRow) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestQueueTerminalRowsDropBriefText(t *testing.T) {
	q, _ := testQueue(t)
	first := mustEnqueue(t, q, "w1", "the brief")
	row, err := q.UpdateIf("w1", func(QueueRow) bool { return true }, queueNow.Add(time.Minute), func(r *QueueRow) { r.State = QueueReported })
	if err != nil {
		t.Fatal(err)
	}
	if row.Brief != "" || row.BriefSHA256 != first.BriefSHA256 || !row.StateAt.Equal(queueNow.Add(time.Minute)) {
		t.Fatalf("row %+v", row)
	}
	rows, err := q.Rows()
	if err != nil || rows[0].Brief != "" || rows[0].BriefSHA256 == "" {
		t.Fatalf("stored %+v, err %v", rows, err)
	}
	// The same brief again is still a no-op: the hash is kept.
	if _, added, err := q.Enqueue("w1", "/repo/one", "the brief", "", queueNow); err != nil || added {
		t.Fatalf("added %v, err %v", added, err)
	}
}

// TestQueueClaimOneWinner: of two claimers of one row, exactly one wins.
func TestQueueClaimOneWinner(t *testing.T) {
	state := t.TempDir()
	mustEnqueue(t, openQueueAt(state), "w1", "brief")

	const claimers = 2
	var wg sync.WaitGroup
	errs := make([]error, claimers)
	for i := range claimers {
		wg.Go(func() {
			// Each claimer opens its own Queue, as two processes would.
			_, errs[i] = openQueueAt(state).Claim("w1", "launch-"+strconv.Itoa(i), queueNow)
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrQueueNotQueued):
		default:
			t.Fatalf("claim: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d claimers won, want exactly 1 (%v)", won, errs)
	}
	rows, err := openQueueAt(state).Rows()
	if err != nil || rows[0].State != QueueClaimed || rows[0].LaunchID == "" {
		t.Fatalf("rows %+v, err %v", rows, err)
	}
	// SameRead guards a later update to the winner's claim only.
	other := rows[0]
	other.LaunchID = "someone-else"
	if _, err := openQueueAt(state).UpdateIf("w1", SameRead(other), queueNow, func(r *QueueRow) { r.State = QueueLaunched }); !errors.Is(err, ErrQueueRowChanged) {
		t.Fatalf("UpdateIf with another launch id: %v", err)
	}
	if _, err := openQueueAt(state).UpdateIf("w1", SameRead(rows[0]), queueNow, func(r *QueueRow) { r.State = QueueLaunched }); err != nil {
		t.Fatalf("UpdateIf with the claim's launch id: %v", err)
	}
}

func TestQueueClaimRefusesAnUnqueuedRow(t *testing.T) {
	q, _ := testQueue(t)
	mustEnqueue(t, q, "w1", "brief")
	if _, err := q.Claim("w1", "", queueNow); err == nil {
		t.Fatal("a claim with no launch id was accepted")
	}
	if _, err := q.Claim("w1", "l1", queueNow); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim("w1", "l2", queueNow); !errors.Is(err, ErrQueueNotQueued) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := q.Claim("nope", "l3", queueNow); !errors.Is(err, ErrQueueNoRow) {
		t.Fatalf("claim of a missing row: %v", err)
	}
}

func TestQueueRefusesAnUnreadableFile(t *testing.T) {
	cases := map[string]string{
		"unknown version": `{"version": 2, "rows": []}`,
		"unknown field":   `{"version": 1, "rows": [], "extra": true}`,
		"unknown state":   `{"version": 1, "rows": [{"name": "w1", "repo": "/r", "brief_sha256": "x", "state": "running", "attempts": 0, "enqueued_at": "2026-10-07T00:00:00Z", "state_at": "2026-10-07T00:00:00Z"}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			q, dir := testQueue(t)
			mustEnqueue(t, q, "w0", "brief") // creates the pinned directory
			path := filepath.Join(dir, "queue.json")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := q.Rows(); !errors.Is(err, ErrQueueUnreadable) {
				t.Fatalf("Rows: %v", err)
			}
			if _, _, err := q.Enqueue("w1", "/r", "brief", "", queueNow); !errors.Is(err, ErrQueueUnreadable) {
				t.Fatalf("Enqueue: %v", err)
			}
			//nolint:gosec // G304: reading back a queue file this test wrote under its own temp dir
			got, err := os.ReadFile(path)
			if err != nil || string(got) != doc {
				t.Fatal("a refused file was rewritten")
			}
		})
	}
}

// TestQueueFileChecks: the queue reuses the ledger's pinned directory and
// verified open, so it gets a 0600 file in a 0700 directory and refuses a
// symlink in place of queue.json.
func TestQueueFileChecks(t *testing.T) {
	q, dir := testQueue(t)
	mustEnqueue(t, q, "w1", "brief")
	for _, name := range []string{"queue.json", "queue.lock"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != ledgerFileMode {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}

	elsewhere := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(elsewhere, []byte(`{"version":1,"rows":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "queue.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Rows(); !errors.Is(err, ErrLedgerUnreadable) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Rows through a symlink: %v", err)
	}
}
