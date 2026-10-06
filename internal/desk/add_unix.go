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
	base := filepath.Base(src)
	var kind Kind
	switch {
	case strings.HasSuffix(base, extScript):
		kind = KindScript
	case strings.HasSuffix(base, extManifest):
		kind = KindBatch
	default:
		return Added{}, errors.New("desk: the file must end in .sh (a script) or .manifest (a batch)")
	}
	stem := strings.TrimSuffix(base, kind.Ext())
	if !stemRe.MatchString(stem) || strings.Contains(stem, "..") {
		return Added{}, fmt.Errorf("desk: %q is not a usable item name (letters, digits, '.', '_', '-'; at most 64)", describe(stem))
	}
	data, err := ReadSource(src)
	if err != nil {
		return Added{}, err
	}
	body, err := insertHeaders(data, kind, what, why, tty)
	if err != nil {
		return Added{}, err
	}
	var warnings []string
	if kind == KindBatch {
		m, err := LoadManifest(body, base)
		if err != nil {
			return Added{}, err
		}
		warnings = m.Lint()
	}
	sum := SHA256Hex(body)
	tmp, err := d.writeTemp(DirPending, "add", body)
	if err != nil {
		return Added{}, err
	}
	defer d.root.Remove(tmp) //nolint:errcheck // best effort; a dot-named leftover is ignored by scans

	n, err := d.nextNumber()
	if err != nil {
		return Added{}, err
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
			return Added{}, fmt.Errorf("desk: queue %s: %w", name, err)
		}
		now := d.now().UTC()
		if err := d.writeMeta(DirPending, name, Meta{AddedAt: &now, SHA256: sum, Kind: kind, SignalPane: d.signalPane}); err != nil {
			return Added{}, err
		}
		return Added{Name: name, Kind: kind, SHA256: sum, Path: d.abs(path.Join(DirPending, name+kind.Ext())), Warnings: warnings}, nil
	}
	return Added{}, errors.New("desk: no free item number; is something else writing pending/?")
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
