package pr

// Test plan for findings_marker.go (forgectl#558, part 2)
//
// A findings dir is live, and never removed, while the session record its
// owner marker names still exists and is local. It is stale once that record
// is gone. A dir with no marker is refused.
//
//   [x] A dir whose marker names an existing local record survives
//       `cleanup --older-than 0 --apply`, is absent from the preview, and
//       writes no audit row
//   [x] A dir that turns live between the preview and the apply is skipped
//       by FindingsRemove's under-lock re-check
//   [x] A marker naming a record that is gone: removed
//   [x] A marker naming a record that exists but is not local: removed
//   [x] A marker naming a record that exists but does not decode: kept
//   [x] An unmarked dir: absent from the preview and refused by FindingsRemove
//   [x] A garbled marker ("../" climb to a live record outside the sessions
//       dir): stale, removed, and the climb is never followed
//   [x] An oversized marker: stale, removed
//   [x] PrepareLocal writes a marker naming its own record; cleanup keeps the
//       dir while the record lives and removes it once the record is gone
//   [x] A marker write that fails partway (injected through WithRecordFS)
//       publishes nothing: no marker, no temp, and the dir is kept
//   [x] An empty, truncated, or newline-less marker next to a live record is
//       incomplete, so unmarked, so kept
//   [x] No session record is committed while any owner marker exists yet
//       (a WithRecordFS double checks at the record's commit Rename)
//   (findings_marker_unix_test.go)
//   [x] A marker that is a symlink to a valid marker is not followed: stale
//   [x] A marker that is a FIFO fails fast and reads as stale

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveRecord writes a session record named name into c's sessions dir with
// the given body. The liveness check reads only the "local" field.
func liveRecord(t *testing.T, c *Client, name, body string) {
	t.Helper()
	if err := os.MkdirAll(c.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.sessionsDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const ownerRecord = "local-deadbee-14593470-1700000000000000000.json"

// markedDir creates a findings dir whose marker names record.
func markedDir(t *testing.T, store, suffix, record string) string {
	t.Helper()
	d := filepath.Join(store, findingsDirPrefix+suffix)
	mustMkdirUnmarked(t, d)
	writeMarker(t, d, record+"\n")
	return d
}

func wantKept(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("findings dir %q was removed, want it kept: %v", dir, err)
	}
}

func wantGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("findings dir %q still exists, want it removed (stat err %v)", dir, err)
	}
}

func contains558(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

// Mutation that turns it red: delete the skipLiveFindingsDir call from
// FindingsCleanup's scan (the preview offers the dir) or from
// removeFindingsDirAudited (the apply removes it), and return false for
// findingsLive in skipLiveFindingsDir (both).
func TestFindingsCleanup_KeepsLiveReviewDir(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	liveRecord(t, c, ownerRecord, `{"local":true}`)
	live := markedDir(t, store, "live", ownerRecord)

	preview, err := c.FindingsCleanup(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("FindingsCleanup preview: %v", err)
	}
	if contains558(preview, live) {
		t.Errorf("preview offers the live dir %q", live)
	}
	removed, err := c.FindingsCleanup(context.Background(), 0, true)
	if err != nil {
		t.Fatalf("FindingsCleanup apply: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing", removed)
	}
	wantKept(t, live)
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %v, want none for a kept dir", rows)
	}
}

// The TOCTOU half: the preview ran while the dir was stale, then the dir's
// record appeared (in production: its marker only ever names a record that
// already exists, so this stands in for any gap between preview and apply).
// FindingsRemove must re-ask under the lock rather than trust the preview.
//
// Mutation that turns it red: delete the skipLiveFindingsDir call from
// removeFindingsDirAudited.
func TestFindingsRemove_RechecksLivenessUnderLock(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := markedDir(t, store, "turned-live", ownerRecord)

	preview, err := c.FindingsCleanup(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("FindingsCleanup preview: %v", err)
	}
	if !contains558(preview, d) {
		t.Fatalf("preview = %v, want the still-stale dir offered", preview)
	}
	liveRecord(t, c, ownerRecord, `{"local":true}`)

	removed, err := c.FindingsRemove(context.Background(), preview)
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing", removed)
	}
	wantKept(t, d)
}

