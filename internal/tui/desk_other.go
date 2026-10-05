// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package tui

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// RunDesk is not available on this platform: the desk needs Unix primitives.
func RunDesk(_ context.Context, _ DeskOptions) (DeskResult, error) {
	return DeskResult{}, desk.ErrUnsupported
}
