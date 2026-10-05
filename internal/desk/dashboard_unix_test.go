// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package desk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What the dashboard reads from the desk, and the approval-control hardening
// it depends on: accessors that never block or follow, claims that cannot be
// orphaned, and a supervisor bound to the approved bytes.

// within fails the test when f has not returned after limit: a read that
// blocks on a FIFO must fail a test, not hang the suite.
func within(t *testing.T, limit time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("still blocked after %v", limit)
	}
}

// plants are the files an accessor must refuse in place of a regular one.
var plants = []struct {
	name  string
	plant func(t *testing.T, p string)
}{
	{"symlink", func(t *testing.T, p string) {
		target := filepath.Join(t.TempDir(), "elsewhere")
		writeFile(t, target, "a -- true\n", 0o600)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}},
	{"fifo", func(t *testing.T, p string) {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}},
	{"directory", func(t *testing.T, p string) {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}},
}

func TestManifestAndBatchStatusRefuseAnythingButARegularFile(t *testing.T) {
	for _, pl := range plants {
		t.Run(pl.name, func(t *testing.T) {
			d := openDesk(t)
			pl.plant(t, filepath.Join(d.Path(), DirDone, "05-x.manifest"))
			if err := os.Mkdir(filepath.Join(d.Path(), DirDone, "05-x.d"), 0o700); err != nil {
				t.Fatal(err)
			}
			pl.plant(t, filepath.Join(d.Path(), DirDone, "05-x.d", "status.tsv"))
			within(t, 2*time.Second, func() {
				if m, err := d.Manifest("05-x"); !errors.Is(err, ErrRefused) {
					t.Errorf("Manifest = %v, %v; want ErrRefused", m, err)
				}
				if rows, err := d.BatchStatus("05-x"); !errors.Is(err, ErrRefused) {
					t.Errorf("BatchStatus = %v, %v; want ErrRefused", rows, err)
				}
			})
		})
	}
}

func TestManifestAndBatchStatusRefuseAnOversizeFile(t *testing.T) {
	d := openDesk(t)
	big := strings.Repeat("a -- true\n", maxItemBytes/10+1)
	writeFile(t, filepath.Join(d.Path(), DirDone, "05-x.manifest"), big, 0o600)
	if _, err := d.Manifest("05-x"); !errors.Is(err, ErrRefused) {
		t.Errorf("Manifest = %v; want ErrRefused", err)
	}
	row := "a\tok\t0\t-\t-\t-\n"
	writeFile(t, filepath.Join(d.Path(), DirDone, "05-x.d", "status.tsv"), statusHeader+"\n"+strings.Repeat(row, maxStatusBytes/len(row)+1), 0o600)
	if _, err := d.BatchStatus("05-x"); !errors.Is(err, ErrRefused) {
		t.Errorf("BatchStatus = %v; want ErrRefused", err)
	}
	if _, err := d.Manifest("06-none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Manifest of a missing item = %v; want ErrNotFound", err)
	}
	if _, err := d.BatchStatus("06-none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BatchStatus of a missing item = %v; want ErrNotFound", err)
	}
}

