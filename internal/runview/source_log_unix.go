// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// logSource reads one JSONL file a person named: each line one JSON object,
// one event. It has no step model, so its runs show their events and no step
// state, and their Live is LiveUnknown. It holds no per-run state: the Cursor
// carries it.
type logSource struct {
	path string
	keys LogKeys
}

// NewLogSource returns the source for the JSONL file at path, read with keys.
// path must be absolute; keys.Event must be set.
func NewLogSource(path string, keys LogKeys) (Source, error) {
	switch {
	case !filepath.IsAbs(path):
		return nil, fmt.Errorf("log %s: must be an absolute path", clean(path))
	case keys.Event == "":
		return nil, errors.New("log: the event key must not be empty")
	}
	return &logSource{path: filepath.Clean(path), keys: keys}, nil
}

func (s *logSource) Name() string { return "log" }

// List returns the one run: the file, named by its base name.
func (s *logSource) List() ([]RunRef, error) {
	f, st, err := openLog(s.path)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	return []RunRef{{Source: s.Name(), Name: clean(filepath.Base(s.path)), Kind: KindLog, Updated: mtimeOf(&st)}}, nil
}

// Load reads what the log gained since cur. When the file was replaced or
// shrank, it is read again from line 1 and Reset is set.
func (s *logSource) Load(_ RunRef, cur *Cursor) (Delta, error) {
	if cur == nil {
		cur = &Cursor{}
	}
	f, st, err := openLog(s.path)
	if err != nil {
		return Delta{}, err
	}
	defer f.Close() //nolint:errcheck // read-only

	d := Delta{Live: LiveUnknown}
	if replaced(&st, cur) {
		*cur = Cursor{}
		d.Reset = true
	}
	capped, err := readLines(f, &st, cur, func(seq int, b []byte) {
		if cur.kept >= maxRunEvents {
			cur.dropped++
			return
		}
		if strings.TrimSpace(string(b)) == "" {
			return
		}
		raw, nf, ok := scalarFields(b)
		e, named := logEvent(s.keys, seq, raw)
		if !ok || !named {
			cur.dropped++
			return
		}
		cur.fields += nf
		cur.kept++
		d.Events = append(d.Events, e)
	})
	d.Partial = capped
	d.Held = len(cur.held) > 0 || cur.dropping
	if err != nil {
		d.Err = cleanErr(fmt.Errorf("cannot read %s: %w", clean(filepath.Base(s.path)), err))
	}
	d.Dropped, d.DroppedFields = cur.dropped, cur.fields
	return d, nil
}
