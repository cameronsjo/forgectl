//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
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

// PrunedItem is one item [Desk.Prune] would remove, as [PrunePlanAt] lists it.
type PrunedItem struct {
	// State is the directory the item is in: [DirDone] or [DirSkipped].
	State string
	// Name is the item's name.
	Name string
	// Newest is the modification time of its newest file.
	Newest time.Time
}

// pruneCandidate is one item Prune removes: its entries in sub.
type pruneCandidate struct {
	sub     string
	name    string
	newest  time.Time
	entries []fs.DirEntry
}

// pruneCandidates selects the items older than days in the open desk.
func (d *Desk) pruneCandidates(days int) ([]pruneCandidate, error) {
	return selectPrune(d.list, d.now(), days)
}

// selectPrune is the one place that decides what is old, shared by Prune
// and PrunePlanAt. It reads through list and never writes.
func selectPrune(list func(sub string) ([]fs.DirEntry, error), now time.Time, days int) ([]pruneCandidate, error) {
	if days < 1 {
		return nil, errors.New("desk: prune needs --days of at least 1")
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	running, err := list(DirRunning)
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
		entries, err := list(sub)
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

// Exists reports whether dir looks like a desk: it holds at least one of the
// protocol subdirectories as a real directory (a symlink does not count). It
// reads only. Open creates a missing desk, so a verb that must not create one
// asks this first.
func Exists(dir string) bool {
	for _, sub := range protocolDirs {
		if fi, err := os.Lstat(filepath.Join(dir, sub)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// beforeBind runs in PrunePlanAt between the root being checked and the root
// being opened by name; a test swaps the directory there to reach bindRoot's
// refusal, as add's beforeLink hook does for the link.
var beforeBind = func(string) {}

// PrunePlanAt returns what [Desk.Prune] would remove from the desk at dir,
// without opening it: no MkdirAll, no chmod, no migration, nothing written.
// (Open tightens modes and creates the protocol directories, so a preview
// cannot go through it.) It selects with the same code as Prune, and refuses
// what Open refuses, with the same checks made read-only: a desk root that is
// a symlink, not a directory, or owned by another user (privdir.OpenChecked),
// and a protocol directory that is a symlink, a file, or not readable
// (checkProtocolDir, the checks tightenDir makes). A protocol directory that
// is absent reads as empty.
//
// Like Open, it pins the root by descriptor, binds an os.Root opened by name
// to that descriptor (bindRoot), and lists through the root, so a root swapped
// for another directory after the check is refused rather than read.
//
// A protocol directory swapped for a symlink in the instant between its check
// and its listing is not caught. The listing goes through os.Root, so a
// swapped-in symlink can only redirect it to another directory inside the desk
// root: the preview may be wrong, and it never writes. This is not identical to
// Open: Open narrows the root to 0700 before it lists, and OpenChecked lets a
// too-broad root through on purpose (narrowing is a write), so in a dry run
// another local user with write access to that root can trigger the swap.
func PrunePlanAt(dir string, days int) ([]PrunedItem, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("desk: the desk directory must be an absolute path")
	}
	dir = filepath.Clean(dir)
	fd, err := privdir.OpenChecked(privdir.Spec{Base: filepath.Dir(dir), Leaf: filepath.Base(dir), Mode: dirMode})
	switch {
	case errors.Is(err, privdir.ErrAbsent):
		return []PrunedItem{}, nil
	case err != nil:
		return nil, fmt.Errorf("desk: open %s: %w", describe(dir), err)
	}
	defer unix.Close(fd) //nolint:errcheck // read-only descriptor
	beforeBind(dir)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("desk: open %s: %w", describe(dir), err)
	}
	defer root.Close() //nolint:errcheck // read-only
	if err := bindRoot(fd, root, dir); err != nil {
		return nil, err
	}
	for _, sub := range protocolDirs {
		if err := checkProtocolDir(fd, dir, sub); err != nil {
			return nil, err
		}
	}
	list := func(sub string) ([]fs.DirEntry, error) {
		entries, err := fs.ReadDir(root.FS(), sub)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("desk: read %s/: %w", sub, err)
		}
		return entries, nil
	}
	cands, err := selectPrune(list, time.Now(), days)
	if err != nil {
		return nil, err
	}
	plan := make([]PrunedItem, 0, len(cands))
	for _, c := range cands {
		plan = append(plan, PrunedItem{State: c.sub, Name: c.name, Newest: c.newest})
	}
	return plan, nil
}

// checkProtocolDir makes tightenDir's refusals on sub under the pinned desk
// descriptor fd, without its writes: the directory is opened no-follow, and a
// symlink or a file, or a directory its owner cannot read, is refused with
// tightenDir's message. An absent one passes (Open would create it). dir is
// only for the message.
func checkProtocolDir(fd int, dir, sub string) error {
	sfd, err := unix.Openat(fd, sub, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		return unix.Close(sfd)
	case errors.Is(err, unix.ENOENT):
		return nil
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EMLINK):
		return fmt.Errorf("desk: %s/ is not a directory; refusing", sub)
	case errors.Is(err, unix.EACCES):
		return fmt.Errorf("desk: %s/ is not readable by its owner; it needs mode 0700 (chmod 700 %s)", sub, describe(filepath.Join(dir, sub)))
	}
	return fmt.Errorf("desk: open %s/: %w", sub, err)
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
