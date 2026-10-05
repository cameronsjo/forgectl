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
	if days < 1 {
		return 0, errors.New("desk: prune needs --days of at least 1")
	}
	cutoff := d.now().Add(-time.Duration(days) * 24 * time.Hour)
	running, err := d.list(DirRunning)
	if err != nil {
		return 0, err
	}
	live := map[string]bool{}
	for _, e := range running {
		if name, _, ok := kindOfFile(e.Name()); ok {
			live[name] = true
		}
	}
	removed := 0
	for _, sub := range [...]string{DirDone, DirSkipped} {
		entries, err := d.list(sub)
		if err != nil {
			return removed, err
		}
		groups := map[string][]fs.DirEntry{}
		var order []string
		for _, e := range entries {
			name, ok := protocolEntry(e)
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
			for _, e := range groups[name] {
				p := path.Join(sub, e.Name())
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
	}
	return removed, nil
}

// protocolEntry returns the item name an entry belongs to, or ok=false for
// anything that is not a protocol entry.
func protocolEntry(e fs.DirEntry) (string, bool) {
	t := e.Type()
	if strings.HasPrefix(e.Name(), ".") || t&fs.ModeSymlink != 0 {
		return "", false
	}
	for _, suffix := range protocolSuffixes {
		name, found := strings.CutSuffix(e.Name(), suffix)
		if !found || !ValidName(name) {
			continue
		}
		if (suffix == extBatchDir) != t.IsDir() || (!t.IsDir() && !t.IsRegular()) {
			return "", false
		}
		return name, true
	}
	return "", false
}
