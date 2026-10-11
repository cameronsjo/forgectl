//go:build !unix

package worker

import "github.com/cameronsjo/forgectl/internal/privdir"

// The audit file rests on openat, O_NOFOLLOW and flock, which this
// platform does not offer, so every call refuses.

// Read refuses.
func (MergeAudit) Read() ([]byte, error) { return nil, privdir.ErrUnsupported }

// Append refuses.
func (MergeAudit) Append(func([]byte) ([]byte, error)) error { return privdir.ErrUnsupported }
