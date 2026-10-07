//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

// protocolSuffixes are the entry suffixes Prune may remove, longest first so
// ".meta.json" is matched before a shorter suffix could be.
var protocolSuffixes = [...]string{extMeta, extManifest, extEvents, extScript, extLog, extBatchDir}

// Prune deletes items in done/ and skipped/ whose newest entry is older than
// days, and returns how many items it removed. Only protocol entries are
// removed (an item's .sh, .manifest, .log, .events, .meta.json and .d/), never
// symlinks, dot files, or anything else; an item still in running/ keeps its
// done/ files. Nothing in pending/ or running/ is touched.
func (d *Desk) Prune(days int) (int, error) {
	cands, err := d.pruneCandidates(days)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, c := range cands {
		for _, e := range c.entries {
			p := path.Join(c.sub, e.Name())
			var err error
			if e.IsDir() {
				err = d.root.RemoveAll(p)
			} else {
				err = d.root.Remove(p)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, fmt.Errorf("desk: prune %s: %w", describe(p), err)
			}
		}
		removed++
	}
	return removed, nil
}

// PrunedItem is one item [Desk.Prune] would remove.
type PrunedItem struct {
	// State is the directory the item is in: [DirDone] or [DirSkipped].
	State string
	// Name is the item's name.
	Name string
	// Newest is the modification time of its newest file.
	Newest time.Time
}

// PrunePlan returns the items [Desk.Prune] would remove for days, in the
// order it removes them, and removes nothing. It runs the same selection as
// Prune, so the preview and the deletion cannot disagree about what is old.
func (d *Desk) PrunePlan(days int) ([]PrunedItem, error) {
	cands, err := d.pruneCandidates(days)
	if err != nil {
		return nil, err
	}
	plan := make([]PrunedItem, 0, len(cands))
	for _, c := range cands {
		plan = append(plan, PrunedItem{State: c.sub, Name: c.name, Newest: c.newest})
	}
	return plan, nil
}

// pruneCandidate is one item Prune removes: its entries in sub.
type pruneCandidate struct {
	sub     string
	name    string
	newest  time.Time
	entries []fs.DirEntry
}

// pruneCandidates selects the items older than days: the one place that
// decides what is old, shared by Prune and PrunePlan. It reads, never writes.
func (d *Desk) pruneCandidates(days int) ([]pruneCandidate, error) {
	if days < 1 {
		return nil, errors.New("desk: prune needs --days of at least 1")
	}
	cutoff := d.now().Add(-time.Duration(days) * 24 * time.Hour)
	running, err := d.list(DirRunning)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, e := range running {
		if name, _, ok := kindOfFile(e.Name()); ok {
			live[name] = true
		}
	}
	var out []pruneCandidate
	for _, sub := range [...]string{DirDone, DirSkipped} {
		entries, err := d.list(sub)
		if err != nil {
			return nil, err
		}
		groups := map[string][]fs.DirEntry{}
		var order []string
		for _, e := range entries {
			name, ok := protocolEntry(e, sub == DirDone)
			if !ok || live[name] {
				continue
			}
			if _, seen := groups[name]; !seen {
				order = append(order, name)
			}
			groups[name] = append(groups[name], e)
		}
		for _, name := range order {
			newest := time.Time{}
			for _, e := range groups[name] {
				if fi, err := e.Info(); err == nil && fi.ModTime().After(newest) {
					newest = fi.ModTime()
				}
			}
			if !newest.Before(cutoff) {
				continue
			}
			out = append(out, pruneCandidate{sub: sub, name: name, newest: newest, entries: groups[name]})
		}
	}
	return out, nil
}

// protocolEntry returns the item name an entry belongs to, or ok=false for
// anything that is not a protocol entry. In done/ (legacy true) an old log's
// display-only name counts too, so prune clears what history shows.
func protocolEntry(e fs.DirEntry, legacy bool) (string, bool) {
	t := e.Type()
	if strings.HasPrefix(e.Name(), ".") || t&fs.ModeSymlink != 0 {
		return "", false
	}
	for _, suffix := range protocolSuffixes {
		name, found := strings.CutSuffix(e.Name(), suffix)
		if !found || (!ValidName(name) && (!legacy || !legacyName(name))) {
			continue
		}
		if (suffix == extBatchDir) != t.IsDir() || (!t.IsDir() && !t.IsRegular()) {
			return "", false
		}
		return name, true
	}
	return "", false
}
