//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pendingItem returns the pending item called name from a scan.
func pendingItem(t *testing.T, s *Snapshot, name string) Item {
	t.Helper()
	for _, it := range s.Pending {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("no pending item %s in %+v", name, s.Pending)
	return Item{}
}

// A pending item queued under the name of an item still in running/ is
// shown refused, and claiming it is refused before anything moves: the
// claim's rename would replace the running item's record (forgectl#1157).
func TestClaimRefusesANameARunHolds(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "a.sh", "echo first\n")
	record := readFile(t, c.RecordPath)

	dropPending(t, d, c.Name+".sh", "# WHAT: again\n# WHY: a test\necho second\n")
	it := pendingItem(t, scan(t, d), c.Name)
	if it.State != StateRefused || it.Refusal != refusedNameTaken {
		t.Fatalf("pending %s: state %s refusal %q, want refused because a run holds the name", c.Name, it.State, it.Refusal)
	}
	if _, err := d.Claim(c.Name, queuedSHA(t, d, c.Name)); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim = %v, want ErrRefused", err)
	}
	if got := readFile(t, c.RecordPath); got != record {
		t.Errorf("the running record changed: %q, want %q", got, record)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), DirPending, c.Name+".sh")); err != nil {
		t.Errorf("the refused item left pending/: %v", err)
	}
	if m, ok, err := d.readMeta(DirPending, c.Name); err != nil || !ok || m.ClaimedAt != nil {
		t.Errorf("pending meta after the refusal: %+v ok=%v err=%v; want it untouched", m, ok, err)
	}

	// Once the run ends its name is in done/: the item stays refused, never
	// waiting, since an approval would only be skipped as name-reused.
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(0, "exit"); err != nil {
		t.Fatal(err)
	}
	if it := pendingItem(t, scan(t, d), c.Name); it.State != StateRefused || it.Refusal != refusedNameTaken {
		t.Fatalf("after the run ended: state %s refusal %q, want refused", it.State, it.Refusal)
	}
	if _, err := d.Claim(c.Name, queuedSHA(t, d, c.Name)); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim after the run ended = %v, want ErrRefused", err)
	}
}

// addAfter queues body as file with an AFTER line naming after.
func addAfter(t *testing.T, d *Desk, file, body string, after ...string) Added {
	t.Helper()
	src := filepath.Join(t.TempDir(), file)
	writeFile(t, src, body, 0o600)
	a, _, err := d.AddWith(src, Headers{What: "verify", Why: "a test", After: after}, false)
	if err != nil {
		t.Fatalf("AddWith: %v", err)
	}
	return a
}

