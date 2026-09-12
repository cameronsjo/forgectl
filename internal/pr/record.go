package pr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// breadcrumbVersion is the record schema this build writes when it writes a
// versioned record. A record without a version field is legacy and loads
// under the pre-version rules.
const breadcrumbVersion = 2

// Sentinels for writeRecordAtomic's expectRevision argument. Any positive
// value means "the destination must exist and decode to exactly that
// revision".
const (
	// expectNewRecord means the destination must NOT exist.
	expectNewRecord = 0
	// expectLegacyRecord means the destination must exist as a legacy (no
	// version) record — the one shape that carries no revision to compare.
	expectLegacyRecord = -1
)

// errRecordRevisionMismatch is the compare-and-write refusal. The message is
// written for the operator who hits it through no fault of their own (two
// terminals): it says what happened and what to do, and never the word
// "revision".
var errRecordRevisionMismatch = errors.New(
	"another forgectl pr command changed this session record first; re-run to act on its current state")

// recordFile is the handle the writer needs: write, flush to disk, close.
type recordFile interface {
	io.Writer
	Sync() error
	Close() error
}

// recordFS is the filesystem seam under the atomic record writer. Production
// is osRecordFS; tests inject a fault-and-log double so every crash cell in
// the writer (open, write, sync, rename, dirsync, cleanup) is deterministic
// and in-process.
type recordFS interface {
	// OpenExclusive creates path 0600 with O_EXCL and O_NOFOLLOW (where the
	// platform has it) — a private temp nothing else can have pre-placed.
	OpenExclusive(path string) (recordFile, error)
	Rename(oldpath, newpath string) error
	// SyncDir fsyncs the directory so a completed rename is durable.
	SyncDir(path string) error
	Remove(path string) error
	ReadFile(path string) ([]byte, error)
	Lstat(path string) (fs.FileInfo, error)
}

// osRecordFS is the production recordFS. OpenExclusive lives in the
// platform-split files.
type osRecordFS struct{}

func (osRecordFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osRecordFS) SyncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // the sessions dir, validated by the caller
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (osRecordFS) Remove(path string) error { return os.Remove(path) }

func (osRecordFS) ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a path inside the sessions dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBreadcrumbBytes(f)
}

func (osRecordFS) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }

