// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package runview

import (
	"fmt"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// NewSpecSource is unavailable off Unix: reading a run without following a
// symlink in any component needs openat with O_NOFOLLOW and O_DIRECTORY.
// There is no NewDeskSource here, because desk.Desk exists only on Unix.
func NewSpecSource(*Spec) (Source, error) {
	return nil, fmt.Errorf("run sources: %w", desk.ErrUnsupported)
}
