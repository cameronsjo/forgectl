package gitenv

import (
	"errors"
	"slices"
	"testing"
)

// A listing splits into filter drivers and configured hooks; a hook name is
// parsed from the right, so it may hold dots, and one -c cannot switch off
// is refused.
// Mutations: take the hook name from the left (x.y.command names "x");
// drop the '=' check (a=b is switched off as "a", leaving it live); accept
// any hook variable (hook.x.enabled passes).
func TestListedNames(t *testing.T) {
	filters, hooks, err := listedNames("filter.lfs.clean\x00hook.x.y.command\x00hook.x.y.event\x00hook.z.event\x00")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(filters, []string{"lfs"}) || !slices.Equal(hooks, []string{"x.y", "z"}) {
		t.Errorf("filters %q, hooks %q; want [lfs], [x.y z]", filters, hooks)
	}
	if f, h, err := listedNames(""); err != nil || len(f) != 0 || len(h) != 0 {
		t.Errorf("empty listing = %q, %q, %v; want none", f, h, err)
	}
	for bad, want := range map[string]error{
		"hook.a=b.command":  errUnoverridableHook,
		"hook.a\nb.event":   errUnoverridableHook,
		"hook.x.enabled":    errUnexpectedFilterKey,
		"hook.command":      errUnexpectedFilterKey,
		"core.hooksPath":    errUnexpectedFilterKey,
		"filter.x.required": errUnexpectedFilterKey,
	} {
		if _, _, err := listedNames(bad + "\x00"); !errors.Is(err, want) {
			t.Errorf("listedNames(%q) = %v, want %v", bad, err, want)
		}
	}
}

// NoHookArgs switches off the hooks directory first, then every event,
// then each name.
func TestNoHookArgs(t *testing.T) {
	got := NoHookArgs([]string{"x.y"})
	if !slices.Equal(got[:2], []string{"-c", "core.hooksPath=/dev/null"}) {
		t.Errorf("NoHookArgs starts %q, want the hooks-directory pin", got[:2])
	}
	for _, want := range []string{"hook.post-index-change.enabled=false", "hook.post-checkout.enabled=false", "hook.reference-transaction.enabled=false", "hook.x.y.enabled=false"} {
		if !slices.Contains(got, want) {
			t.Errorf("NoHookArgs lacks %s: %q", want, got)
		}
	}
	if got[len(got)-1] != "hook.x.y.enabled=false" {
		t.Errorf("NoHookArgs ends %q, want the named hook last", got[len(got)-1])
	}
}
