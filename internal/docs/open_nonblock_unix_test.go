//go:build unix

package docs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO must never hang Open: one the walk finds is refused before any
// open, and one swapped in over an approved regular file is opened
// nonblocking and refused by the regular-file check. Each call runs under a
// deadline so a regression fails instead of hanging the suite.
func TestOpenVerified_FIFONeverBlocks(t *testing.T) {
	root := mustCanonicalRoot(t, t.TempDir())
	fifo := filepath.Join(root, "pipe.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	doc := filepath.Join(root, "doc.md")
	if err := os.WriteFile(doc, []byte("# d"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	fifoInfo, err := r.Lstat("pipe.md")
	if err != nil {
		t.Fatal(err)
	}
	docInfo, err := r.Lstat("doc.md")
	if err != nil {
		t.Fatal(err)
	}
	// Whatever happens, release a blocked open so the goroutine can end.
	t.Cleanup(func() {
		if w, err := os.OpenFile(filepath.Clean(fifo), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})

	within := func(name, base string, want os.FileInfo) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			f, err := openVerified(r, base, want)
			if f != nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrOutsideRoot) {
				t.Errorf("%s: err = %v, want ErrOutsideRoot", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: openVerified blocked on a FIFO", name)
		}
	}
	within("FIFO found by the walk", "pipe.md", fifoInfo)

	if err := os.Rename(fifo, doc); err != nil {
		t.Fatal(err)
	}
	fifo = doc
	within("FIFO swapped over an approved doc", "doc.md", docInfo)
}

// forgectl#743: a FIFO named like a doc hung NewIndex (and so a live-reload
// Rebuild) forever, in a directory root and as a single-file root. The walk
// now skips it and the single-file root refuses it. Each build runs under a
// deadline so a regression fails instead of hanging the suite; the cleanup
// opens the FIFO for writing to release a blocked reader.
func TestNewIndex_FIFONamedLikeADocNeverBlocks(t *testing.T) {
	dir := mustCanonicalRoot(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(filepath.Clean(fifo), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})

	build := func(name string, paths []string) (*Index, error) {
		t.Helper()
		type result struct {
			idx *Index
			err error
		}
		done := make(chan result, 1)
		go func() {
			idx, err := NewIndex(paths)
			done <- result{idx, err}
		}()
		select {
		case r := <-done:
			return r.idx, r.err
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: NewIndex blocked on a FIFO", name)
			return nil, nil
		}
	}

	idx, err := build("directory root", []string{dir})
	if err != nil {
		t.Fatalf("directory root: %v", err)
	}
	if docs := idx.List(); len(docs) != 1 || docs[0].RelPath != "a.md" {
		t.Errorf("directory root indexed %+v, want only a.md", docs)
	}
	if _, err := build("single-file root", []string{fifo}); err == nil {
		t.Error("a FIFO named as a single-file root was indexed, want an error")
	}
}
