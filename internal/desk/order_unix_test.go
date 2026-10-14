//go:build unix

package desk

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	if it.State != StateRefused || !strings.Contains(it.Refusal, "running") {
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
// with exit 0: refused while that one waits, runs, or failed, and claimable
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
		if it.State != StateRefused || !strings.Contains(it.Refusal, "has not run") || !slices.Equal(it.After, []string{enable.Name}) {
			t.Fatalf("before %s ran: state %s refusal %q after %q", enable.Name, it.State, it.Refusal, it.After)
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
		if it := pendingItem(t, scan(t, d), verify.Name); it.State != StateRefused || !strings.Contains(it.Refusal, "still running") {
			t.Fatalf("while %s runs: state %s refusal %q", enable.Name, it.State, it.Refusal)
		}
		if err := run.Finish(tc.rc, "exit"); err != nil {
			t.Fatal(err)
		}

		it = pendingItem(t, scan(t, d), verify.Name)
		_, err = d.Claim(verify.Name, verify.SHA256)
		if tc.claimable {
			if it.State != StateWaiting || err != nil {
				t.Fatalf("after rc 0: state %s refusal %q, Claim = %v; want it waiting and claimable", it.State, it.Refusal, err)
			}
			continue
		}
		if it.State != StateRefused || !strings.Contains(it.Refusal, tc.reason) || !errors.Is(err, ErrRefused) {
			t.Fatalf("after rc %d: state %s refusal %q, Claim = %v; want refused (%s)", tc.rc, it.State, it.Refusal, err, tc.reason)
		}
	}
}

// A hand-dropped AFTER line that names no item, or the item itself, keeps
// the item refused.
func TestAfterLineThatCannotBeMetIsRefused(t *testing.T) {
	d := openDesk(t)
	dropPending(t, d, "01-x.sh", "# WHAT: x\n# WHY: y\n# AFTER: ../etc\necho x\n")
	dropPending(t, d, "02-y.sh", "# WHAT: x\n# WHY: y\n# AFTER: 02-y\necho y\n")
	s := scan(t, d)
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

func TestParseHeadersReadsAfter(t *testing.T) {
	h := ParseHeaders([]byte("# AFTER: 01-a, 02-b\n# AFTER: 03-c\n"))
	if !slices.Equal(h.After, []string{"01-a", "02-b"}) {
		t.Errorf("After = %q, want the first line's two names", h.After)
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
