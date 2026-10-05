//go:build unix

package desk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
)

// The readers below serve the dashboard: they read through the pinned root,
// never by re-resolving the desk's name, and never change anything. What they
// return is raw script or log text; render it through termsafe.

// BatchStatus reads a batch's done/<name>.d/status.tsv. A batch that has not
// started yet has none: that is ErrNotFound.
func (d *Desk) BatchStatus(name string) ([]StepStatus, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	data, err := d.readCapped(path.Join(DirDone, name+extBatchDir, "status.tsv"), maxMetaBytes)
	if err != nil {
		return nil, err
	}
	return ParseStatusTSV(data), nil
}

// ReadSummary reads a finished batch's done/<name>.d/summary.json. A batch
// that has not finished has none: that is ErrNotFound.
func (d *Desk) ReadSummary(name string) (*Summary, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	data, err := d.readCapped(path.Join(DirDone, name+extBatchDir, "summary.json"), maxMetaBytes)
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("desk: read summary for %s: %w", describe(name), err)
	}
	return &s, nil
}

// Record returns the bytes of an item that is no longer pending: running/ is
// looked at first, then done/, then skipped/. For a running item this is the
// record of what runs, which is a copy: see [Claimed].
func (d *Desk) Record(name string) ([]byte, Kind, error) {
	for _, sub := range [...]string{DirRunning, DirDone, DirSkipped} {
		kind, err := d.findKind(sub, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		data, err := d.readCapped(path.Join(sub, name+kind.Ext()), maxItemBytes)
		return data, kind, err
	}
	return nil, "", ErrNotFound
}

// LogTail returns at most the last maxBytes of done/<name>.log, and whether
// anything before them was left out.
func (d *Desk) LogTail(name string, maxBytes int64) (data []byte, cut bool, err error) {
	if !ValidName(name) {
		return nil, false, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	f, err := d.openRegular(path.Join(DirDone, name+extLog))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	off := max(fi.Size()-maxBytes, 0)
	data, err = io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	return data, off > 0, err
}

// readCapped is readRegular with a missing file reported as ErrNotFound.
func (d *Desk) readCapped(p string, limit int64) ([]byte, error) {
	data, err := d.readRegular(p, limit)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}