// encodeBreadcrumb is the ONE encoder, paired with decodeBreadcrumb: indented
// JSON, newline-terminated, bounded by maxBreadcrumbRecordBytes.
func encodeBreadcrumb(bc Breadcrumb) ([]byte, error) {
	// termsafe:allow-raw-json persisted PR breadcrumb, never command output
	data, err := json.MarshalIndent(bc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal breadcrumb: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxBreadcrumbRecordBytes {
		return nil, errBreadcrumbRecordTooLarge
	}
	return data, nil
}

// writeRecordAtomic replaces dir/name with data so that a crash at any point
// leaves either the previous record or the new one, never a torn file:
//
//	compare on-disk revision against expectRevision
//	open a private same-directory temp (0600, O_EXCL, O_NOFOLLOW)
//	full write, short-write check, fsync, close
//	revalidate the destination, rename over it
//	fsync the directory
//
// The directory sync is the step with no precedent in this repo, and it is
// the one the durability claim rests on: without it a completed rename can
// vanish on power loss. A dirsync error is returned — the rename has already
// landed, so the new revision is authoritative and the caller logs the
// uncertainty rather than retrying the write.
//
// Compare-and-write: expectNewRecord requires an absent destination; a
// positive expectRevision requires a version-2 record at exactly that
// revision; expectLegacyRecord requires a legacy record. An ABSENT destination
// against a positive expectRevision is a mismatch, not a create — a record a
// peer tore down between our read and our write must not be resurrected.
func writeRecordAtomic(rfs recordFS, dir, name string, data []byte, expectRevision int) error {
	if name == "" || filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return fmt.Errorf("record name %s is not a plain basename", termsafe.QuotePath(name))
	}
	if len(data) > maxBreadcrumbRecordBytes {
		return errBreadcrumbRecordTooLarge
	}
	dest := filepath.Join(dir, name)
	if err := checkRecordRevision(rfs, dest, expectRevision); err != nil {
		return err
	}

	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("derive temp record name: %w", err)
	}
	tmp := filepath.Join(dir, "."+name+".tmp-"+suffix)
	f, err := rfs.OpenExclusive(tmp)
	if err != nil {
		return fmt.Errorf("open temp record: %w", termsafe.Error(err))
	}
	// A temp that cannot be removed is harmless clutter — List enumerates only
	// .json names and this one starts with a dot — but an operator diagnosing
	// debris in the sessions dir deserves a line naming it.
	discard := func() {
		if err := rfs.Remove(tmp); err != nil {
			slog.Warn("Failed to remove a temp session record after a write failure; it is never read, but it is left behind.",
				"path", tmp, "error", err)
		}
	}

	n, err := f.Write(data)
	if err != nil {
		_ = f.Close()
		discard()
		return fmt.Errorf("write temp record: %w", err)
	}
	if n != len(data) {
		_ = f.Close()
		discard()
		return fmt.Errorf("write temp record: %w (wrote %d of %d bytes)", io.ErrShortWrite, n, len(data))
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		discard()
		return fmt.Errorf("sync temp record: %w", err)
	}
	if err := f.Close(); err != nil {
		discard()
		return fmt.Errorf("close temp record: %w", err)
	}

	// The destination is re-proved immediately before the rename: the check at
	// the top ran before any I/O, and a peer, a symlink swap, or a teardown
	// could have moved in since.
	if err := checkRecordRevision(rfs, dest, expectRevision); err != nil {
		discard()
		return err
	}
	if err := rfs.Rename(tmp, dest); err != nil {
		discard()
		return fmt.Errorf("rename record into place: %w", termsafe.Error(err))
	}
	if err := rfs.SyncDir(dir); err != nil {
		return fmt.Errorf("sync pr sessions dir after writing %s (the record is written; its durability could not be confirmed): %w",
			termsafe.QuotePath(name), err)
	}
	return nil
}

// checkRecordRevision is the compare half of compare-and-write. It reads
// through rfs so a test can observe it, and it refuses anything at dest that
// is not a regular file — a symlink there is never written through.
func checkRecordRevision(rfs recordFS, dest string, expect int) error {
	info, err := rfs.Lstat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if expect == expectNewRecord {
			return nil
		}
		return fmt.Errorf("%w: no record exists at %s", errRecordRevisionMismatch, termsafe.QuotePath(dest))
	case err != nil:
		return fmt.Errorf("stat record %s: %w", termsafe.QuotePath(dest), termsafe.Error(err))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("record %s is not a regular file; refusing to write over it", termsafe.QuotePath(dest))
	}
	if expect == expectNewRecord {
		return fmt.Errorf("%w: a record already exists at %s", errRecordRevisionMismatch, termsafe.QuotePath(dest))
	}
	data, err := rfs.ReadFile(dest)
	if err != nil {
		return fmt.Errorf("re-read record %s: %w", termsafe.QuotePath(dest), termsafe.Error(err))
	}
	bc, err := decodeBreadcrumb(data)
	if err != nil {
		return fmt.Errorf("re-read record %s before writing: %w", termsafe.QuotePath(dest), err)
	}
	if expect == expectLegacyRecord {
		if bc.Version != 0 {
			return fmt.Errorf("%w: expected a legacy record at %s, found version %d",
				errRecordRevisionMismatch, termsafe.QuotePath(dest), bc.Version)
		}
		return nil
	}
	if bc.Version == 0 || bc.Revision != expect {
		return fmt.Errorf("%w: %s", errRecordRevisionMismatch, termsafe.QuotePath(dest))
	}
	return nil
}

func randomSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
