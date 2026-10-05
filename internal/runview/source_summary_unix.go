// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"fmt"
	"io"
)

// loadSummary sets d.Summary to the flattened summary at rel when the file
// changed since cur.Summary last read it (a new device, inode, size or
// mtime), and leaves it nil otherwise. A summary over maxSummaryBytes is not
// read: the run is Partial and Err says why.
func loadSummary(dirfd int, rel string, cur *Cursor, d *Delta) {
	if cur.Summary == nil {
		cur.Summary = &Cursor{}
	}
	sc := cur.Summary
	defer func() { d.DroppedFields += sc.fields }()

	f, st, err := openFile(dirfd, rel)
	if err != nil {
		if d.Err == nil {
			d.Err = fmt.Errorf("cannot read summary: %w", err)
		}
		return
	}
	defer f.Close() //nolint:errcheck // read-only
	if st.Size > maxSummaryBytes {
		d.Partial = true
		if d.Err == nil {
			d.Err = fmt.Errorf("summary is over the %d MiB cap", maxSummaryBytes>>20)
		}
		return
	}
	dev, ino := devIno(&st)
	mtime := mtimeOf(&st)
	if sc.opened && dev == sc.Dev && ino == sc.Ino && st.Size == sc.size && mtime.Equal(sc.mtime) {
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSummaryBytes+1))
	if err == nil && len(data) > maxSummaryBytes {
		d.Partial = true
		err = fmt.Errorf("over the %d MiB cap", maxSummaryBytes>>20)
	}
	if err != nil {
		if d.Err == nil {
			d.Err = fmt.Errorf("cannot read summary: %w", err)
		}
		return
	}
	sc.Dev, sc.Ino, sc.size, sc.mtime, sc.opened = dev, ino, st.Size, mtime, true
	kvs, dropped, err := flattenSummary(data)
	if err != nil {
		sc.fields = 0
		d.Summary = []KV{}
		if d.Err == nil {
			d.Err = fmt.Errorf("summary is not valid JSON: %s", clean(err.Error()))
		}
		return
	}
	sc.fields = dropped
	d.Summary = kvs
}