// An item with an AFTER line waits until the item it names has finished
// with exit 0: held while that one waits, runs, or failed, and claimable
// once it succeeded (forgectl#1202).
func TestAfterHoldsAnItemUntilItsPredecessorSucceeds(t *testing.T) {
	for _, tc := range []struct {
		rc        int
		claimable bool
		reason    string
	}{{0, true, ""}, {1, false, "exit 1"}} {
		d := openDesk(t)
		enable := addScript(t, d, "enable.sh", "echo enable\n")
		verify := addAfter(t, d, "verify.sh", "echo verify\n", enable.Name)
		if body := readFile(t, verify.Path); !strings.Contains(body, "# AFTER: "+enable.Name+"\n") {
			t.Fatalf("queued body lacks the AFTER line:\n%s", body)
		}

		it := pendingItem(t, scan(t, d), verify.Name)
		if it.State != StateHeld || !strings.Contains(it.Hold, "has not finished") || !slices.Equal(it.After, []string{enable.Name}) {
			t.Fatalf("before %s ran: state %s hold %q after %q", enable.Name, it.State, it.Hold, it.After)
		}
		if _, err := d.Claim(verify.Name, verify.SHA256); !errors.Is(err, ErrRefused) {
			t.Fatalf("Claim before %s ran = %v, want ErrRefused", enable.Name, err)
		}

		if _, err := d.Claim(enable.Name, enable.SHA256); err != nil {
			t.Fatal(err)
		}
		run, err := d.BeginRun(enable.Name, os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if it := pendingItem(t, scan(t, d), verify.Name); it.State != StateHeld || !strings.Contains(it.Hold, "still running") {
			t.Fatalf("while %s runs: state %s hold %q", enable.Name, it.State, it.Hold)
		}
		if err := run.Finish(tc.rc, "exit"); err != nil {
			t.Fatal(err)
		}

		it = pendingItem(t, scan(t, d), verify.Name)
		_, err = d.Claim(verify.Name, verify.SHA256)
		if tc.claimable {
			if it.State != StateWaiting || err != nil {
				t.Fatalf("after rc 0: state %s hold %q, Claim = %v; want it waiting and claimable", it.State, it.Hold, err)
			}
			continue
		}
		if it.State != StateHeld || !strings.Contains(it.Hold, tc.reason) || !errors.Is(err, ErrRefused) {
			t.Fatalf("after rc %d: state %s hold %q, Claim = %v; want held (%s)", tc.rc, it.State, it.Hold, err, tc.reason)
		}
	}
}

// A hand-dropped AFTER line that can never be met (a bad name, the item
// itself, too many names) makes the item refused, not held.
func TestAfterLineThatCannotBeMetIsRefused(t *testing.T) {
	d := openDesk(t)
	dropPending(t, d, "01-x.sh", "# WHAT: x\n# WHY: y\n# AFTER: ../etc\necho x\n")
	dropPending(t, d, "02-y.sh", "# WHAT: x\n# WHY: y\n# AFTER: 02-y\necho y\n")
	var many []string
	for i := range MaxAfter + 1 {
		many = append(many, fmt.Sprintf("%02d-n", 50+i))
	}
	dropPending(t, d, "03-z.sh", "# WHAT: x\n# WHY: y\n# AFTER: "+strings.Join(many, ",")+"\necho z\n")
	s := scan(t, d)
	if it := pendingItem(t, s, "03-z"); it.State != StateRefused || !strings.Contains(it.Refusal, "at most") {
		t.Errorf("03-z: state %s refusal %q", it.State, it.Refusal)
	}
	if it := pendingItem(t, s, "01-x"); it.State != StateRefused || !strings.Contains(it.Refusal, "not an item name") {
		t.Errorf("01-x: state %s refusal %q", it.State, it.Refusal)
	}
	if it := pendingItem(t, s, "02-y"); it.State != StateRefused || !strings.Contains(it.Refusal, "itself") {
		t.Errorf("02-y: state %s refusal %q", it.State, it.Refusal)
	}
}

func TestAddWithRefusesABadAfter(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "v.sh")
	writeFile(t, src, "echo v\n", 0o600)
	if _, _, err := d.AddWith(src, Headers{What: "w", Why: "y", After: []string{"enable"}}, false); err == nil {
		t.Error("AddWith accepted an AFTER name with no NN- number")
	}
	writeFile(t, src, "# AFTER: 01-a\necho v\n", 0o600)
	if _, _, err := d.AddWith(src, Headers{What: "w", Why: "y", After: []string{"01-b"}}, false); err == nil {
		t.Error("AddWith accepted --after over a file that already has an AFTER line")
	}
}

// A hostile AFTER line with a huge number of names parses in one linear
// pass and is refused as over the cap.
func TestSplitAfterIsLinearAndCapped(t *testing.T) {
	var b strings.Builder
	for i := range 165_000 {
		fmt.Fprintf(&b, "%d-n,", i)
	}
	start := time.Now()
	h := ParseHeaders([]byte("# AFTER: " + b.String() + "\n"))
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("parsing took %v", took)
	}
	if len(h.After) != MaxAfter+1 {
		t.Errorf("parsed %d names, want MaxAfter+1 = %d (enough to refuse)", len(h.After), MaxAfter+1)
	}
	if why, permanent := (&Desk{}).unmetAfter("99-x", h.After); !permanent || !strings.Contains(why, "at most") {
		t.Errorf("unmetAfter = %q, %v; want refused as over the cap", why, permanent)
	}
}

