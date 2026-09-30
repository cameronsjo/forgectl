package pr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// findingsOwnerMarker is the liveness marker PrepareLocal writes into each
// findings dir it creates (forgectl#558). Its content is one line: the file
// name of the session record that owns the dir.
//
// Records deliberately never persist FindingsDir (see Session.FindingsDir and
// the Launch barrier in launch.go), so without the marker nothing ties a
// findings dir to the review still writing into it, and
// `pr findings cleanup --older-than 0 --apply` could remove a running review's
// deliverable. The marker points the other way, from the dir to the record,
// and leaves the record schema untouched.
//
// The marker sits in the one directory the reviewer agent may write to, so
// its content is hostile input: it is size-capped, never followed as a
// symlink, never blocked on as a FIFO, and must name a plain record file name.
// Tampering can only move the reviewer's OWN dir between states. Rewriting
// the marker to name another live record pins the dir. Garbling it, or
// swapping in a FIFO, makes it stale and removable. Swapping in a symlink,
// or anything else that makes the marker fail to open, and deleting it both
// leave the dir unclassifiable, which is refused. None of those reaches
// another dir, and none makes cleanup remove a dir that another review still
// owns.
const findingsOwnerMarker = ".forgectl-owner"

// maxFindingsMarkerBytes caps the marker read. A record name is well under
// 255 bytes; anything larger was not written by writeFindingsMarker.
const maxFindingsMarkerBytes = 512

// errFindingsMarkerInvalid is readFindingsMarker's refusal of a marker that
// exists but cannot be what writeFindingsMarker wrote.
var errFindingsMarkerInvalid = errors.New("findings owner marker is not valid")

// errFindingsMarkerIncomplete is readFindingsMarker's report of a marker that
// could be an unfinished write rather than a tampered one: an empty file, a
// file with no terminating newline, or one that could not be read to the end.
// It maps to findingsUnmarked (kept), not findingsStale.
var errFindingsMarkerIncomplete = errors.New("findings owner marker is incomplete")

// errFindingsMarkerUnreadable is readFindingsMarker's report of a marker that
// exists but could not be opened: a symlink (ELOOP), a permission error, fd
// exhaustion, an I/O fault. None of those says what the marker holds, so the
// dir cannot be classified and maps to findingsUnmarked (kept), never
// findingsStale (forgectl#659). Only ENOENT is left unwrapped.
var errFindingsMarkerUnreadable = errors.New("findings owner marker cannot be opened")

// openFindingsMarker is the marker opener, a seam so a test can inject open
// errors (EACCES, EMFILE) that root and a healthy process cannot produce. It
// opens name inside dir, the handle on one findings dir, never by path
// (forgectl#685).
var openFindingsMarker = openInRootNoFollowNonblock

// findingsLiveness is the removal verdict for one findings dir.
type findingsLiveness int

const (
	// findingsStale: the marker names no existing local record, or it opens
	// but is garbled, oversized, or not a regular file. Removable.
	findingsStale findingsLiveness = iota
	// findingsLive: the marker names a local record that still exists.
	// Never removed.
	findingsLive
	// findingsUnmarked: no marker, an incomplete one (empty, or cut off
	// before its newline), or one that exists but cannot be opened (#659).
	// That is a dir from a forgectl before #558, one
	// PrepareLocal created but has not marked yet (the marker is written only
	// after the owning record exists), or one whose marker write failed.
	// Refused, because none of these can be told apart from a live review.
	findingsUnmarked
)

// findingsMarkerTemp is the private name the marker is written under before
// it is published. A crash can leave it behind; it is never read, so a dir
// holding only the temp is unmarked, and kept.
const findingsMarkerTemp = findingsOwnerMarker + ".tmp"