// The accessors read what the runner really writes: a batch run through the
// runner into the desk's own done/<name>.d.
func TestManifestAndBatchStatusReadARealRun(t *testing.T) {
	d := openDesk(t)
	text := "alpha -- true\nbeta after=alpha -- exit 3\ngamma after=beta -- true\n"
	writeFile(t, filepath.Join(d.Path(), DirDone, "05-x.manifest"), text, 0o600)
	m, err := d.Manifest("05-x")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(m.Steps) != 3 || m.Steps[1].ID != "beta" || m.SHA256 != SHA256Hex([]byte(text)) {
		t.Fatalf("manifest = %+v", m)
	}
	dir := filepath.Join(d.Path(), DirDone, "05-x.d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	if res := NewBatch(m, BatchOptions{ID: "05-x", Dir: dir, WorkDir: t.TempDir()}).Run(); res.RC != 1 {
		t.Fatalf("batch = %+v", res)
	}
	rows, err := d.BatchStatus("05-x")
	if err != nil {
		t.Fatalf("BatchStatus: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	a, b, c := rows[0], rows[1], rows[2]
	if a.Step != "alpha" || a.State != stepOK || a.RC == nil || *a.RC != 0 || a.Start.Before(before) || a.End.Before(a.Start) || a.Deps != nil {
		t.Errorf("alpha = %+v", a)
	}
	if b.State != stepFailed || b.RC == nil || *b.RC != 3 || !slices.Equal(b.Deps, []string{"alpha"}) {
		t.Errorf("beta = %+v", b)
	}
	if c.State != stepSkipped || c.RC != nil || !c.Start.IsZero() || !c.End.IsZero() {
		t.Errorf("gamma = %+v", c)
	}
}

func TestParseStatusRejectsAMalformedFile(t *testing.T) {
	for _, text := range []string{
		"not a header\n",
		statusHeader + "\na\tok\t0\t-\t-\n",
		statusHeader + "\na\tok\tzero\t-\t-\t-\n",
		statusHeader + "\na\tok\t0\tNaN\t-\t-\n",
	} {
		if rows, err := parseStatus(text); err == nil {
			t.Errorf("parseStatus(%q) = %+v; want an error", text, rows)
		}
	}
}

// A FIFO planted at done/<name>.events must not freeze a dashboard: neither
// a watcher's Poll nor the Scan that judges a running item lost may block.
func TestAFIFOAtTheEventsFileBlocksNeitherPollNorScan(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	pid, start := deadProcess(t)
	m, _, err := d.readMeta(DirRunning, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	m.PID, m.PIDStart = pid, start
	if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(d.EventsPath(c.Name), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := d.NewWatcher(c.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	within(t, 2*time.Second, func() {
		if _, _, err := w.Poll(); !errors.Is(err, ErrRefused) {
			t.Errorf("Poll = %v; want ErrRefused", err)
		}
		if s, err := d.Scan(); err != nil || len(s.Running) != 1 {
			t.Errorf("Scan = %+v, %v", s, err)
		}
	})
}

// appendEvents writes lines to done/<name>.events as the desk would.
func appendEvents(t *testing.T, d *Desk, name, text string) {
	t.Helper()
	f, err := os.OpenFile(d.EventsPath(name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func pollLines(t *testing.T, w *Watcher) []string {
	t.Helper()
	lines, _, err := w.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return lines
}

// A replaced or truncated events file starts the watcher over on the new
// file; Seen then counts the new file's lines only.
func TestWatcherStartsOverWhenTheEventsFileIsReplacedOrShrinks(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n") // running, claimer alive: the watcher keeps waiting
	appendEvents(t, d, c.Name, "STEP-START id=a deps=\nSTEP-START id=b deps=\nSTEP-START id=c deps=\n")
	w, err := d.NewWatcher(c.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := pollLines(t, w); len(got) != 2 || w.Seen() != 3 {
		t.Fatalf("first poll = %q, seen %d", got, w.Seen())
	}

	tmp := filepath.Join(d.Path(), DirDone, ".swap")
	writeFile(t, tmp, "STEP-START id=new deps=\n", 0o600)
	if err := os.Rename(tmp, d.EventsPath(c.Name)); err != nil {
		t.Fatal(err)
	}
	if got := pollLines(t, w); !slices.Equal(got, []string{"STEP-START id=new deps="}) || w.Seen() != 1 {
		t.Errorf("after replace = %q, seen %d; want the new file's line, seen 1", got, w.Seen())
	}

	appendEvents(t, d, c.Name, "STEP-START id=more deps=\n")
	pollLines(t, w)
	if err := os.Truncate(d.EventsPath(c.Name), 0); err != nil {
		t.Fatal(err)
	}
	appendEvents(t, d, c.Name, "RUN-END rc=0 reason=ok\n")
	lines, state, err := w.Poll()
	if err != nil || state != WatchEnded || !slices.Equal(lines, []string{"RUN-END rc=0 reason=ok"}) || w.Seen() != 1 {
		t.Errorf("after shrink = %q, %s, %v, seen %d", lines, state, err, w.Seen())
	}
}

// A line longer than 64 KiB is dropped and counted, terminated or not when
// it passes the limit; the lines around it still arrive.
func TestWatcherDropsAndCountsAnOverlongLine(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	long := "STEP-WARN id=a msg=" + strings.Repeat("x", maxEventLine)
	appendEvents(t, d, c.Name, "STEP-START id=a deps=\n"+long[:maxEventLine/2])
	w, err := d.NewWatcher(c.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := pollLines(t, w); !slices.Equal(got, []string{"STEP-START id=a deps="}) {
		t.Fatalf("first poll = %q", got)
	}
	appendEvents(t, d, c.Name, long[maxEventLine/2:]+"\nSTEP-START id=b deps=\n"+long+"\nRUN-END rc=0 reason=ok\n")
	lines, state, err := w.Poll()
	if err != nil || state != WatchEnded || !slices.Equal(lines, []string{"STEP-START id=b deps=", "RUN-END rc=0 reason=ok"}) {
		t.Errorf("Poll = %q, %s, %v", lines, state, err)
	}
	if w.Dropped() != 2 || w.Seen() != 5 {
		t.Errorf("dropped %d seen %d; want 2 dropped, 5 seen", w.Dropped(), w.Seen())
	}
}

// One poll reads at most 32 MiB; the rest arrives on the next.
func TestWatcherReadsAtMost32MiBPerPoll(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	line := "STEP-WARN id=a msg=" + strings.Repeat("y", 1000) + "\n"
	n := maxPollBytes/len(line) + 2000
	appendEvents(t, d, c.Name, strings.Repeat(line, n))
	w, err := d.NewWatcher(c.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := len(pollLines(t, w))
	if first >= n || first < maxPollBytes/len(line)-1 {
		t.Fatalf("first poll returned %d of %d lines; want about 32 MiB worth", first, n)
	}
	if second := len(pollLines(t, w)); first+second != n {
		t.Errorf("polls returned %d + %d lines, want %d", first, second, n)
	}
}

// deadProcess starts a process, records its start time while it lives, and
// reaps it: the pid and start time of a process that is gone.
func deadProcess(t *testing.T) (int, int64) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start := ownStart(pid)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return pid, start
}

func TestClaimStampsTheClaimerAndTheApprovedHash(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	raw := readFile(t, filepath.Join(d.Path(), DirRunning, c.Name+".meta.json"))
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m["claim_pid"] != float64(os.Getpid()) || m["approved_sha256"] != c.SHA256 || m["claimed_at"] == nil {
		t.Errorf("running meta = %s", raw)
	}
	if (isDarwin() || isLinux()) && m["claim_start"] != float64(ownStart(os.Getpid())) {
		t.Errorf("claim_start = %v", m["claim_start"])
	}
	if _, found := m["pid"]; found {
		t.Errorf("a claim with no run recorded an owner: %s", raw)
	}
}

// A claim whose claimer died before a run began reads as lost, and the
// operator can skip it. With the claimer alive it is still running.
func TestAClaimWhoseClaimerDiedIsLostAndSkippable(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	if s := scan(t, d); len(s.Running) != 1 || s.Running[0].State != StateRunning {
		t.Fatalf("with this process as claimer: %+v", s.Running)
	}
	if err := d.Skip(c.Name, SkipLost); err == nil {
		t.Fatal("Skip took a live claim")
	}

	pid, start := deadProcess(t)
	m, _, err := d.readMeta(DirRunning, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	m.ClaimPID, m.ClaimStart = pid, start
	if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
		t.Fatal(err)
	}
	if s := scan(t, d); len(s.Running) != 1 || s.Running[0].State != StateLost {
		t.Fatalf("with a dead claimer: %+v", s.Running)
	}
	w, err := d.NewWatcher(c.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines, state, err := w.Poll()
	if err != nil || state != WatchLost || !slices.Equal(lines, []string{fmt.Sprintf("RUN-LOST id=%s pid=%d", c.Name, pid)}) {
		t.Errorf("Poll = %q, %s, %v", lines, state, err)
	}
	if err := d.Skip(c.Name, SkipLost); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	if s := scan(t, d); s.Running != nil || len(s.Skipped) != 1 {
		t.Errorf("after skip: running %v skipped %v", s.Running, s.Skipped)
	}
}

// With no claim stamp (a claim that failed between its renames), the item
// file's age decides: lost only after 30 s.
func TestAnUnstampedClaimIsLostOnlyAfterTheGrace(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	m, _, err := d.readMeta(DirRunning, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	m.ClaimPID, m.ClaimStart, m.ClaimedAt = 0, 0, nil
	if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
		t.Fatal(err)
	}
	if s := scan(t, d); s.Running[0].State != StateRunning {
		t.Fatalf("a fresh unstamped claim = %s, want running", s.Running[0].State)
	}
	d.now = func() time.Time { return time.Now().Add(claimGrace + time.Second) }
	if s := scan(t, d); s.Running[0].State != StateLost {
		t.Fatalf("an unstamped claim past the grace = %s, want lost", s.Running[0].State)
	}
}

func TestAbandonGivesBackAClaimThisProcessMade(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	if err := d.Abandon(c.Name, "launch-failed"); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	s := scan(t, d)
	if s.Running != nil || len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != "launch-failed" || s.Skipped[0].Meta.Kind != KindScript {
		t.Errorf("after Abandon: running %v skipped %+v", s.Running, s.Skipped)
	}
}

func TestAbandonRefusesARunWithAnOwnerOrAnotherClaimer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, d *Desk, c *Claimed)
	}{
		{"live owner", func(t *testing.T, d *Desk, c *Claimed) {
			if _, err := d.BeginRun(c.Name, os.Getpid()); err != nil {
				t.Fatal(err)
			}
		}},
		{"another claimer", func(t *testing.T, d *Desk, c *Claimed) {
			m, _, err := d.readMeta(DirRunning, c.Name)
			if err != nil {
				t.Fatal(err)
			}
			m.ClaimPID = os.Getppid()
			if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDesk(t)
			c := queue(t, d, "x.sh", "true\n")
			tc.setup(t, d, c)
			before := readFile(t, filepath.Join(d.Path(), DirRunning, c.Name+".meta.json"))
			if err := d.Abandon(c.Name, "launch-failed"); !errors.Is(err, ErrClaimed) {
				t.Fatalf("Abandon = %v, want ErrClaimed", err)
			}
			if s := scan(t, d); len(s.Running) != 1 || s.Skipped != nil {
				t.Errorf("running %v skipped %v; want the item back in running/", s.Running, s.Skipped)
			}
			if after := readFile(t, filepath.Join(d.Path(), DirRunning, c.Name+".meta.json")); after != before {
				t.Errorf("meta changed: %s -> %s", before, after)
			}
		})
	}
}

// Claim takes only a full hash: an empty one, a 63-digit prefix, or one in
// the wrong case is refused before anything moves.
func TestClaimRefusesAnythingButAFullSHA(t *testing.T) {
	d := openDesk(t)
	added := addScript(t, d, "x.sh", "true\n")
	for _, sha := range []string{"", added.SHA256[:63], strings.ToUpper(added.SHA256), added.SHA256 + "0"} {
		if _, err := d.Claim(added.Name, sha); !errors.Is(err, ErrRefused) {
			t.Errorf("Claim(%q) = %v, want ErrRefused", sha, err)
		}
	}
	if s := scan(t, d); len(s.Pending) != 1 || s.Running != nil {
		t.Errorf("pending %v running %v; a refused claim moved the item", s.Pending, s.Running)
	}
}

// A reviewed manifest and a script with its exact bytes under the same name:
// Claim finds the script first and must not run the manifest text as bash.
func TestClaimRefusesAKindOtherThanTheOneQueued(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "b.manifest")
	writeFile(t, src, "a -- true\n", 0o600)
	a, err := d.Add(src, "a batch", "a test", false)
	if err != nil {
		t.Fatal(err)
	}
	body := readFile(t, a.Path)
	dropPending(t, d, a.Name+".sh", body)
	if _, err := d.Claim(a.Name, a.SHA256); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim = %v, want ErrRefused", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(d.Path(), DirRunning)); len(entries) != 0 {
		t.Errorf("running/ = %v, want empty", entries)
	}
}

// After a claim, rewriting both the record and its meta to a consistent new
// item runs nothing: the supervisor is bound to the hash that was approved.
func TestARewriteOfRecordAndMetaAfterClaimRunsNothing(t *testing.T) {
	for _, path := range []string{"launch", "supervise"} {
		t.Run(path, func(t *testing.T) {
			d := openDesk(t)
			marker := filepath.Join(t.TempDir(), "ran")
			c := queue(t, d, "x.sh", "echo approved\n")
			evil := "touch " + marker + "\n"
			writeFile(t, c.RecordPath, evil, 0o600)
			m, _, err := d.readMeta(DirRunning, c.Name)
			if err != nil {
				t.Fatal(err)
			}
			m.SHA256 = SHA256Hex([]byte(evil))
			if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
				t.Fatal(err)
			}
			if path == "launch" {
				if _, err := d.Launch(c); err != nil {
					t.Fatal(err)
				}
				if _, state := watchUntil(t, d, c.Name, 20*time.Second); state != WatchSkipped {
					t.Fatalf("state = %s, want skipped", state)
				}
			} else if rc, err := d.supervise(c.Name, c.SHA256, c.Kind); rc != 2 || !errors.Is(err, ErrChanged) {
				t.Fatalf("supervise = %d, %v; want ErrChanged", rc, err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the rewritten bytes ran")
			}
			if s := scan(t, d); len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != SkipChanged {
				t.Errorf("skipped = %+v", s.Skipped)
			}
		})
	}
}

// The hash binds the bytes and the kind binds how they run. A claimed
// manifest with an identical-bytes .sh dropped beside it in running/ runs
// nothing: the bytes are written so that run as bash they would create a
// marker, and run as a batch they would create another.
func TestAClaimedManifestNeverRunsAsAScript(t *testing.T) {
	for _, path := range []string{"launch", "supervise"} {
		t.Run(path, func(t *testing.T) {
			d := openDesk(t)
			dir := t.TempDir()
			asBash, asBatch := filepath.Join(dir, "ran-as-bash"), filepath.Join(dir, "ran-as-batch")
			// As a manifest: step "touch" runs a missing command, step "a"
			// touches asBatch. As bash: `touch -- asBash`, then `a`, not found.
			src := filepath.Join(t.TempDir(), "two.manifest")
			writeFile(t, src, "touch -- "+asBash+"\na -- touch "+asBatch+"\n", 0o600)
			a, err := d.Add(src, "a batch", "kind binding", false)
			if err != nil {
				t.Fatal(err)
			}
			scan(t, d)
			c, err := d.Claim(a.Name, a.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(d.Path(), DirRunning, c.Name+".sh"), string(c.Content), 0o600)
			if path == "launch" {
				if _, err := d.Launch(c); err == nil {
					t.Fatal("Launch started a supervisor for a claim whose kind running/ no longer holds alone")
				}
			} else if rc, err := d.supervise(c.Name, c.SHA256, c.Kind); rc != 2 || !errors.Is(err, ErrRefused) {
				t.Fatalf("supervise = %d, %v; want ErrRefused", rc, err)
			}
			time.Sleep(200 * time.Millisecond) // a step or script, had one started, has touched its marker by now
			for _, m := range []string{asBash, asBatch} {
				if _, err := os.Stat(m); err == nil {
					t.Errorf("%s exists: the item ran", filepath.Base(m))
				}
			}
			if _, err := os.Stat(d.EventsPath(c.Name)); err == nil {
				t.Error("a run began")
			}
		})
	}
}

// The argv kind alone is enough to refuse: a script approved as a script
// does not run when the supervisor is told it is a batch, nor the reverse.
func TestSuperviseRefusesAKindOtherThanTheApprovedOne(t *testing.T) {
	d := openDesk(t)
	marker := filepath.Join(t.TempDir(), "ran")
	c := queue(t, d, "x.sh", "touch "+marker+"\n")
	m, _, err := d.readMeta(DirRunning, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	m.Kind = "" // so only the argv kind can catch it
	if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
		t.Fatal(err)
	}
	if rc, err := d.supervise(c.Name, c.SHA256, KindBatch); rc != 2 || !errors.Is(err, ErrRefused) {
		t.Fatalf("supervise = %d, %v; want ErrRefused", rc, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the item ran")
	}
	if s := scan(t, d); len(s.Skipped) != 1 || !strings.HasPrefix(s.Skipped[0].Meta.SkipReason, "supervisor: ") {
		t.Errorf("skipped = %+v, want a supervisor: skip reason", s.Skipped)
	}
}

// bashEnvMarker sets BASH_ENV to a script that creates the returned marker,
// and proves bash on this machine honors it (the positive control): without
// it, "the marker is absent" would prove nothing.
func bashEnvMarker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker, hook := filepath.Join(dir, "bash-env-ran"), filepath.Join(dir, "hook.sh")
	writeFile(t, hook, "touch "+marker+"\n", 0o600)
	t.Setenv("BASH_ENV", hook)
	probe := exec.CommandContext(t.Context(), "/bin/bash", "-c", "true")
	probe.Env = append(os.Environ(), "BASH_ENV="+hook)
	if err := probe.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("positive control: bash did not run BASH_ENV (%v)", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	return marker
}

// An approved item runs its own bytes only: BASH_ENV never runs first, and
// it starts in $HOME, not wherever the desk was started.
func TestAnItemStartsInHomeWithoutBashStartupHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := bashEnvMarker(t)
	out := filepath.Join(t.TempDir(), "cwd")
	d := openDesk(t)
	c := queue(t, d, "x.sh", "pwd -P > "+out+"\n")
	if _, err := d.Launch(c); err != nil {
		t.Fatal(err)
	}
	if lines, state := watchUntil(t, d, c.Name, 20*time.Second); state != WatchEnded || lines[len(lines)-1] != "RUN-END rc=0 reason=ok" {
		t.Fatalf("state %s, events %q", state, lines)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("BASH_ENV ran before the item")
	}
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, out)); got != want {
		t.Errorf("cwd = %q, want $HOME %q", got, want)
	}
}

func TestABatchStepStartsInHomeWithoutBashStartupHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := bashEnvMarker(t)
	m, err := ParseManifest("a -- pwd -P > \"$RUN_BATCH_DIR/cwd\"\n", "t.manifest")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "t.d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if res := NewBatch(m, BatchOptions{ID: "t", Dir: dir}).Run(); res.RC != 0 {
		t.Fatalf("batch = %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("BASH_ENV ran before the step")
	}
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "cwd"))); got != want {
		t.Errorf("cwd = %q, want $HOME %q", got, want)
	}
}

