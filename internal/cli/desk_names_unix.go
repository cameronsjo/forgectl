// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// deskItemExts are the file extensions `desk add` strips to make an item's
// name: `x.sh` becomes `01-x`. The JSON `path` of a queued item still ends in
// the extension, so a caller that takes its basename holds `01-x.sh`.
var deskItemExts = []string{".sh", ".manifest"}

// trimDeskExt returns name without a trailing item extension, or "" when it
// has none.
func trimDeskExt(name string) string {
	for _, ext := range deskItemExts {
		if stem, ok := strings.CutSuffix(name, ext); ok && stem != "" {
			return stem
		}
	}
	return ""
}

// snapshotHasItem reports whether any state of snap holds an item called name.
func snapshotHasItem(snap *desk.Snapshot, name string) bool {
	_, ok := findItem(snap, name)
	return ok
}

// resolveDeskName maps the name a caller typed to the item's own name
// (forgectl#1087). A name that matches an item stands as typed. One that does
// not, but matches once its `.sh` or `.manifest` is dropped, is the file name
// of an item the caller took from a JSON `path`, so it resolves to the item.
// A scan failure leaves the name as typed; the verb's own scan reports it.
func resolveDeskName(d *desk.Desk, name string) string {
	stem := trimDeskExt(name)
	if stem == "" {
		return name
	}
	snap, err := d.Scan()
	if err != nil || snapshotHasItem(snap, name) || !snapshotHasItem(snap, stem) {
		return name
	}
	return stem
}

// deskNotFound builds a "no <what> named <name>" error that says why the name
// may be wrong: an item name carries no extension, and the waiting items are
// listed so the caller can pick one. waiting may be nil.
func deskNotFound(verb, what, name string, waiting []string) error {
	msg := fmt.Sprintf("%s: no %s named %s", verb, what, termsafe.SafeLineMax(name, deskQuoteMax))
	if stem := trimDeskExt(name); stem != "" {
		msg += fmt.Sprintf(" (item names carry no extension; try %s)", termsafe.SafeLineMax(stem, deskQuoteMax))
	}
	if len(waiting) > 0 {
		msg += "; waiting: " + strings.Join(waiting, ", ")
	}
	return fmt.Errorf("%s", msg)
}

// deskNotFoundListMax caps the waiting names a not-found error lists.
const deskNotFoundListMax = 10

// waitingNames lists the names of the items waiting to run. A scan failure
// yields none: this only decorates an error that is already being reported.
func waitingNames(d *desk.Desk) []string {
	snap, err := d.Scan()
	if err != nil {
		return nil
	}
	var names []string
	for _, it := range snap.Pending {
		if it.State != desk.StateWaiting {
			continue
		}
		if len(names) == deskNotFoundListMax {
			names = append(names, "...")
			break
		}
		names = append(names, safeText(it.Name))
	}
	return names
}
