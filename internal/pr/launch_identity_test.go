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
func TestNewDispatchReadsBothTmuxRenderings(t *testing.T) {
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	want := strings.Join([]string{"4677", "1786644304", "@1"}, tmux.FieldSep)
	for _, tc := range []struct {
		name   string
		output string
	}{
		{"raw (tmux 3.7b)", want},
		{"octal-escaped (tmux <=3.5a)", strings.Join([]string{"4677", "1786644304", "@1"}, escapedSep)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newDispatch(ref, tc.output)
			if err != nil {
				t.Fatalf("newDispatch(%q): %v", tc.output, err)
			}
			// Normalized onto the raw separator either way, so the WindowID
			// stays comparable to the key VerifyDispatched rebuilds from a
			// ListWindows row.
			if got.WindowID != want {
				t.Errorf("WindowID = %q, want %q", got.WindowID, want)
			}
			if got.Ref != ref {
				t.Errorf("Ref = %+v, want %+v", got.Ref, ref)
			}
		})
	}
}

// TestNewDispatchStillRejectsUnusableIdentities pins that widening the SPLIT did
// not widen what counts as an identity: every field is still server-supplied and
// strictly validated, which is what makes a dispatch unforgeable from anything a
// window name can carry.
func TestNewDispatchStillRejectsUnusableIdentities(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	for _, tc := range []struct {
		name   string
		output string
	}{
		{"no separator at all", "4677"},
		{"too many fields", strings.Join([]string{"1", "2", "@3", "4"}, tmux.FieldSep)},
		{"zero pid", strings.Join([]string{"0", "2", "@3"}, tmux.FieldSep)},
		{"non-numeric pid", strings.Join([]string{"pid", "2", "@3"}, escapedSep)},
		{"non-numeric start time", strings.Join([]string{"1", "when", "@3"}, escapedSep)},
		{"window id without @", strings.Join([]string{"1", "2", "3"}, escapedSep)},
		{"window id not numeric", strings.Join([]string{"1", "2", "@x"}, tmux.FieldSep)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := newDispatch(ref, tc.output); err == nil {
				t.Fatalf("newDispatch(%q) = %+v, want an error", tc.output, got)
			}
		})
	}
}
