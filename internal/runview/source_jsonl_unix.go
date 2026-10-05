// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// runNameRe is what a run or summary name (the text a pattern's '*'
// matched) must look like. Anything else is skipped, never opened.
var runNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// maxDirEntries bounds how many entries of the directory holding a
// pattern's '*' are read per List.
const maxDirEntries = 100000

// pattern is a spec pattern split around its one '*': the fixed directories
// before the starred component, that component's text on either side of the
// '*', and the fixed components after it.
type pattern struct {
	text           string
	dirs           []string
	prefix, suffix string
	rest           []string
}

func parsePattern(p string) (pattern, error) {
	parts, err := split(p)
	if err != nil {
		return pattern{}, err
	}
	for i, c := range parts {
		if before, after, found := strings.Cut(c, "*"); found {
			return pattern{text: p, dirs: parts[:i], prefix: before, suffix: after, rest: parts[i+1:]}, nil
		}
	}
	return pattern{}, fmt.Errorf("%s: no '*'", clean(p))
}

// path is the pattern with name in place of the '*'.
func (p pattern) path(name string) string {
	parts := append(slices.Clone(p.dirs), p.prefix+name+p.suffix)
	return path.Join(append(parts, p.rest...)...)
}

// match is one name a pattern matched and the newest mtime seen for it.
type match struct {
	name    string
	updated time.Time
}

// list returns the names the pattern matches below dirfd, newest first, at
// most maxMatches. A name must match runNameRe, and the whole matched path
// must be a real directory (wantDir) or a regular file, with no symlink in
// any component. A missing directory before the '*' is no matches.
func (p pattern) list(dirfd int, wantDir bool) ([]match, error) {
	base := path.Join(p.dirs...)
	fd, err := openDir(dirfd, p.dirs, base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), base) //nolint:gosec // G115: fd >= 0 here
	defer dir.Close()                    //nolint:errcheck // read-only
	names, err := dir.Readdirnames(maxDirEntries)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, pathError(base, err)
	}

	// The starred component is checked first with one fstatat, which does
	// not follow a symlink, so the newest candidates can be chosen without
	// walking every one.
	var cands []match
	for _, n := range names {
		name, ok := strings.CutPrefix(n, p.prefix)
		if ok {
			name, ok = strings.CutSuffix(name, p.suffix)
		}
		if !ok || !runNameRe.MatchString(name) {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		kind := st.Mode & unix.S_IFMT
		if kind == unix.S_IFLNK || (len(p.rest) > 0 || wantDir) != (kind == unix.S_IFDIR) {
			continue
		}
		cands = append(cands, match{name: name, updated: mtimeOf(&st)})
	}
	slices.SortFunc(cands, newestFirst)

	var out []match
	for _, c := range cands {
		if len(out) == maxMatches {
			break
		}
		rel := p.path(c.name)
		if wantDir {
			if _, err := statDir(dirfd, rel); err != nil {
				continue
			}
		} else {
			f, st, err := openFile(dirfd, rel)
			if err != nil {
				continue
			}
			_ = f.Close()
			c.updated = mtimeOf(&st)
		}
		out = append(out, c)
	}
	return out, nil
}

func newestFirst(a, b match) int {
	if c := b.updated.Compare(a.updated); c != 0 {
		return c
	}
	return strings.Compare(a.name, b.name)
}

// specSource reads the runs a spec describes. It holds no per-run state:
// the Cursor carries it.
type specSource struct {
	spec      *Spec
	runs      pattern
	summaries *pattern
}

// NewSpecSource returns the source for spec's runs and summaries. The root
// comes from the variable spec.RootEnv names; it must be set and absolute,
// and is opened afresh by every List and Load.
func NewSpecSource(spec *Spec) (Source, error) {
	runs, err := parsePattern(spec.Runs)
	if err != nil {
		return nil, fmt.Errorf("%w: runs: %w", ErrSpec, err)
	}
	s := &specSource{spec: spec, runs: runs}
	if spec.Summaries != "" {
		sum, err := parsePattern(spec.Summaries)
		if err != nil {
			return nil, fmt.Errorf("%w: summaries: %w", ErrSpec, err)
		}
		s.summaries = &sum
	}
	for _, rel := range []string{spec.Log.Path, spec.Metrics.Path} {
		if _, err := split(rel); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpec, err)
		}
	}
	r, err := openRunRoot(spec.RootEnv)
	if err != nil {
		return nil, err
	}
	_ = r.Close()
	return s, nil
}

func (s *specSource) Name() string { return clean(s.spec.Name) }

