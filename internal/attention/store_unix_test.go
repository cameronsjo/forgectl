//go:build unix

package attention

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSession = "0b5d6c1e-1111-4222-8333-944455556666"

func testRecord(session string) Record {
	return Record{
		Session: session,
		Pane:    "w1:p2",
		Socket:  "/tmp/herdr-test.sock",
		Cwd:     "widget",
		Turn:    "turn-1",
		At:      time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		Items:   []Item{{Kind: KindQuestion, N: 1, Text: "Ship it?", Rec: RecNow}},
	}
}

// openTestSpool returns a spool under a fresh state base, and the spool's
// directory path.
func openTestSpool(t *testing.T) (*Spool, string) {
	t.Helper()
	base := t.TempDir()
	s, err := OpenSpool(base, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, filepath.Join(base, "cadence", "attention")
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestPutWritesAPrivateFileInAPrivateDir(t *testing.T) {
	s, dir := openTestSpool(t)
	if err := s.Put(testRecord(testSession)); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir).Perm(); m != 0o700 {
		t.Errorf("spool dir mode = %o, want 700", m)
	}
	file := filepath.Join(dir, testSession+".json")
	if m := mode(t, file); !m.IsRegular() || m.Perm() != 0o600 {
		t.Errorf("record mode = %v, want a regular 0600 file", m)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("spool holds %d entries, want only the record (no tmp left behind)", len(entries))
	}
	var got Record
	data, err := os.ReadFile(file) //nolint:gosec // G304: a test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil || got.Session != testSession || len(got.Items) != 1 {
		t.Errorf("record = %+v (%v)", got, err)
	}
}

func TestPutRefusesInvalidSessionIDs(t *testing.T) {
	s, dir := openTestSpool(t)
	for _, id := range []string{"", "../escape", "-flag", "a/b", "a.json", ".hidden", strings.Repeat("a", 65), "tab\tid"} {
		if err := s.Put(testRecord(id)); err == nil {
			t.Errorf("session %q was accepted", id)
		}
		if err := s.Remove(id); err == nil {
			t.Errorf("rm of session %q was accepted", id)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused put wrote %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "escape.json")); err == nil {
		t.Error("a traversing id wrote outside the spool")
	}
}

func TestPutRefusesInvalidFields(t *testing.T) {
	s, _ := openTestSpool(t)
	for name, edit := range map[string]func(*Record){
		"pane with a leading dash": func(r *Record) { r.Pane = "-w1:p2" },
		"pane with a slash":        func(r *Record) { r.Pane = "w1/p2" },
		"relative socket":          func(r *Record) { r.Socket = "herdr.sock" },
		"cwd with a slash":         func(r *Record) { r.Cwd = "a/b" },
		"turn with a space":        func(r *Record) { r.Turn = "turn 1" },
		"empty turn":               func(r *Record) { r.Turn = "" },
	} {
		r := testRecord(testSession)
		edit(&r)
		if err := s.Put(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPutRefusesASymlinkedRecord(t *testing.T) {
	s, dir := openTestSpool(t)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, testSession+".json")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(testRecord(testSession)); err == nil {
		t.Error("put over a symlinked record was accepted")
	}
	if err := s.Remove(testSession); err == nil {
		t.Error("rm of a symlinked record was accepted")
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep\n" { //nolint:gosec // G304: a test path
		t.Errorf("the symlink target changed: %q", data)
	}
	if m := mode(t, filepath.Join(dir, testSession+".json")); m&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
}

func TestPutCreatesItsTmpFileExclusively(t *testing.T) {
	s, dir := openTestSpool(t)
	old := tmpSuffix
	tmpSuffix = func() (string, error) { return "fixed", nil }
	t.Cleanup(func() { tmpSuffix = old })
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "."+testSession+".json.tmp-fixed")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(testRecord(testSession)); err == nil {
		t.Error("put through a planted tmp name was accepted")
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep\n" { //nolint:gosec // G304: a test path
		t.Errorf("the planted tmp target was written: %q", data)
	}
	if _, err := os.Lstat(filepath.Join(dir, testSession+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a record appeared after a failed put: %v", err)
	}
}

func TestOpenSpoolRefusesASymlinkedDir(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "cadence"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(base, "cadence", "attention")); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		if s, err := OpenSpool(base, create); err == nil {
			_ = s.Close()
			t.Errorf("create=%v: a symlinked spool dir was opened", create)
		}
	}
}

func TestOpenSpoolRefusesAWideDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "cadence", "attention")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: the test needs a wide mode to see it refused
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		s, err := OpenSpool(base, create)
		if err == nil {
			_ = s.Close()
			t.Errorf("create=%v: a 0755 spool dir was opened", create)
		}
	}
	if m := mode(t, dir).Perm(); m != 0o755 {
		t.Errorf("the refused dir was changed to %o", m)
	}
}

func TestOpenSpoolAbsentWithoutCreate(t *testing.T) {
	if _, err := OpenSpool(t.TempDir(), false); !errors.Is(err, ErrNoSpool) {
		t.Errorf("err = %v, want ErrNoSpool", err)
	}
}

