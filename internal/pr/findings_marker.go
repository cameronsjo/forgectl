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
// swapping in a symlink or FIFO, makes it stale and removable. Deleting it
// makes the dir unmarked, which is refused. None of those reaches another
// dir, and none makes cleanup remove a dir that another review still owns.
const findingsOwnerMarker = ".forgectl-owner"

// maxFindingsMarkerBytes caps the marker read. A record name is well under
// 255 bytes; anything larger was not written by writeFindingsMarker.
const maxFindingsMarkerBytes = 512

// errFindingsMarkerInvalid is readFindingsMarker's refusal of a marker that
// exists but cannot be what writeFindingsMarker wrote.
var errFindingsMarkerInvalid = errors.New("findings owner marker is not valid")

// findingsLiveness is the removal verdict for one findings dir.
type findingsLiveness int

const (
	// findingsStale: the marker names no existing local record, or the
	// marker is unreadable or garbled. Removable.
	findingsStale findingsLiveness = iota
	// findingsLive: the marker names a local record that still exists.
	// Never removed.
	findingsLive
	// findingsUnmarked: no marker at all. That is a dir from a forgectl
	// before #558, or one PrepareLocal created but has not marked yet (the
	// marker is written only after the owning record exists). Refused with a
	// warning, because neither case can be told apart from a live review.
	findingsUnmarked
)

// writeFindingsMarker records recordPath's file name as the owner of
// findingsDir. The file is created exclusively and without following a
// symlink (OpenExclusive), so a pre-placed entry fails the write rather than
// being written through.
//
// PrepareLocal calls it only AFTER the owning record exists. Written any
// earlier, the marker would name a record that is not there yet, and a
// cleanup running in that window would read the dir as stale and remove it.
func writeFindingsMarker(findingsDir, recordPath string) error {
	name := filepath.Base(recordPath)
	if !validFindingsOwnerName(name) {
		return fmt.Errorf("record name %q cannot be a findings owner", name)
	}
	f, err := osRecordFS{}.OpenExclusive(filepath.Join(findingsDir, findingsOwnerMarker))
	if err != nil {
		return err
	}
	_, werr := io.WriteString(f, name+"\n")
	serr := f.Sync()
	cerr := f.Close()
	return errors.Join(werr, serr, cerr)
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
// marker is an error errors.Is matches to fs.ErrNotExist. Every other failure
// (a symlink, a FIFO or other non-regular file, an oversized or garbled
// content) is some other error.
//
// The open is openNoFollowNonblock, the repair-log opener's core: a symlinked
// marker fails with ELOOP rather than being followed, and a FIFO opens
// without blocking and is then refused by the Fstat check. The read is capped
// before it allocates.
func readFindingsMarker(dir string) (string, error) {
	f, err := openNoFollowNonblock(filepath.Join(dir, findingsOwnerMarker), os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: it is a %s", errFindingsMarkerInvalid, fileKind(info.Mode()))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFindingsMarkerBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxFindingsMarkerBytes {
		return "", fmt.Errorf("%w: it is over %d bytes", errFindingsMarkerInvalid, maxFindingsMarkerBytes)
	}
	name := strings.TrimSuffix(string(data), "\n")
	if !validFindingsOwnerName(name) {
		return "", fmt.Errorf("%w: it does not name a session record", errFindingsMarkerInvalid)
	}
	return name, nil
}

// ownerRecordLive reports whether the session record named by a marker still
// exists and is a local review. name has already passed
// validFindingsOwnerName, so the path is a direct child of sessionsDir.
//
// Only a record that is provably gone, or provably not local, reads as dead.
// Everything uncertain reads as live, because the cost of a wrong "live" is a
// dir kept until the next cleanup, and the cost of a wrong "dead" is a running
// review's deliverable deleted. So a record that is a symlink, a non-regular
// file, unreadable, oversized, or undecodable keeps its dir. The decode is
// tolerant (no DisallowUnknownFields), so a record written by a newer
// forgectl still counts, the same reasoning recordedWorkspaceFor applies.
func (c *Client) ownerRecordLive(name string) bool {
	f, err := openNoFollowNonblock(filepath.Join(c.sessionsDir, name), os.O_RDONLY, 0)
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

// findingsDirLiveness classifies the findings dir at full for removal. full
// must already be a plain directory directly under the store: both callers
// run isFindingsStoreChild and a directory check before asking.
func (c *Client) findingsDirLiveness(full string) findingsLiveness {
	name, err := readFindingsMarker(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return findingsUnmarked
	case err != nil:
		slog.Warn("Findings dir has an unreadable owner marker; treating it as stale.", "path", full, "error", err)
		return findingsStale
	case c.ownerRecordLive(name):
		return findingsLive
	default:
		return findingsStale
	}
}

// skipLiveFindingsDir logs and reports whether the dir at full must be kept
// because a live review may still own it. It is the one place both the
// preview and the apply-time re-check turn a verdict into a skip.
func (c *Client) skipLiveFindingsDir(full string) bool {
	switch c.findingsDirLiveness(full) {
	case findingsLive:
		slog.Info("Skipping findings dir: the review that owns it still has a session record.", "path", full)
		return true
	case findingsUnmarked:
		slog.Warn("Skipping findings dir with no owner marker: it predates the marker or its review is still starting. "+
			"Remove it by hand once no review is using it.", "path", full)
		return true
	default:
		return false
	}
}
