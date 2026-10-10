package cli

import (
	"io"
	"strings"
)

// fakeTTY is a terminal stand-in shared by the intake and merge
// confirmation tests: answers are read from in, and what the confirmation
// writes lands in out. It has no build tag, so the merge tests vet on
// every platform.
type fakeTTY struct {
	in     io.Reader
	out    strings.Builder
	closed bool
}

func (f *fakeTTY) Read(p []byte) (int, error)  { return f.in.Read(p) }
func (f *fakeTTY) Write(p []byte) (int, error) { return f.out.Write(p) }
func (f *fakeTTY) Close() error                { f.closed = true; return nil }
