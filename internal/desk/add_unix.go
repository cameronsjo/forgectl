//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// beforeLink runs between the name check and the link; tests plant a file
// there to make the link collide.
var beforeLink = func(string) {}

// maxNumberTries bounds the NN- search when other writers keep taking numbers.
const maxNumberTries = 1000

// Added describes an enqueued item.
type Added struct {
	Name   string
	Kind   Kind
	SHA256 string
	Path   string
	// Warnings are a batch manifest's planner warnings (see Manifest.Lint),
	// which do not stop it from being queued. nil for a script.
	Warnings []string
}

// Add enqueues the file at src. The kind comes from its extension (.sh or
// .manifest), the name from its basename. what and why become "# WHAT:" and
// "# WHY:" lines (each may be empty if the file already carries the line); tty
// adds "# TTY: yes". The header lines are inserted before the hash is taken,
// and a manifest is validated before it is queued.
//
// The next NN- is taken exclusively: the finished bytes are written to a
// dot-named temp file and hard-linked to pending/NN-name, which fails rather
// than overwrite when a hand-dropped file holds that name, and then the next
// number is tried. A desk that scans between the link and the temp file's
// removal sees link count 2 and refuses the item until the next scan, so it
// never hashes a partial file.
func (d *Desk) Add(src, what, why string, tty bool) (Added, error) {
	a, _, err := d.add(src, what, why, tty, false)
	return a, err
}

// AddUnique is [Desk.Add], except that an item already waiting in pending/ with
// the same kind and the same sha256 (the hash taken after the header lines are
// inserted, so the same file with the same what and why) is not queued again:
// it returns that item, with duplicate true, and writes nothing. A retry after
// a timeout therefore finds the first attempt instead of queueing a second
// approval. The check and the queueing are not one atomic step: two adds of
// the same file at the same instant can both queue.
func (d *Desk) AddUnique(src, what, why string, tty bool) (a Added, duplicate bool, err error) {
	return d.add(src, what, why, tty, true)
}

// Signalled reports whether the operator signal for the pending item name is
// recorded as sent (see [Meta.SignalledAt]). An item with no meta, such as a
// hand-dropped file, reads as signalled: nothing here ever signalled it, and a
// retry must not start pinging for a file a person put there.
func (d *Desk) Signalled(name string) bool {
	meta, ok, err := d.readMeta(DirPending, name)
	return err != nil || !ok || meta.SignalledAt != nil
}

// MarkSignalled records that the operator signal for the pending item name
// was sent. An item with no meta is left alone, so a legacy item is never
// given a partial one.
func (d *Desk) MarkSignalled(name string) error {
	meta, ok, err := d.readMeta(DirPending, name)
	if err != nil || !ok {
		return err
	}
	now := d.now().UTC()
	meta.SignalledAt = &now
	return d.writeMeta(DirPending, name, meta)
}

// waitingWith returns the pending item of kind whose bytes hash to sum. An
// entry that cannot be read (mid-link, refused, unreadable) does not match.
func (d *Desk) waitingWith(kind Kind, sum string) (string, bool, error) {
	entries, err := d.list(DirPending)
	if err != nil {
		return "", false, err
	}
	for _, e := range entries {
		name, k, ok := kindOfFile(e.Name())
		if !ok || k != kind {
			continue
		}
		data, _, err := d.readItem(DirPending, e.Name())
		if err != nil {
			continue
		}
		if SHA256Hex(data) == sum {
			return name, true, nil
		}
	}
	return "", false, nil
}

