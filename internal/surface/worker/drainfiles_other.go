//go:build !unix

package worker

import "github.com/cameronsjo/forgectl/internal/privdir"

// The drain's files rest on openat, O_NOFOLLOW and flock, which this
// platform does not offer, so every call refuses.

// DrainLock is never held on this platform.
type DrainLock struct{}

// Close does nothing.
func (*DrainLock) Close() error { return nil }

// Lock refuses.
func (DrainFiles) Lock() (*DrainLock, error) { return nil, privdir.ErrUnsupported }

// ReadStatus refuses.
func (DrainFiles) ReadStatus() ([]byte, error) { return nil, privdir.ErrUnsupported }

// WriteStatus refuses.
func (DrainFiles) WriteStatus(*DrainLock, []byte) error { return privdir.ErrUnsupported }

// AppendEvent refuses.
func (DrainFiles) AppendEvent(*DrainLock, []byte) error { return privdir.ErrUnsupported }

// ReadEvents refuses.
func (DrainFiles) ReadEvents() ([]byte, []byte, error) { return nil, nil, privdir.ErrUnsupported }
