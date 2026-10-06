// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package runview

import "errors"

// NewLogSource needs openat and O_NOFOLLOW; off Unix there is no log source.
// The desk itself is Unix-only, so there is no desk source here either.
func NewLogSource(string, LogKeys) (Source, error) {
	return nil, errors.New("runview: reading a log needs a Unix system")
}