// add is Add and AddUnique: with unique set, an identical waiting item is
// returned instead of a new one being queued.
func (d *Desk) add(src, what, why string, tty, unique bool) (Added, bool, error) {
	base := filepath.Base(src)
	var kind Kind
	switch {
	case strings.HasSuffix(base, extScript):
		kind = KindScript
	case strings.HasSuffix(base, extManifest):
		kind = KindBatch
	default:
		return Added{}, false, errors.New("desk: the file must end in .sh (a script) or .manifest (a batch)")
	}
	stem := strings.TrimSuffix(base, kind.Ext())
	if !stemRe.MatchString(stem) || strings.Contains(stem, "..") {
		return Added{}, false, fmt.Errorf("desk: %q is not a usable item name (letters, digits, '.', '_', '-'; at most 64)", describe(stem))
	}
	data, err := ReadSource(src)
	if err != nil {
		return Added{}, false, err
	}
	body, err := insertHeaders(data, kind, what, why, tty)
	if err != nil {
		return Added{}, false, err
	}
	var warnings []string
	if kind == KindBatch {
		m, err := LoadManifest(body, base)
		if err != nil {
			return Added{}, false, err
		}
		warnings = m.Lint()
	}
	sum := SHA256Hex(body)
	if unique {
		name, found, err := d.waitingWith(kind, sum)
		if err != nil {
			return Added{}, false, err
		}
		if found {
			return Added{Name: name, Kind: kind, SHA256: sum, Path: d.abs(path.Join(DirPending, name+kind.Ext())), Warnings: warnings}, true, nil
		}
	}
	tmp, err := d.writeTemp(DirPending, "add", body)
	if err != nil {
		return Added{}, false, err
	}
	defer d.root.Remove(tmp) //nolint:errcheck // best effort; a dot-named leftover is ignored by scans

	n, err := d.nextNumber()
	if err != nil {
		return Added{}, false, err
	}
	for range maxNumberTries {
		name := fmt.Sprintf("%02d-%s", n, stem)
		n++
		if d.nameTaken(name) {
			continue
		}
		beforeLink(name + kind.Ext())
		err := d.root.Link(tmp, path.Join(DirPending, name+kind.Ext()))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return Added{}, false, fmt.Errorf("desk: queue %s: %w", name, err)
		}
		now := d.now().UTC()
		if err := d.writeMeta(DirPending, name, Meta{AddedAt: &now, SHA256: sum, Kind: kind, SignalPane: d.signalPane}); err != nil {
			return Added{}, false, err
		}
		return Added{Name: name, Kind: kind, SHA256: sum, Path: d.abs(path.Join(DirPending, name+kind.Ext())), Warnings: warnings}, false, nil
	}
	return Added{}, false, errors.New("desk: no free item number; is something else writing pending/?")
}

// ReadSource reads the file Claude asked to enqueue, capped like an item.
//
// It refuses anything but a regular file, after opening it O_NONBLOCK so a
// FIFO cannot hang the caller. A symlink is followed: the bytes are copied
// and hashed, so where they came from does not matter.
func ReadSource(src string) ([]byte, error) {
	f, err := os.OpenFile(src, os.O_RDONLY|unix.O_NONBLOCK, 0) //nolint:gosec // G304: reading the file the caller named is the operation
	if err != nil {
		return nil, fmt.Errorf("desk: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	if fi, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("desk: stat %s: %w", describe(src), err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrRefused, describe(src))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxItemBytes+1))
	if err != nil {
		return nil, fmt.Errorf("desk: read %s: %w", describe(src), err)
	}
	if len(data) > maxItemBytes {
		return nil, errors.New("desk: the file is larger than 1 MiB")
	}
	return data, nil
}

// nextNumber is one more than the highest NN- in any protocol dir.
func (d *Desk) nextNumber() (int, error) {
	top := 0
	for _, sub := range protocolDirs {
		entries, err := d.list(sub)
		if err != nil {
			return 0, err
		}
		for _, e := range entries {
			if n, _ := SplitName(e.Name()); n > top && !strings.HasPrefix(e.Name(), ".") {
				top = n
			}
		}
	}
	return top + 1, nil
}

// nameTaken reports whether name is used by any item, of either kind, in any
// protocol dir.
func (d *Desk) nameTaken(name string) bool {
	for _, sub := range protocolDirs {
		for _, ext := range [...]string{extScript, extManifest, extMeta, extLog, extEvents, extBatchDir} {
			if d.exists(path.Join(sub, name+ext)) {
				return true
			}
		}
	}
	return false
}