func TestRemove(t *testing.T) {
	s, dir := openTestSpool(t)
	if err := s.Put(testRecord(testSession)); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(testSession); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, testSession+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record still there: %v", err)
	}
	if err := s.Remove(testSession); err != nil {
		t.Errorf("rm of an absent record = %v, want nil", err)
	}
}

func TestListSkipsMalformedRecords(t *testing.T) {
	s, dir := openTestSpool(t)
	good := testRecord(testSession)
	if err := s.Put(good); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bad-json.json", "{not json")
	other := testRecord("other-session")
	data, _ := json.Marshal(other)
	write("mismatch.json", string(data)) // names a session other than its file
	bad := testRecord("bad-pane")
	bad.Pane = "-rf"
	data, _ = json.Marshal(bad)
	write("bad-pane.json", string(data))
	// A record that is valid but for one unknown field.
	data, _ = json.Marshal(testRecord("unknown-field"))
	write("unknown-field.json", strings.Replace(string(data), `"session"`, `"extra":1,"session"`, 1))
	if err := os.Symlink(filepath.Join(dir, testSession+".json"), filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := s.List(time.Now(), DefaultTTL, Liveness{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Session != testSession {
		t.Errorf("records = %+v, want only the good one", recs)
	}
	if skipped != 5 {
		t.Errorf("skipped = %d, want 5", skipped)
	}
}

func TestListCleansAndCapsText(t *testing.T) {
	s, dir := openTestSpool(t)
	r := testRecord(testSession)
	r.Items = nil
	for range 25 {
		r.Items = append(r.Items, Item{Kind: KindQuestion, N: 1, Text: "a\u200bb\u202e" + strings.Repeat("x", 400)})
	}
	data, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, testSession+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := s.List(time.Now(), DefaultTTL, Liveness{})
	if err != nil || skipped != 0 || len(recs) != 1 {
		t.Fatalf("List = %+v, %d, %v", recs, skipped, err)
	}
	if n := len(recs[0].Items); n != MaxItems {
		t.Errorf("items = %d, want %d", n, MaxItems)
	}
	if got := recs[0].Items[0].Text; !strings.HasPrefix(got, "abx") || len([]rune(got)) > MaxText {
		t.Errorf("text = %q, want cleaned and cut", got)
	}
}

func setAge(t *testing.T, dir, session string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(dir, session+".json"), at, at); err != nil {
		t.Fatal(err)
	}
}

func TestPrune(t *testing.T) {
	const sock = "/tmp/herdr-test.sock"
	live := Liveness{Known: true, Socket: sock, Panes: map[string]bool{"w1:p1": true}}
	for _, tt := range []struct {
		name   string
		socket string
		pane   string
		age    time.Duration
		live   Liveness
		kept   bool
	}{
		{"visible dead pane, fresh", sock, "w1:p9", time.Minute, live, false},
		{"visible live pane, past the TTL", sock, "w1:p1", 48 * time.Hour, live, true},
		{"no socket, fresh", "", "", time.Minute, live, true},
		{"no socket, past the TTL", "", "", 25 * time.Hour, live, false},
		{"another server's pane, fresh", "/tmp/other.sock", "w1:p9", time.Minute, live, true},
		{"another server's pane, past the TTL", "/tmp/other.sock", "w1:p1", 25 * time.Hour, live, false},
		{"herdr unavailable, fresh", sock, "w1:p9", time.Minute, Liveness{Socket: sock}, true},
		{"herdr unavailable, past the TTL", sock, "w1:p1", 25 * time.Hour, Liveness{Socket: sock}, false},
		{"caller outside herdr", sock, "w1:p9", time.Minute, Liveness{Known: true, Panes: live.Panes}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, dir := openTestSpool(t)
			r := testRecord(testSession)
			r.Socket, r.Pane = tt.socket, tt.pane
			if err := s.Put(r); err != nil {
				t.Fatal(err)
			}
			setAge(t, dir, testSession, tt.age)
			recs, _, err := s.List(time.Now(), DefaultTTL, tt.live)
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Lstat(filepath.Join(dir, testSession+".json"))
			if kept := statErr == nil; kept != tt.kept {
				t.Errorf("kept = %v, want %v", kept, tt.kept)
			}
			if (len(recs) == 1) != tt.kept {
				t.Errorf("listed %d records, want kept=%v", len(recs), tt.kept)
			}
		})
	}
}

func TestPruneRemovesStaleJunkButNotForeignNames(t *testing.T) {
	s, dir := openTestSpool(t)
	for _, name := range []string{"broken.json", ".broken.json.tmp-abc", "notes.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.List(time.Now(), DefaultTTL, Liveness{}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"broken.json": false, ".broken.json.tmp-abc": false, "notes.txt": true} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", name, exists, want)
		}
	}
}

func TestGet(t *testing.T) {
	s, _ := openTestSpool(t)
	if _, ok, err := s.Get(testSession); ok || err != nil {
		t.Fatalf("Get on an empty spool = %v, %v", ok, err)
	}
	if err := s.Put(testRecord(testSession)); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.Get(testSession)
	if !ok || err != nil || r.Turn != "turn-1" {
		t.Fatalf("Get = %+v, %v, %v", r, ok, err)
	}
}