// Mutation that turns it red: in ownerRecordLive, return true on any open
// error (drop the fs.ErrNotExist test), so a gone record reads as live.
func TestFindingsCleanup_RemovesDirWhoseRecordIsGone(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := markedDir(t, store, "orphan", ownerRecord)

	removed, err := c.FindingsCleanup(context.Background(), 0, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if !contains558(removed, d) {
		t.Errorf("removed = %v, want %q", removed, d)
	}
	wantGone(t, d)
}

// Mutation that turns it red: in ownerRecordLive, return true instead of
// rec.Local after the decode.
func TestFindingsCleanup_RemovesDirWhoseRecordIsNotLocal(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	liveRecord(t, c, ownerRecord, `{"ref":"acme/widget#1"}`)
	d := markedDir(t, store, "remote-owner", ownerRecord)

	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantGone(t, d)
}

// Mutation that turns it red: in ownerRecordLive, return false when
// json.Unmarshal fails.
func TestFindingsCleanup_KeepsDirWhoseRecordDoesNotDecode(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	liveRecord(t, c, ownerRecord, `{"local":true`)
	d := markedDir(t, store, "undecodable-owner", ownerRecord)

	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantKept(t, d)
}

// Mutation that turns it red: return false for findingsUnmarked in
// skipLiveFindingsDir (both the preview and FindingsRemove then take it).
func TestFindingsCleanup_RefusesUnmarkedDir(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := filepath.Join(store, findingsDirPrefix+"legacy")
	mustMkdirUnmarked(t, d)

	preview, err := c.FindingsCleanup(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("FindingsCleanup preview: %v", err)
	}
	if contains558(preview, d) {
		t.Errorf("preview offers the unmarked dir %q", d)
	}
	removed, err := c.FindingsRemove(context.Background(), []string{d})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing", removed)
	}
	wantKept(t, d)
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %v, want none for a refused dir", rows)
	}
}

