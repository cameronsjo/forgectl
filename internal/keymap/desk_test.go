// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package keymap

import (
	"slices"
	"testing"

	"charm.land/bubbles/v2/key"
)

// TestDeskKeysAreUniqueAndDocumented pins the two properties the footer and
// the help screen rely on: no key is bound twice, and every binding has the
// help text that lists it.
func TestDeskKeysAreUniqueAndDocumented(t *testing.T) {
	seen := map[string]int{}
	for i, b := range NewDesk().All() {
		h := b.Help()
		if h.Key == "" || h.Desc == "" {
			t.Errorf("binding %d (%v) has no help text: %+v", i, b.Keys(), h)
		}
		if !b.Enabled() || len(b.Keys()) == 0 {
			t.Errorf("binding %d (%q) is disabled or has no keys", i, h.Key)
		}
		for _, k := range b.Keys() {
			if prev, dup := seen[k]; dup {
				t.Errorf("key %q is bound by binding %d and by binding %d (%q)", k, prev, i, h.Key)
			}
			seen[k] = i
		}
	}
	for _, want := range []string{"[", "]", "<", ">", "o", "1", "6", "tab", "up", "down", "j", "k", "g", "G", "enter", "p", "left", "right", "space", "a", "s", "u", "/", "r", "R", "m", "?", "esc", "q", "ctrl+c"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("key %q from the key table is not bound", want)
		}
	}
}

// TestDeskHelpListsOnlyBoundKeys checks each mode's footer and the full help
// draw from the bindings, and the footer differs by mode.
func TestDeskHelpListsOnlyBoundKeys(t *testing.T) {
	keys := NewDesk()
	all := keys.All()
	modes := []DeskMode{DeskBrowse, DeskQueue, DeskFilter, DeskHelp}
	short := map[DeskMode][]string{}
	for _, mode := range modes {
		h := DeskHelpMap{Keys: keys, Mode: mode}
		bindings := h.ShortHelp()
		if len(bindings) == 0 {
			t.Errorf("mode %d has an empty footer", mode)
		}
		for _, b := range bindings {
			if b.Help().Key == "" {
				t.Errorf("mode %d lists a binding with no help", mode)
			}
			if mode != DeskFilter && !slices.ContainsFunc(all, func(a key.Binding) bool { return a.Help() == b.Help() }) {
				t.Errorf("mode %d lists %+v, which is not in the key table", mode, b.Help())
			}
			short[mode] = append(short[mode], b.Help().Key)
		}
	}
	if slices.Equal(short[DeskBrowse], short[DeskQueue]) {
		t.Error("the queue footer is the same as the browse footer; it should list the approval keys")
	}
	if !slices.Contains(short[DeskQueue], "a") {
		t.Errorf("the queue footer %v lacks the approve key", short[DeskQueue])
	}
	if slices.Contains(short[DeskBrowse], "a") {
		t.Errorf("the browse footer %v offers approve outside the queue", short[DeskBrowse])
	}
	var full int
	for _, grp := range (DeskHelpMap{Keys: keys}).FullHelp() {
		full += len(grp)
	}
	if full != len(all) {
		t.Errorf("full help lists %d bindings, the table has %d", full, len(all))
	}
}
