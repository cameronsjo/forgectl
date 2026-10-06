// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// deskItemExts are the extensions that follow an item's name on disk or in
// `desk status --json`: `desk add` strips .sh and .manifest to make the name
// (`x.sh` becomes `01-x`), and the item's log and events files end .log and
// .events. A caller that takes the basename of any of those paths holds
// `01-x.sh`, `01-x.log` or `01-x.events`.
var deskItemExts = []string{".sh", ".manifest", ".log", ".events"}

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

// resolveName maps the name a caller typed to the item's own name
// (forgectl#1087), and is the one place desk verbs do it. A name that matches
// an item (has reports it) stands as typed. One that does not, but matches once
// its extension is dropped, is a file name the caller took from a JSON path, so
// it resolves to the item. has is only called for a name that has an extension.
func resolveName(name string, has func(string) bool) string {
	stem := trimDeskExt(name)
	if stem == "" || has(name) || !has(stem) {
		return name
	}
	return stem
}

// resolveDeskName is resolveName over a scan of d. A scan failure leaves the
// name as typed; the verb's own scan reports it.
func resolveDeskName(d *desk.Desk, name string) string {
	return resolveName(name, func(n string) bool {
		snap, err := d.Scan()
		return err == nil && snapshotHasItem(snap, n)
	})
}

// deskNotFound builds a "no <what> named <name>" error that says why the name
// may be wrong: an item name carries no extension (a name that would resolve
// to an item already has, so there is no stem to suggest), and the known names
// are listed under label ("waiting", "runs") so the caller can pick one. known
// may be nil.
func deskNotFound(verb, what, name, label string, known []string) error {
	msg := fmt.Sprintf("%s: no %s named %s", verb, what, termsafe.SafeLineMax(name, deskQuoteMax))
	if trimDeskExt(name) != "" {
		msg += " (item names carry no extension)"
	}
	if len(known) > 0 {
		msg += "; " + label + ": " + strings.Join(known, ", ")
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
	var waiting []desk.Item
	for _, it := range snap.Pending {
		if it.State == desk.StateWaiting {
			waiting = append(waiting, it)
		}
	}
	return cappedNames(waiting)
}

// runNames lists the names of the items that have a run: running, then done.
func runNames(d *desk.Desk) []string {
	snap, err := d.Scan()
	if err != nil {
		return nil
	}
	return cappedNames(append(append([]desk.Item{}, snap.Running...), snap.Done...))
}

// cappedNames is the names of items, at most deskNotFoundListMax of them.
func cappedNames(items []desk.Item) []string {
	var names []string
	for _, it := range items {
		if len(names) == deskNotFoundListMax {
			names = append(names, "...")
			break
		}
		names = append(names, safeText(it.Name))
	}
	return names
}