// A marker is reviewer-writable, so a "../" climb must not reach a record
// outside the sessions dir. The file it climbs to is a live local record, so
// following it would keep the dir.
//
// Mutation that turns it red: drop the validFindingsOwnerName check from
// readFindingsMarker (and its ".json"/charset test lets "../" through).
func TestFindingsCleanup_GarbledMarkerIsStaleAndNeverFollowed(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	outside := filepath.Join(filepath.Dir(c.sessionsDir), ownerRecord)
	if err := os.WriteFile(outside, []byte(`{"local":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := markedDir(t, store, "climb", "../"+ownerRecord)

	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantGone(t, d)
}

// An oversized marker reads as stale. The name inside is valid-charset but
// longer than any file name can be, so if it ever reached ownerRecordLive the
// open would fail ENAMETOOLONG, which reads as live (uncertain → keep).
//
// Mutation that turns it red: in readFindingsMarker, read f without the
// io.LimitReader AND drop the maxFindingsMarkerBytes check, AND drop the
// 255-byte limit in validFindingsOwnerName. Any one of the three still
// refuses the marker (the LimitReader alone truncates away the ".json"), so
// removing just one or two of them stays green; the cap is an allocation
// bound first and a verdict input only in combination.
func TestFindingsCleanup_OversizedMarkerIsStale(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := markedDir(t, store, "big", strings.Repeat("a", maxFindingsMarkerBytes)+".json")

	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantGone(t, d)
}

// PrepareLocal marks its dir with its own record, so the live-review case is
// the real one, not a hand-built fixture.
//
// Mutation that turns it red: delete the writeFindingsMarker call from
// PrepareLocal (the marker check fails, and the dir stays unmarked so the
// post-teardown cleanup refuses it). No test here pins the write coming
// AFTER recordPrepared; that ordering is argued on writeFindingsMarker.
func TestPrepareLocal_MarkerKeepsDirWhileRecordLives(t *testing.T) {
	c := testClient(t, localGitRunner())
	sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sess.Workspace)
		_ = os.RemoveAll(sess.FindingsDir)
	})

	got, err := os.ReadFile(filepath.Join(sess.FindingsDir, findingsOwnerMarker))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if want := filepath.Base(sess.Path) + "\n"; string(got) != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}

	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup while live: %v", err)
	}
	wantKept(t, sess.FindingsDir)

	if err := os.Remove(sess.Path); err != nil {
		t.Fatalf("remove record: %v", err)
	}
	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup after the record is gone: %v", err)
	}
	wantGone(t, sess.FindingsDir)
}

// markerFaultFS is a recordFS double that short-writes only the owner
// marker (its temp or its final name) (as ENOSPC or EIO would cut it off) and passes every
// other call, including the session record's writes, to the real filesystem.
type markerFaultFS struct{ osRecordFS }

type shortWriteFile struct{ recordFile }

func (f shortWriteFile) Write(p []byte) (int, error) {
	n, err := f.recordFile.Write(p[:len(p)/2])
	if err != nil {
		return n, err
	}
	return n, errInjected
}

func (m markerFaultFS) OpenExclusive(path string) (recordFile, error) {
	file, err := m.osRecordFS.OpenExclusive(path)
	if err != nil {
		return file, err
	}
	// Both names, so an in-place writer (no temp) is faulted too.
	if base := filepath.Base(path); base != findingsMarkerTemp && base != findingsOwnerMarker {
		return file, nil
	}
	return shortWriteFile{file}, nil
}

// A marker write that fails partway must leave the dir unmarked (kept), never
// holding a partial marker that reads as stale next to a live record.
//
// Mutations that turn it red:
//   - publish in place: OpenExclusive the final marker name and write into it,
//     with no temp and no link (the partial marker is left under its real
//     name, so the "no marker" assertion fails);
//   - drop the werr check before the link (the half-written temp is
//     published).
func TestPrepareLocal_FailedMarkerWriteLeavesDirUnmarkedAndKept(t *testing.T) {
	c := New(localGitRunner(),
		WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithRecordFS(markerFaultFS{}),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sess.Workspace)
		_ = os.RemoveAll(sess.FindingsDir)
	})

	for _, name := range []string{findingsOwnerMarker, findingsMarkerTemp} {
		if _, err := os.Lstat(filepath.Join(sess.FindingsDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed marker write (lstat err %v), want it absent", name, err)
		}
	}
	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantKept(t, sess.FindingsDir)
}

// Defence in depth behind the atomic publish: a marker that is empty, or cut
// off before its newline, next to a live record keeps the dir.
//
// Mutation that turns it red: delete the errFindingsMarkerIncomplete case in
// findingsDirLiveness, so an incomplete marker falls through to stale.
func TestFindingsCleanup_IncompleteMarkerIsKept(t *testing.T) {
	for label, content := range map[string]string{
		"empty":     "",
		"truncated": ownerRecord[:len(ownerRecord)/2],
		"no-eol":    ownerRecord,
	} {
		t.Run(label, func(t *testing.T) {
			store := t.TempDir()
			c := findingsClient(t, store)
			liveRecord(t, c, ownerRecord, `{"local":true}`)
			d := markedDir(t, store, label, "")
			writeMarker(t, d, content)

			if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
				t.Fatalf("FindingsCleanup: %v", err)
			}
			wantKept(t, d)
		})
	}
}

// orderFS fails the test if any findings dir already holds an owner marker at
// the moment a session record is committed (the atomic writer's Rename into
// the sessions dir). A marker that exists before its record would read as
// stale, and a cleanup in that window would remove a live review's dir.
type orderFS struct {
	osRecordFS
	t           *testing.T
	sessionsDir string
	findingsDir string
	commits     int
}

func (o *orderFS) Rename(oldpath, newpath string) error {
	if filepath.Dir(newpath) == o.sessionsDir && filepath.Ext(newpath) == ".json" {
		o.commits++
		markers, _ := filepath.Glob(filepath.Join(o.findingsDir, "*", findingsOwnerMarker))
		if len(markers) != 0 {
			o.t.Errorf("record %s committed while markers already exist: %v", filepath.Base(newpath), markers)
		}
	}
	return o.osRecordFS.Rename(oldpath, newpath)
}

// Mutation that turns it red: write the marker right after MkdirTemp in
// PrepareLocal, naming breadcrumbFilename(ref, sess.CreatedAt), instead of
// after recordPrepared.
func TestPrepareLocal_MarkerIsWrittenAfterTheRecord(t *testing.T) {
	ofs := &orderFS{t: t, sessionsDir: t.TempDir(), findingsDir: t.TempDir()}
	c := New(localGitRunner(),
		WithSessionsDir(ofs.sessionsDir), WithFindingsDir(ofs.findingsDir),
		WithRecordFS(ofs),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sess.Workspace)
		_ = os.RemoveAll(sess.FindingsDir)
	})
	if ofs.commits == 0 {
		t.Fatal("no record commit went through the double; the ordering check never ran")
	}
	if _, err := os.Stat(filepath.Join(sess.FindingsDir, findingsOwnerMarker)); err != nil {
		t.Fatalf("no marker after PrepareLocal: %v", err)
	}
}