// The supervisor survives SIGHUP without passing an ignored SIGHUP on: the
// item starts with the default disposition, so HUP ends it.
func TestAnItemStartsWithTheDefaultSIGHUP(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "kill -HUP $$\necho survived\n")
	if _, err := d.Launch(c); err != nil {
		t.Fatal(err)
	}
	lines, state := watchUntil(t, d, c.Name, 20*time.Second)
	if state != WatchEnded || lines[len(lines)-1] != "RUN-END rc=129 reason=failed" {
		t.Fatalf("state %s, events %q", state, lines)
	}
	if log := readFile(t, d.LogPath(c.Name)); strings.Contains(log, "survived") {
		t.Errorf("the item ignored SIGHUP: %q", log)
	}
}

// Every line a supervised batch writes parses, and the fields that could
// hold a space or a path stay whole: msg= runs to the end of the line, and
// log= is relative to the desk dir.
func TestEveryEventASupervisedBatchWritesParses(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "demo.manifest")
	writeFile(t, src, `alpha -- echo "greeting=hello" >> "$STEP_OUT"; echo novalue >> "$STEP_OUT"
beta after=alpha -- exit 3
gamma after=beta -- true
`, 0o600)
	a, err := d.Add(src, "a demo batch", "event shapes", false)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, d)
	c, err := d.Claim(a.Name, a.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := d.Launch(c)
	if err != nil {
		t.Fatal(err)
	}
	lines, state := watchUntil(t, d, c.Name, 20*time.Second)
	if state != WatchEnded {
		t.Fatalf("state %s, events %q", state, lines)
	}
	lines = append(lines, fmt.Sprintf("%s id=%s pid=%d", EventRunLost, c.Name, pid)) // as Poll writes it
	keys := map[string]bool{}
	for _, line := range lines {
		ev, ok := ParseEvent(line)
		if !ok {
			t.Errorf("did not parse: %q", line)
			continue
		}
		keys[ev.Key] = true
		switch {
		case ev.Key == EventRunStart && (ev.Fields["id"] != c.Name || ev.Fields["pid"] != strconv.Itoa(pid) || ev.Fields["steps"] != "3"):
			t.Errorf("RUN-START fields = %v", ev.Fields)
		case ev.Key == EventStepWarn && ev.Fields["msg"] != "STEP_OUT line 2 is not key=value; dropped":
			t.Errorf("STEP-WARN msg = %q", ev.Fields["msg"])
		case ev.Key == EventStepEnd && ev.Fields["log"] != "done/"+c.Name+".d/steps/"+ev.Fields["id"]+".log":
			t.Errorf("STEP-END log = %q, want it relative to the desk dir", ev.Fields["log"])
		case ev.Key == EventRunEnd && ev.Fields["skipped"] != "1":
			t.Errorf("RUN-END fields = %v", ev.Fields)
		}
	}
	for _, k := range []string{EventRunStart, EventStepStart, EventStepWarn, EventStepEnd, EventStepSkip, EventRunEnd, EventRunLost} {
		if !keys[k] {
			t.Errorf("no %s line in %q", k, lines)
		}
	}
}

// The events file is created exclusively: one planted ahead of a run, which
// might already hold RUN-END, is never appended to.
func TestOpenEventsRefusesAnExistingFile(t *testing.T) {
	d := openDesk(t)
	writeFile(t, d.EventsPath("05-x"), "RUN-END rc=0 reason=ok\n", 0o600)
	if ev, err := d.openEvents("05-x"); err == nil {
		_ = ev.Close()
		t.Fatal("openEvents appended to an existing events file")
	}
}
