package pr

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// escapedSep is how tmux 3.5a and older render tmux.FieldSep back to a
// non-attached client — the four printable characters \, 0, 3, 7. Spelled out
// here rather than imported so this test fails if internal/tmux ever stops
// accepting the rendering the CI runner's tmux actually emits.
const escapedSep = `\037`

// TestNewDispatchReadsBothTmuxRenderings is the internal/pr half of the Linux CI
// regression. `new-window -P -F IdentityFormat` returns the identity joined by
// whichever rendering the local tmux uses; before this, a bare split on the raw
// byte saw one field on tmux 3.4, newDispatch errored, launchInline returned no
// Dispatch, and verifyReviewDispatches silently reported Skipped — a review
// dispatched with no liveness check behind it.
// renderings runs fn once per rendering tmux uses for FieldSep, so a table
// wrapped in it is pinned under BOTH rather than under whichever one the case
// happened to be written with.
func renderings(t *testing.T, fn func(t *testing.T, sep string)) {
	t.Helper()
	for _, r := range []struct{ name, sep string }{
		{"raw (tmux 3.7b)", tmux.FieldSep},
		{"octal-escaped (tmux <=3.5a)", escapedSep},
	} {
		t.Run(r.name, func(t *testing.T) { fn(t, r.sep) })
	}
}

func TestNewDispatchReadsBothTmuxRenderings(t *testing.T) {
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	want := strings.Join([]string{"4677", "1786644304", "@1"}, tmux.FieldSep)
	renderings(t, func(t *testing.T, sep string) {
		output := strings.Join([]string{"4677", "1786644304", "@1"}, sep)
		got, err := newDispatch(ref, output)
		if err != nil {
			t.Fatalf("newDispatch(%q): %v", output, err)
		}
		// Normalized onto the raw separator either way, so the WindowID stays
		// comparable to the key VerifyDispatched rebuilds from a ListWindows row.
		if got.WindowID != want {
			t.Errorf("WindowID = %q, want %q", got.WindowID, want)
		}
		if got.Ref != ref {
			t.Errorf("Ref = %+v, want %+v", got.Ref, ref)
		}
	})
}

// TestNewDispatchStillRejectsUnusableIdentities pins that widening the SPLIT did
// not widen what counts as an identity: every field is still server-supplied and
// strictly validated, which is what makes a dispatch unforgeable from anything a
// window name can carry.
func TestNewDispatchStillRejectsUnusableIdentities(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	// Fields, not pre-joined strings: the cases were previously spread ACROSS
	// the two renderings (three escaped-only, two raw-only), so each rejection
	// was pinned under exactly one of them despite the comment above claiming
	// both. Joining inside the renderings helper runs all seven under each.
	for _, tc := range []struct {
		name   string
		fields []string
	}{
		{"no separator at all", []string{"4677"}},
		{"too many fields", []string{"1", "2", "@3", "4"}},
		{"zero pid", []string{"0", "2", "@3"}},
		{"non-numeric pid", []string{"pid", "2", "@3"}},
		{"non-numeric start time", []string{"1", "when", "@3"}},
		{"window id without @", []string{"1", "2", "3"}},
		{"window id not numeric", []string{"1", "2", "@x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renderings(t, func(t *testing.T, sep string) {
				output := strings.Join(tc.fields, sep)
				if got, err := newDispatch(ref, output); err == nil {
					t.Fatalf("newDispatch(%q) = %+v, want an error", output, got)
				}
			})
		})
	}
}
