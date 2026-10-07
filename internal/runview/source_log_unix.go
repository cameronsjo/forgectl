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
//
// With a lens (NewLensSource) the same file is read through the lens: its
// lines may be plain text, its rules give it a step model, and its runs fold
// with the lens's spec.
type logSource struct {
	path string
	keys LogKeys
	lens *Lens
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

// NewLensSource returns the source for the log at path, read through lens.
// path must be absolute.
func NewLensSource(path string, lens *Lens) (Source, error) {
	switch {
	case !filepath.IsAbs(path):
		return nil, fmt.Errorf("log %s: must be an absolute path", clean(path))
	case lens == nil:
		return nil, errors.New("log: no lens")
	}
	return &logSource{path: filepath.Clean(path), lens: lens}, nil
}

func (s *logSource) Name() string { return "log" }

// Spec is the fold a run of this source takes: the lens's, or none.
func (s *logSource) Spec(RunRef) *Spec {
	if s.lens != nil {
		return s.lens.Spec()
	}
	return &Spec{}
}

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
	if s.lens != nil {
		d.Defs = s.lens.Steps()
		if s.lens.Ends() {
			d.Live = "" // the fold knows when the run ends
		}
	}
	if replaced(&st, cur) {
		*cur = Cursor{}
		d.Reset = true
	}
	capped, err := readLines(f, &st, cur, func(_ int, b []byte) {
		if cur.kept >= maxRunEvents {
			cur.dropped++
			return
		}
		if strings.TrimSpace(string(b)) == "" {
			return
		}
		if s.lens != nil {
			e, nf, res, rule, split := s.lens.read(cur.kept+1, b)
			cur.lensLines++
			if split {
				cur.split++
			}
			if rule > 0 {
				if cur.ruleHits == nil {
					cur.ruleHits = make([]int, s.lens.Rules())
				}
				cur.ruleHits[rule-1]++
			}
			switch res {
			case lineDropped:
				cur.dropped++
			case lineIgnored:
				cur.ignored++
			default:
				cur.fields += nf // as for a plain log: fields of a kept event
				cur.kept++
				d.Events = append(d.Events, e)
			}
			return
		}
		raw, nf, ok := scalarFields(b)
		// Seq numbers events, not file lines, so #N and --at N agree.
		e, named := logEvent(s.keys, cur.kept+1, raw)
		if !ok || !named {
			cur.dropped++
			return
		}
		cur.fields += nf
		cur.kept++
		d.Events = append(d.Events, e)
	})
	d.Partial = capped
	d.Held = !capped && (len(cur.held) > 0 || cur.dropping) // past the cap, a cut line is Partial
	if err != nil {
		d.Err = cleanErr(fmt.Errorf("cannot read %s: %w", clean(filepath.Base(s.path)), err))
	}
	d.Dropped, d.DroppedFields, d.Ignored = cur.dropped, cur.fields, cur.ignored
	if s.lens != nil {
		d.RuleHits = make([]int, s.lens.Rules())
		copy(d.RuleHits, cur.ruleHits)
		d.Lines, d.Split = cur.lensLines, cur.split
	}
	return d, nil
}