// writeFindingsMarker records recordPath's file name as the owner of
// findingsDir, and publishes it ATOMICALLY: the complete, synced content
// appears under findingsOwnerMarker in one step, or nothing does.
//
// That matters because a marker that exists with the wrong content is worse
// than no marker. An unmarked dir is kept, but a marker that does not name a
// live record makes the dir removable. Creating the marker in place and then
// writing into it would leave a window, and after a failed write (ENOSPC, EIO)
// or a crash a permanent state, in which a live review's dir carries an empty
// or truncated marker.
//
// So the content goes to a temp file first: created with O_EXCL (so nothing
// can have pre-placed it, and a symlink at its name is refused rather than
// followed), written in full, synced, closed. It is then published with a
// hard link, which, unlike a rename, refuses to replace an existing marker
// (EEXIST). The temp is removed on every path. Any failure leaves the dir
// unmarked.
//
// Every step goes through a handle on findingsDir, opened from the checked
// store (openFindingsStore) and proven to be the plain directory the store's
// Lstat saw (openFindingsChild), the same binding cleanup's reads use
// (forgectl#685, forgectl#754). A path-based create and link re-resolved
// findingsDir at each call, so a findings dir swapped for a symlink after
// MkdirTemp redirected the marker write to wherever the link pointed. A
// findingsDir that is not a findings dir directly under the store, or not a
// plain directory there, is refused, and so is a store that is not private.
//
// PrepareLocal calls it only AFTER the owning record exists. Written any
// earlier, the marker would name a record that is not there yet, and a
// cleanup running in that window would read the dir as stale and remove it.
func (c *Client) writeFindingsMarker(findingsDir, recordPath string) error {
	name := filepath.Base(recordPath)
	if !validFindingsOwnerName(name) {
		return fmt.Errorf("record name %q cannot be a findings owner", name)
	}
	if !isFindingsStoreChild(c.findingsDir, findingsDir) {
		return errors.New("not a findings dir directly under the findings store; no marker written")
	}
	store, err := c.openFindingsStore()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	dirName := filepath.Base(filepath.Clean(findingsDir))
	info, err := store.Lstat(dirName)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("the findings dir is no longer a plain directory; no marker written")
	}
	dir, err := openFindingsChild(store, dirName, info)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	f, err := createFindingsMarkerTemp(dir, findingsMarkerTemp)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Remove(findingsMarkerTemp) }()
	content := name + "\n"
	n, werr := io.WriteString(f, content)
	if werr == nil && n != len(content) {
		werr = io.ErrShortWrite
	}
	var serr error
	if werr == nil {
		serr = f.Sync()
	}
	if err := errors.Join(werr, serr, f.Close()); err != nil {
		return err
	}
	return dir.Link(findingsMarkerTemp, findingsOwnerMarker)
}

// createFindingsMarkerTemp creates name inside dir, 0600 with O_EXCL, the
// marker's private temp. It is a seam so a test can fail the write, the
// sync, or the close partway; tests that swap it must not call t.Parallel.
var createFindingsMarkerTemp = func(dir *os.Root, name string) (recordFile, error) {
	f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// validFindingsOwnerName reports whether name can be a session record's file
// name: a bare base name ending in ".json", not a dotfile, and drawn from the
// charset breadcrumbFilename produces. That keeps a marker-supplied name from
// reaching outside the sessions dir, or naming the lock or the audit log.
func validFindingsOwnerName(name string) bool {
	if len(name) == 0 || len(name) > 255 || name[0] == '.' || !strings.HasSuffix(name, ".json") {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return true
}

// readFindingsMarker returns the record name dir's marker holds. A missing
// marker is an error errors.Is matches to fs.ErrNotExist.
//
// An INCOMPLETE marker is errFindingsMarkerIncomplete, which the caller keeps:
// an empty file, content with no terminating newline, or a read or stat that
// failed partway. writeFindingsMarker emits the name and its newline as the
// last bytes of one publish, so no write of ours that stopped short can end
// in a newline. The newline is what separates "cut off" from "wrong". This is
// defence in depth, since the atomic publish already means no truncated
// marker of ours ever appears under the marker's name.
//
// An open error other than ENOENT is errFindingsMarkerUnreadable, which the
// caller also keeps: it says nothing about what the marker holds, and a
// transient one (EMFILE, EIO) next to a live record must not delete that
// review's findings (forgectl#659).
//
// Every other failure is errFindingsMarkerInvalid, which the caller treats as
// stale: a FIFO or other non-regular file, content over the cap, or a
// newline-terminated line that is not a record name. Only the dir's own
// reviewer can produce those, and making its own dir removable is the only
// thing it gains.
//
// dir is the handle on the findings dir that cleanup judges and removes, so
// the marker read is the marker of that directory, whatever happens to the
// store's path meanwhile (forgectl#685). The open is
// openInRootNoFollowNonblock, an openat against dir's descriptor with the
// repair-log opener's flags: a symlinked marker fails with ELOOP rather than
// being followed (so it is unreadable, and kept), and a FIFO opens without
// blocking and is then refused by the Fstat check. os.Root.OpenFile would not
// do: it resolves a symlink that stays inside the root, and so would follow a
// marker linked to another marker. The read is capped before it allocates.
func readFindingsMarker(dir *os.Root) (string, error) {
	f, err := openFindingsMarker(dir, findingsOwnerMarker)
	if errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errFindingsMarkerUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: %w", errFindingsMarkerIncomplete, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: it is a %s", errFindingsMarkerInvalid, fileKind(info.Mode()))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFindingsMarkerBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: %w", errFindingsMarkerIncomplete, err)
	}
	if len(data) > maxFindingsMarkerBytes {
		return "", fmt.Errorf("%w: it is over %d bytes", errFindingsMarkerInvalid, maxFindingsMarkerBytes)
	}
	name, terminated := strings.CutSuffix(string(data), "\n")
	if !terminated {
		return "", fmt.Errorf("%w: %d bytes and no terminating newline", errFindingsMarkerIncomplete, len(data))
	}
	if !validFindingsOwnerName(name) {
		return "", fmt.Errorf("%w: it does not name a session record", errFindingsMarkerInvalid)
	}
	return name, nil
}

