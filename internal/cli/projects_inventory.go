// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

const (
	inventoryStatus = "Querying local, GitHub, and Gitea…"
	// inventoryStatusSlow must fit a 40-column terminal in one row: the erase
	// clears one row, so a wrapped line would leave its first row above the
	// picker.
	inventoryStatusSlow = "Still querying… (Ctrl+C cancels)"

	// slowInventoryAfter is when the status line says the wait is long. A host
	// that does not answer holds the whole inventory for as long as it takes,
	// so a line that never changes reads as a hang.
	slowInventoryAfter = 3 * time.Second
)

// withStatus runs work, showing a one-line status on w while it runs: at once,
// so there is a visible response inside 100 ms, then a longer line if work
// outlasts slowAfter. The line is erased when work returns or ctx ends, so it
// never lingers above the picker or after a Ctrl+C. show false (no terminal)
// writes nothing.
//
// A one-line status rather than a spinner: no second Bubble Tea program starts
// before the picker's own, so there is no extra terminal query to stall on.
//
// work runs on its own goroutine so a cancelled ctx returns at once, without
// waiting for a host that is still sleeping on the other end.
func withStatus(ctx context.Context, w io.Writer, show bool, slowAfter time.Duration, work func() error) error {
	if !show {
		return work()
	}
	var (
		mu       sync.Mutex
		finished bool
	)
	width := writerWidth(w)
	write := func(line string) {
		if width > 0 {
			line = truncate(line, width-1)
		}
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_, _ = fmt.Fprint(w, "\r"+ansi.EraseEntireLine+line)
		}
	}
	write(inventoryStatus)
	slow := time.AfterFunc(slowAfter, func() { write(inventoryStatusSlow) })

	done := make(chan error, 1)
	go func() { done <- work() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	slow.Stop()

	mu.Lock()
	finished = true
	_, _ = fmt.Fprint(w, "\r"+ansi.EraseEntireLine)
	mu.Unlock()
	return err
}

// heldNotes holds the host-degradation notes of an interactive run until the
// picker can show them. Printed at once they land on the terminal around the
// form and push it down (#1104). Methods are safe on a nil receiver, so a
// caller with nothing held passes nil.
type heldNotes struct {
	cmd   *cobra.Command
	notes []string
}

// take hands the held notes to the picker and forgets them.
func (h *heldNotes) take() []string {
	if h == nil {
		return nil
	}
	n := h.notes
	h.notes = nil
	return n
}

// flush prints whatever is still held, for the paths that never reach the
// picker: a unique match, an empty inventory, an error.
func (h *heldNotes) flush() {
	if h == nil || len(h.notes) == 0 {
		return
	}
	renderDegradationNotes(h.cmd, h.notes)
	h.notes = nil
}

// before prints what is still held, then runs act. Every verb that goes on to
// open, clone, or add a worktree does it through here: those paths may replace
// the process, so a deferred flush would never run and the notes would vanish.
func (h *heldNotes) before(act func() error) error {
	h.flush()
	return act()
}

// loadInventory queries the inventory with a status line, and holds the notes
// when a picker may follow (an interactive terminal) or prints them at once
// when none can.
func loadInventory(cmd *cobra.Command, client *projects.Client) ([]projects.Repo, *heldNotes, error) {
	// The status line and the held notes are for a person at a terminal. With
	// stderr redirected (2>log, | tee) neither belongs there: the log would
	// collect erase sequences and the notes would never reach it.
	stderr := cmd.ErrOrStderr()
	interactive := isInteractiveTTY() && writerWidth(stderr) > 0

	ctx := cmd.Context()
	if interactive {
		// Ctrl+C during the wait cancels the query and erases the line; the
		// terminal is not in raw mode yet, so SIGINT is what arrives.
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}
	var (
		all   []projects.Repo
		notes []string
	)
	err := withStatus(ctx, stderr, interactive, slowInventoryAfter, func() error {
		var err error
		all, notes, err = inventoryFn(ctx, client)
		return err
	})
	held := &heldNotes{cmd: cmd}
	if err != nil {
		if interactive && errors.Is(err, context.Canceled) && cmd.Context().Err() == nil {
			return nil, held, huh.ErrUserAborted
		}
		return nil, held, err
	}
	if interactive {
		held.notes = notes
	} else {
		renderDegradationNotes(cmd, notes)
	}
	return all, held, nil
}

// inventoryFn is the inventory query, a seam so a test can supply repos and
// notes without a runner.
var inventoryFn = func(ctx context.Context, c *projects.Client) ([]projects.Repo, []string, error) {
	return c.Inventory(ctx)
}