func TestParseHeadersReadsAfter(t *testing.T) {
	h := ParseHeaders([]byte("# AFTER: 01-a, 02-b,01-a\n# AFTER: 03-c\n"))
	if !slices.Equal(h.After, []string{"01-a", "02-b"}) {
		t.Errorf("After = %q, want the first line's two names, each once", h.After)
	}
}

// An AFTER gate reads only the exit code the desk recorded in meta, never
// the predecessor's own log: a run that printed EXIT=0 last, and never got
// an exit_code (lost, then skipped), does not release the item after it.
func TestAfterIgnoresAnExitLineInTheLog(t *testing.T) {
	d := openDesk(t)
	writeFile(t, d.LogPath("01-enable"), "output\nEXIT=0\n", 0o600)
	dropPending(t, d, "02-verify.sh", "# WHAT: x\n# WHY: y\n# AFTER: 01-enable\necho v\n")
	if it := pendingItem(t, scan(t, d), "02-verify"); it.State != StateHeld {
		t.Fatalf("state %s hold %q; want held, the log's EXIT= line does not count", it.State, it.Hold)
	}
	if _, err := d.Claim("02-verify", queuedSHA(t, d, "02-verify")); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim = %v, want ErrRefused", err)
	}
}

// renameNoReplace never replaces its target: a run's record in running/
// survives a claim that lost the race with the name check (forgectl#1157).
func TestRenameNoReplaceKeepsTheTarget(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(d.Path(), DirPending, "01-a.sh")
	dst := filepath.Join(d.Path(), DirRunning, "01-a.sh")
	writeFile(t, src, "new\n", 0o600)
	writeFile(t, dst, "running record\n", 0o600)
	if err := d.renameNoReplace(DirPending, DirRunning, "01-a.sh"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("renameNoReplace over an existing target = %v, want fs.ErrExist", err)
	}
	if readFile(t, dst) != "running record\n" || readFile(t, src) != "new\n" {
		t.Fatal("a refused rename changed a file")
	}
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := d.renameNoReplace(DirPending, DirRunning, "01-a.sh"); err != nil {
		t.Fatalf("renameNoReplace to a free name = %v", err)
	}
	if readFile(t, dst) != "new\n" {
		t.Error("the moved file has other bytes")
	}
	if err := d.renameNoReplace(DirPending, DirRunning, "01-a.sh"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("renameNoReplace of a missing source = %v, want fs.ErrNotExist", err)
	}
}

// When the source cannot be removed after linking, the new link goes, so
// the file is never left under both names.
func TestLinkUnlinkDropsTheLinkWhenTheSourceStays(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), "a\n", 0o600)
	f, err := os.Open(dir) //nolint:gosec // G304: a directory under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test
	prev := unlinkSource
	unlinkSource = func(int, string, int) error { return unix.EIO }
	t.Cleanup(func() { unlinkSource = prev })
	fd := int(f.Fd())
	if err := linkUnlink(fd, "a", fd, "b"); err == nil {
		t.Fatal("linkUnlink succeeded with the unlink failing")
	}
	if _, err := os.Stat(filepath.Join(dir, "b")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the new link was left: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a")); err != nil || fi.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Errorf("a: %v; want it in place with one link", err)
	}
}

// A pending item with a second link (another desk's link-and-unlink claim
// in flight) is refused by Claim and left where it is, never moved to
// skipped/ out from under that claim.
func TestClaimLeavesATwoLinkItemInPlace(t *testing.T) {
	d := openDesk(t)
	a := addScript(t, d, "a.sh", "echo a\n")
	scan(t, d)
	p := filepath.Join(d.Path(), DirPending, a.Name+".sh")
	if err := os.Link(p, filepath.Join(t.TempDir(), "other")); err != nil {
		t.Skipf("no hard link across these directories: %v", err)
	}
	if _, err := d.Claim(a.Name, a.SHA256); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim = %v, want ErrRefused", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the item left pending/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), DirSkipped, a.Name+".sh")); err == nil {
		t.Error("the item was moved to skipped/")
	}
}