// List returns every run dir and every summary with no run dir, newest
// first. A run dir with a summary of the same name has HasSummary set.
func (s *specSource) List() ([]RunRef, error) {
	r, err := openRunRoot(s.spec.RootEnv)
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck // read-only
	runs, err := s.runs.list(r.fd, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clean(s.runs.text), err)
	}
	var sums []match
	if s.summaries != nil {
		if sums, err = s.summaries.list(r.fd, false); err != nil {
			return nil, fmt.Errorf("%s: %w", clean(s.summaries.text), err)
		}
	}
	src := s.Name()
	refs := make([]RunRef, 0, len(runs)+len(sums))
	byName := map[string]int{}
	for _, m := range runs {
		updated := m.updated
		if f, st, err := openFile(r.fd, path.Join(s.runs.path(m.name), s.spec.Log.Path)); err == nil {
			_ = f.Close()
			updated = mtimeOf(&st)
		}
		byName[m.name] = len(refs)
		refs = append(refs, RunRef{Source: src, Name: m.name, Kind: KindLog, Updated: updated})
	}
	for _, m := range sums {
		if i, ok := byName[m.name]; ok {
			refs[i].HasSummary = true
			continue
		}
		refs = append(refs, RunRef{Source: src, Name: m.name, Kind: KindSummary, Updated: m.updated})
	}
	slices.SortFunc(refs, func(a, b RunRef) int {
		return newestFirst(match{a.Name, a.Updated}, match{b.Name, b.Updated})
	})
	return refs, nil
}

// Load reads what the run gained since cur. When the events log or the
// metrics file was replaced or shrank, both start over from line 1 and
// Reset is set.
func (s *specSource) Load(ref RunRef, cur *Cursor) (Delta, error) {
	if !runNameRe.MatchString(ref.Name) {
		return Delta{}, fmt.Errorf("%w: run name %s", ErrRefused, clean(ref.Name))
	}
	if cur == nil {
		cur = &Cursor{}
	}
	r, err := openRunRoot(s.spec.RootEnv)
	if err != nil {
		return Delta{}, err
	}
	defer r.Close() //nolint:errcheck // read-only

	var d Delta
	if ref.Kind != KindSummary {
		s.loadLog(r.fd, ref.Name, cur, &d)
	}
	if s.summaries != nil && (ref.HasSummary || ref.Kind == KindSummary) {
		loadSummary(r.fd, s.summaries.path(ref.Name), cur, &d)
	}
	return d, nil
}

func (s *specSource) loadLog(dirfd int, name string, cur *Cursor, d *Delta) {
	runDir := s.runs.path(name)
	if cur.Metrics == nil {
		cur.Metrics = &Cursor{}
	}
	logRel := path.Join(runDir, s.spec.Log.Path)
	ev := track(dirfd, logRel, cur)
	defer ev.close()
	mt := tracked{err: fs.ErrNotExist}
	if s.spec.Metrics.Path != "" {
		mt = track(dirfd, path.Join(runDir, s.spec.Metrics.Path), cur.Metrics)
		defer mt.close()
	}
	if ev.replaced || mt.replaced {
		sum := cur.Summary
		*cur = Cursor{Metrics: &Cursor{}, Summary: sum}
		d.Reset = true
	}

	if ev.err != nil {
		d.Err = fmt.Errorf("cannot read %s: %w", clean(s.spec.Log.Path), ev.err)
	} else {
		capped, err := readLines(ev, cur, func(seq int, b []byte) {
			if cur.kept >= maxRunEvents {
				cur.dropped++
				return
			}
			if len(strings.TrimSpace(string(b))) == 0 {
				return
			}
			raw, nf, ok := scalarFields(b)
			e, named := logEvent(s.spec, seq, raw)
			if !ok || !named {
				cur.dropped++
				return
			}
			cur.fields += nf
			cur.kept++
			d.Events = append(d.Events, e)
		})
		d.Partial = d.Partial || capped
		if err != nil {
			d.Err = fmt.Errorf("cannot read %s: %w", clean(s.spec.Log.Path), err)
		}
	}

	if mt.f != nil {
		m := cur.Metrics
		capped, err := readLines(mt, m, func(seq int, b []byte) {
			if m.kept >= maxRunEvents {
				m.dropped++
				return
			}
			if len(strings.TrimSpace(string(b))) == 0 {
				return
			}
			raw, nf, ok := scalarFields(b)
			if !ok {
				m.dropped++
				return
			}
			m.fields += nf
			m.kept++
			d.Metrics = append(d.Metrics, metricLine(s.spec, seq, raw))
		})
		d.Partial = d.Partial || capped
		if err != nil && d.Err == nil {
			d.Err = fmt.Errorf("cannot read %s: %w", clean(s.spec.Metrics.Path), err)
		}
	} else if s.spec.Metrics.Path != "" && !errors.Is(mt.err, fs.ErrNotExist) && d.Err == nil {
		d.Err = fmt.Errorf("cannot read %s: %w", clean(s.spec.Metrics.Path), mt.err)
	}
	d.Dropped = cur.dropped + cur.Metrics.dropped
	d.DroppedFields = cur.fields + cur.Metrics.fields
}