// ownerRecordLive reports whether the session record named by a marker still
// exists and is a local review. name has already passed
// validFindingsOwnerName, so it is a single component, and it is opened
// with an openat against a handle on sessionsDir (openOwnerRecord), not by a
// joined path (forgectl#754). A missing sessions dir means every record is
// gone.
//
// Only a record that is provably gone, or provably not local, reads as dead.
// Everything uncertain reads as live, because the cost of a wrong "live" is a
// dir kept until the next cleanup, and the cost of a wrong "dead" is a running
// review's deliverable deleted. So a record that is a symlink, a non-regular
// file, unreadable, oversized, or undecodable keeps its dir. The decode is
// tolerant (no DisallowUnknownFields), so a record written by a newer
// forgectl still counts, the same reasoning recordedWorkspaceFor applies.
func (c *Client) ownerRecordLive(name string) bool {
	sessions, err := openDirRoot(c.sessionsDir)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	defer func() { _ = sessions.Close() }()
	f, err := openOwnerRecord(sessions, name)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	data, err := readBreadcrumbBytes(f)
	if err != nil {
		return true
	}
	var rec struct {
		Local bool `json:"local"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return true
	}
	return rec.Local
}

// openOwnerRecord opens a marker-named record inside the sessions dir handle
// with the marker reader's flags: no symlink followed, no FIFO blocked on. It
// is a seam so a test can see the open go through the handle.
var openOwnerRecord = openInRootNoFollowNonblock

// findingsDirLiveness classifies the findings dir dir for removal. dir must
// be a handle from openFindingsChild, so it is a plain directory directly
// under the store; full is its path, for log lines only.
func (c *Client) findingsDirLiveness(dir *os.Root, full string) findingsLiveness {
	name, err := readFindingsMarker(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return findingsUnmarked
	case errors.Is(err, errFindingsMarkerIncomplete):
		slog.Debug("Findings dir has an incomplete owner marker; treating it as unmarked.", "path", full, "error", err)
		return findingsUnmarked
	case errors.Is(err, errFindingsMarkerUnreadable):
		slog.Warn("Findings dir has an owner marker that cannot be opened; keeping it.", "path", full, "error", err)
		return findingsUnmarked
	case err != nil:
		slog.Warn("Findings dir has an invalid owner marker; treating it as stale.", "path", full, "error", err)
		return findingsStale
	case c.ownerRecordLive(name):
		return findingsLive
	default:
		return findingsStale
	}
}

// skipLiveFindingsDir reports whether the dir at full must be kept because
// a live review may still own it, logging a live skip at Info. dir is the
// handle on it and full its path, for log lines only. An unmarked
// dir is only counted into *unmarked; the caller logs one summary line per
// run (warnUnmarkedFindings) instead of a warning per dir per run. It is the
// one place both the preview and the apply-time re-check turn a verdict into
// a skip.
func (c *Client) skipLiveFindingsDir(dir *os.Root, full string, unmarked *int) bool {
	switch c.findingsDirLiveness(dir, full) {
	case findingsLive:
		slog.Info("Skipping findings dir: the review that owns it still has a session record.", "path", full)
		return true
	case findingsUnmarked:
		slog.Debug("Skipping findings dir with no complete owner marker.", "path", full)
		*unmarked++
		return true
	default:
		return false
	}
}

// warnUnmarkedFindings is the one summary line for the unmarked dirs a run
// skipped.
func warnUnmarkedFindings(n int) {
	if n == 0 {
		return
	}
	slog.Warn("Skipped findings dirs with no usable owner marker (missing, incomplete, or failed to open): "+
		"they predate the marker, a review is still starting, or its marker write failed. "+
		"Remove one by hand once no review is using it; see `pr findings` in docs/commands/pr.md.", "count", n)
}
