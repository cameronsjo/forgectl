// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package tui

import (
	"time"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The desk dashboard's public surface. The types live in this portable file so
// the non-unix stub exports the same names as the real implementation and a
// cross-platform build checks the whole surface.

// DeskSourceFailure is a source that could not be built, such as a run spec
// that failed to load. The dashboard shows a "source failed" chip and lists
// the cause in help; the other sources still load.
type DeskSourceFailure struct {
	Name string
	Err  error
}

// DeskOptions configures RunDesk.
//
// Sources are the run sources to list. A source that is a
// runview.SpecProvider supplies the spec its runs fold with. Desk runs always
// fold with runview.DeskSpec. A log run with no spec shows its events without
// step state. The first source backed by a desk is the one the queue verbs
// act on.
//
// Now is the clock; nil means time.Now. It also times the approve modal's
// delay. Restore, when set, puts the view back where a previous RunDesk left
// it.
type DeskOptions struct {
	Sources []runview.Source
	Failed  []DeskSourceFailure
	Theme   theme.Theme
	NoIcons bool
	Now     func() time.Time
	Restore *DeskViewState
}

// DeskViewState is what the dashboard remembers between two RunDesk calls: the
// selected run, the view and pane, the filter, and whether mouse capture is off.
type DeskViewState struct {
	Source, Run string
	View        int
	Focus       int
	Filter      string
	MouseOff    bool
}

// DeskAction is what the dashboard asks its caller to do after it exits. A nil
// DeskAction means nothing.
type DeskAction interface{ deskAction() }

// DeskRun asks the caller to run the claimed queue item Name, whose frozen
// content has the full hex SHA-256 SHA, in the foreground.
type DeskRun struct{ Name, SHA string }

func (DeskRun) deskAction() {}

// DeskResult is how RunDesk ended.
type DeskResult struct {
	Action DeskAction
	State  DeskViewState
}