// Known counts a done run only with a recorded exit code: a legacy run (an
// EXIT= log, no meta) would hold an AFTER item for ever.
func TestKnownNeedsARecordedExit(t *testing.T) {
	d := openDesk(t)
	writeFile(t, d.LogPath("01-legacy"), "EXIT=0\n", 0o600)
	if d.Known("01-legacy") {
		t.Error("Known(01-legacy) with only an EXIT= log = true")
	}
	c := queue(t, d, "b.sh", "echo b\n")
	if !d.Known(c.Name) {
		t.Errorf("Known(%s) running = false", c.Name)
	}
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(3, "exit"); err != nil {
		t.Fatal(err)
	}
	if !d.Known(c.Name) {
		t.Errorf("Known(%s) done with exit 3 = false", c.Name)
	}
}

// The portable fallback refuses an existing target too.
func TestLinkUnlinkKeepsTheTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), "a\n", 0o600)
	writeFile(t, filepath.Join(dir, "b"), "b\n", 0o600)
	f, err := os.Open(dir) //nolint:gosec // G304: a directory under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test
	fd := int(f.Fd())
	if err := linkUnlink(fd, "a", fd, "b"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("linkUnlink over b = %v, want fs.ErrExist", err)
	}
	if err := linkUnlink(fd, "a", fd, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a still exists after the move: %v", err)
	}
	if readFile(t, filepath.Join(dir, "c")) != "a\n" {
		t.Error("c does not hold a's bytes")
	}
}

// A run of the same name that reaches running/ between Claim's check and
// its rename is not replaced: the claim is refused and the item stays
// pending, unclaimed.
func TestClaimRefusesWhenARunArrivesAfterTheCheck(t *testing.T) {
	d := openDesk(t)
	a := addScript(t, d, "a.sh", "echo a\n")
	scan(t, d)
	record := filepath.Join(d.Path(), DirRunning, a.Name+".sh")
	afterClaimChecks = func() { writeFile(t, record, "another run\n", 0o600) }
	t.Cleanup(func() { afterClaimChecks = func() {} })
	if _, err := d.Claim(a.Name, a.SHA256); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim = %v, want ErrRefused", err)
	}
	if readFile(t, record) != "another run\n" {
		t.Error("the running record was replaced")
	}
	if m, ok, err := d.readMeta(DirPending, a.Name); err != nil || !ok || m.ClaimedAt != nil {
		t.Errorf("pending meta %+v ok=%v err=%v; want it back unclaimed", m, ok, err)
	}
}

// ApplyItemEnv is the one place an item's cwd and environment are set
// (forgectl#1071).
func TestApplyItemEnv(t *testing.T) {
	t.Setenv("BASH_ENV", "/tmp/hook")
	cmd := exec.Command("/bin/true") //nolint:noctx // never started
	if err := ApplyItemEnv(cmd); err != nil {
		t.Fatal(err)
	}
	home, _ := HomeDir()
	if cmd.Dir != home || cmd.Dir == "" {
		t.Errorf("Dir = %q, want %q", cmd.Dir, home)
	}
	if slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "BASH_ENV=") }) {
		t.Errorf("Env keeps BASH_ENV: %q", cmd.Env)
	}

	t.Setenv("HOME", "relative")
	cmd = exec.Command("/bin/true") //nolint:noctx // never started
	if err := ApplyItemEnv(cmd); err == nil || cmd.Dir != "" || cmd.Env != nil {
		t.Errorf("relative HOME: err %v, Dir %q, Env %d entries; want an error and cmd untouched", err, cmd.Dir, len(cmd.Env))
	}
}
