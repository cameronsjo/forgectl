// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

const (
	inventoryStatus     = "Querying local, GitHub, and Gitea…"
	inventoryStatusSlow = "Still querying local, GitHub, and Gitea… (Ctrl+C cancels)"

	// slowInventoryAfter is when the status line says the wait is long. A host
	// that does not answer holds the whole inventory for as long as it takes,
	// so a line that never changes reads as a hang.
	slowInventoryAfter = 3 * time.Second
)

// withStatus runs work, showing a one-line status on w while it runs: at once,
// so there is a visible response inside 100 ms, then a longer line if work
// outlasts slowAfter. The line is erased when work returns, so it never
// lingers above the picker. show false (no terminal) writes nothing.
//
// A one-line status rather than a spinner: no second Bubble Tea program starts
// before the picker's own, so there is no extra terminal query to stall on.
func withStatus(w io.Writer, show bool, slowAfter time.Duration, work func() error) error {
	if !show {
		return work()
	}
	var (
		mu       sync.Mutex
		finished bool
	)
	write := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_, _ = fmt.Fprint(w, "\r"+ansi.EraseEntireLine+line)
		}
	}
	write(inventoryStatus)
	slow := time.AfterFunc(slowAfter, func() { write(inventoryStatusSlow) })
	err := work()
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

// loadInventory queries the inventory with a status line, and holds the notes
// when a picker may follow (an interactive terminal) or prints them at once
// when none can.
func loadInventory(cmd *cobra.Command, client *projects.Client) ([]projects.Repo, *heldNotes, error) {
	interactive := isInteractiveTTY()
	var (
		all   []projects.Repo
		notes []string
	)
	err := withStatus(cmd.ErrOrStderr(), interactive, slowInventoryAfter, func() error {
		var err error
		all, notes, err = client.Inventory(cmd.Context())
		return err
	})
	held := &heldNotes{cmd: cmd}
	if err != nil {
		return nil, held, err
	}
	if interactive {
		held.notes = notes
	} else {
		renderDegradationNotes(cmd, notes)
	}
	return all, held, nil
}
